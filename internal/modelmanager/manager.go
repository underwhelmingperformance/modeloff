// Package modelmanager owns the LLM-side state that the session
// router has no business carrying: the OpenRouter [api.Client] and
// its rebuild factory, the persona pool, the small-model id used
// for nick generation and persona seeding, the cached supported-
// models catalogue, and the per-instance [modelclient.ModelClient]
// registry that implements [session.ModelClientFactory].
//
// The manager owns both the data (api key, factory, catalogue,
// personas) and the lifecycle (per-instance client construction and
// detach). A [Manager] consumer reads the api client through a
// getter so each model-dispatch turn picks up the latest handle
// after a `SetAPIKey` rebuild; the registry's [modelclient.New]
// call wires the getter into every attached client.
package modelmanager

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
)

// Store is the persistence surface the manager depends on. The
// concrete `*store.SQLiteStore` satisfies it implicitly. Persona
// arbitration writes through the store; the per-instance client
// registry reads the boot-time instance list to attach existing
// model clients.
type Store interface {
	ListInstances(ctx context.Context) ([]*domain.Instance, error)

	ListPersonas(ctx context.Context) ([]domain.Persona, error)
	SavePersona(ctx context.Context, p domain.Persona) error
	DeletePersonasByOrigin(ctx context.Context, origin domain.PersonaOrigin) error
	ReplaceGeneratedPersonas(ctx context.Context, personas []domain.Persona) error
}

// Config is the construction-time configuration for a [Manager].
type Config struct {
	Store         Store
	Memory        memory.Store
	APIClient     api.Client
	APIFactory    func(apiKey, baseURL string) (api.Client, error)
	InitialAPIKey string
	SmallModel    domain.ModelID
	Tools         *modelclient.ToolRegistry
	BaseContext   func() context.Context

	// Now overrides the manager's clock. Defaults to [time.Now].
	Now func() time.Time

	// TracerProvider overrides the OTel tracer provider the
	// manager records spans on. Defaults to the global provider.
	TracerProvider trace.TracerProvider

	// Pacer is the typing-delay [modelclient.Pacer] threaded into
	// every attached model-client. Nil selects a default Pacer
	// tuned for natural-feeling bot replies; explicit zero-valued
	// pacers disable pacing.
	Pacer *modelclient.Pacer
}

// defaultPacer returns the production typing-delay tuning. Floor
// stops one-liners feeling instant; CPS gives longer replies a
// proportional pause; jitter staggers concurrent bot dispatches.
func defaultPacer() *modelclient.Pacer {
	return &modelclient.Pacer{
		Floor:  250 * time.Millisecond,
		CPS:    40,
		Jitter: 200 * time.Millisecond,
		Rng:    modelclient.NewRandRandomiser(),
	}
}

// Manager is the LLM-side coordinator. It owns the OpenRouter
// [api.Client], the rebuild factory, the persona pool, the small-
// model id, the catalogue cache, and the per-instance
// [modelclient.ModelClient] registry. It satisfies
// [session.ModelClientFactory] via [Manager.Attach] and
// [Manager.Detach] so a single value passes to `session.New`.
type Manager struct {
	store       Store
	memory      memory.Store
	tools       *modelclient.ToolRegistry
	baseContext func() context.Context
	now         func() time.Time
	tracer      trace.TracerProvider
	pacer       *modelclient.Pacer

	mu         sync.RWMutex
	api        api.Client
	apiKey     string
	smallModel domain.ModelID
	factory    func(apiKey, baseURL string) (api.Client, error)

	cacheMu              sync.Mutex
	supportedModels      map[domain.ModelID]api.ModelInfo
	supportedModelsReady bool
	// catalogueLoadDone is non-nil while a catalogue fetch is in
	// flight, and closes when it finishes. A caller that finds one
	// already set waits on it and starts no upstream call of its
	// own, so concurrent add-model demands single-flight into one
	// ListModels round trip.
	catalogueLoadDone chan struct{}
	state             atomic.Uint32
	// stateFailedAt holds the UnixNano time of the most recent
	// transition into ListStateFailed, so ensureCatalogueLoaded can
	// tell whether CatalogueRetryBackoff has elapsed. Zero means the
	// catalogue has never failed.
	stateFailedAt atomic.Int64

	// clients holds the attached model-clients; draining holds the
	// ones already released, whose dispatch goroutines are on their
	// way out. Both are guarded by `clientsMu`.
	//
	// A released client leaves `clients` the moment it is released,
	// because its identity is free again from that point. It moves to
	// `draining` so the join still has something to join: a turn can
	// be mid-flight for as long as its upstream call takes, and
	// [Manager.DetachAll] is the only thing that waits for it. A
	// client dropped at release would take its goroutine out of every
	// join's reach.
	clientsMu sync.Mutex
	clients   map[protocol.ClientID]*modelclient.ModelClient
	draining  []*modelclient.ModelClient
}

// New constructs a [Manager] from cfg. The returned value is ready
// to be passed as the `factory` argument to `session.New`; call
// [Manager.Start] once the session is built to attach any stored
// model instances.
func New(cfg Config) *Manager {
	smallModel := cfg.SmallModel
	if smallModel == "" {
		smallModel = config.DefaultSmallModel
	}

	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	tracer := cfg.TracerProvider
	if tracer == nil {
		tracer = otel.GetTracerProvider()
	}

	pacer := cfg.Pacer
	if pacer == nil {
		pacer = defaultPacer()
	}

	return &Manager{
		store:       cfg.Store,
		memory:      cfg.Memory,
		tools:       cfg.Tools,
		baseContext: cfg.BaseContext,
		now:         now,
		tracer:      tracer,
		pacer:       pacer,
		api:         cfg.APIClient,
		apiKey:      strings.TrimSpace(cfg.InitialAPIKey),
		smallModel:  smallModel,
		factory:     cfg.APIFactory,
		clients:     make(map[protocol.ClientID]*modelclient.ModelClient),
	}
}

// WithTracerProvider returns m with its tracer provider replaced
// by tp. Mirrors `*session.Session.WithTracerProvider` for tests
// that need per-test span recording.
func (m *Manager) WithTracerProvider(tp trace.TracerProvider) *Manager {
	m.tracer = tp
	return m
}

// SetAPIFactory configures the runtime API-client factory used by
// [Manager.SetAPIKey] and [Manager.SetBaseURL].
func (m *Manager) SetAPIFactory(factory func(apiKey, baseURL string) (api.Client, error)) {
	m.mu.Lock()
	m.factory = factory
	m.mu.Unlock()
}

// HasAPIKey reports whether an API key is configured.
func (m *Manager) HasAPIKey() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.apiKey != ""
}

// EmbeddingSearchable reports whether the memory store's embedding
// endpoint is currently reachable, and the error from its most
// recent probe. memory.NewDefaultStore re-probes on every APIKey,
// BaseURL or EmbeddingModel change, so this reads that already-
// refreshed state without triggering a new probe of its own. A
// memory store with no embedding endpoint to probe (the plain,
// non-indexed fallback) reports ok=false with a nil error.
func (m *Manager) EmbeddingSearchable() (bool, error) {
	prober, ok := m.memory.(memory.EmbeddingProber)
	if !ok {
		return false, nil
	}

	return prober.Searchable(), prober.ProbeError()
}

// APIClient returns the current API client. The handle may change
// over the manager's lifetime as `SetAPIKey` / `SetBaseURL` rebuild
// it; callers that hold a value risk talking to a stale handle.
// [Manager.APIClientGetter] is the long-lived shape consumers
// should hold instead.
func (m *Manager) APIClient() api.Client {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.api
}

// APIClientGetter returns a closure that re-reads the manager's
// current API client on each call. Each [modelclient.ModelClient]
// receives the getter so a turn dispatched after a rebuild picks
// up the fresh handle without needing reattach.
func (m *Manager) APIClientGetter() func() api.Client {
	return m.APIClient
}

// SetAPIKey records a new API key and, if a factory is configured,
// rebuilds the API client. The supported-models cache is
// invalidated so the next add-model lazy-loads against the new
// upstream.
func (m *Manager) SetAPIKey(ctx context.Context, apiKey, baseURL string) error {
	return m.inSpan(ctx, "modelmanager.set_api_key", nil, func(ctx context.Context, _ trace.Span) error {
		apiKey = strings.TrimSpace(apiKey)

		m.mu.Lock()
		nextClient := m.api
		if apiKey != "" && m.factory != nil {
			client, err := m.factory(apiKey, baseURL)
			if err != nil {
				m.mu.Unlock()
				return observability.ErrWithKind(fmt.Errorf("build api client: %w", err), observability.ErrorKindValidation)
			}
			nextClient = client
		}
		if apiKey == "" {
			nextClient = nil
		}

		m.api = nextClient
		m.apiKey = apiKey
		m.mu.Unlock()

		m.invalidateCatalogue(ctx)

		return nil
	})
}

// SetBaseURL rebuilds the API client with the given base URL if a
// factory and an API key are configured. A rebuild invalidates the
// supported-models cache, the same way SetAPIKey does: the cache
// describes the provider behind the old base URL, so the next
// add-model lazy-loads a fresh catalogue for the new one.
func (m *Manager) SetBaseURL(ctx context.Context, baseURL string) error {
	return m.inSpan(ctx, "modelmanager.set_base_url", nil, func(ctx context.Context, _ trace.Span) error {
		baseURL = strings.TrimSpace(baseURL)

		m.mu.Lock()
		rebuilt := false
		if m.factory != nil && m.apiKey != "" {
			client, err := m.factory(m.apiKey, baseURL)
			if err != nil {
				m.mu.Unlock()
				return observability.ErrWithKind(fmt.Errorf("build api client: %w", err), observability.ErrorKindValidation)
			}
			m.api = client
			rebuilt = true
		}
		m.mu.Unlock()

		if rebuilt {
			m.invalidateCatalogue(ctx)
		}

		return nil
	})
}

// SetSmallModel updates the model id the manager uses for nick
// generation and persona seeding.
func (m *Manager) SetSmallModel(ctx context.Context, modelID domain.ModelID) {
	_ = m.inSpan(ctx, "modelmanager.set_small_model", []attribute.KeyValue{
		attribute.String(observability.AttrModelID, string(modelID)),
	}, func(_ context.Context, _ trace.Span) error {
		m.mu.Lock()
		m.smallModel = modelID
		m.mu.Unlock()

		return nil
	})
}

// SmallModel returns the configured small-model id.
func (m *Manager) SmallModel() domain.ModelID {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.smallModel
}

// Now returns the manager's current time, using the configured
// clock.
func (m *Manager) Now() time.Time {
	return m.now()
}

// SetClock replaces the manager's clock. Tests use this to freeze
// time around persona / nick assertions.
func (m *Manager) SetClock(clock func() time.Time) {
	if clock == nil {
		clock = time.Now
	}

	m.now = clock
}

// snapshotAPI atomically reads the current API client + key pair
// under the manager's read lock.
func (m *Manager) snapshotAPI() (api.Client, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.api, m.apiKey
}

// maxNickGenerationAttempts caps the number of times the small model
// is asked for a nickname before [Manager.generateNickFromModel]
// gives up. Each retry carries the previously rejected suggestion as
// a follow-up turn so the model picks something different. The
// user's full nick list is intentionally never sent to the model.
const maxNickGenerationAttempts = 3

// nickMaxLen matches the nickname schema's `maxLength` constraint
// declared alongside `nicknameResponse` in the api package.
const nickMaxLen = 12

// deterministicNickBaseLen caps the length of the model-id-derived
// base [deterministicNickBase] produces, leaving room within
// [nickMaxLen] for a numeric collision suffix.
const deterministicNickBaseLen = 8

// maxDeterministicNickAttempts caps how many numbered variants
// [Manager.fallbackNick] tries before giving up.
const maxDeterministicNickAttempts = 1000

// generateUniqueNick resolves a nick for a new model instance. It
// asks the small model for one, guided by the assigned persona, and
// falls back to a nick derived deterministically from modelID
// whenever the small model cannot supply a usable one: an upstream
// failure, no API client configured, or every suggestion colliding
// with a taken nick. Nick generation therefore never blocks an
// add-model on its own.
func (m *Manager) generateUniqueNick(
	ctx context.Context,
	sess *session.Session,
	modelID domain.ModelID,
	persona string,
	logger *slog.Logger,
) (domain.Nick, error) {
	nick, err := m.generateNickFromModel(ctx, sess, modelID, persona, logger)
	if err == nil {
		return nick, nil
	}

	logger.WarnContext(ctx, "nick generation unavailable, falling back to a deterministic nick",
		"error", err,
	)

	fallback, fallbackErr := m.fallbackNick(ctx, sess, modelID)
	if fallbackErr != nil {
		return "", fmt.Errorf("generate nick: %w; deterministic fallback also failed: %w", err, fallbackErr)
	}

	return fallback, nil
}

// generateNickFromModel asks the small model for a nickname guided
// by the assigned persona and retries up to
// [maxNickGenerationAttempts] times if the suggested nick is already
// taken.
func (m *Manager) generateNickFromModel(
	ctx context.Context,
	sess *session.Session,
	modelID domain.ModelID,
	persona string,
	logger *slog.Logger,
) (domain.Nick, error) {
	var nick domain.Nick

	err := m.inSpan(ctx, "modelmanager.generate_nick", []attribute.KeyValue{
		attribute.String(observability.AttrModelID, string(modelID)),
	}, func(generateCtx context.Context, generateSpan trace.Span) error {
		client, _ := m.snapshotAPI()
		if client == nil {
			return observability.ErrWithKind(fmt.Errorf("generate nick: api client not configured"), observability.ErrorKindValidation)
		}

		small := m.SmallModel()

		var rejected []domain.Nick

		for attempt := 1; attempt <= maxNickGenerationAttempts; attempt++ {
			result, err := client.GenerateNick(generateCtx, small, persona, rejected)
			if err != nil {
				logger.ErrorContext(ctx, "generate nick failed",
					"error", err,
					"attempt", attempt,
				)
				return observability.ErrWithKind(fmt.Errorf("generate nick: %w", err), observability.ErrorKindDispatch)
			}

			result.Usage.SetSpanAttributes(generateSpan, result.RequestID)

			if !m.nickIsTaken(ctx, sess, result.Nick) {
				nick = result.Nick
				return nil
			}

			logger.InfoContext(ctx, "generated nick already in use",
				"nick", result.Nick,
				"attempt", attempt,
			)
			rejected = append(rejected, result.Nick)
		}

		return observability.ErrWithKind(
			fmt.Errorf("generate nick: %d attempts exhausted, all suggestions collided", maxNickGenerationAttempts),
			observability.ErrorKindDispatch,
		)
	})

	return nick, err
}

// deterministicNickBase derives a nick-safe base string from a model
// id: the segment after the last `/`, lowercased, with every
// character outside the nick charset (`[a-z0-9_-]`) replaced by `-`,
// trimmed of leading and trailing `-`, and capped at
// [deterministicNickBaseLen] so a numeric collision suffix still fits
// within the 12-character nick limit.
func deterministicNickBase(modelID domain.ModelID) string {
	id := string(modelID)
	if idx := strings.LastIndexByte(id, '/'); idx >= 0 {
		id = id[idx+1:]
	}

	var b strings.Builder
	for _, r := range strings.ToLower(id) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}

	base := strings.Trim(b.String(), "-")
	if len(base) > deterministicNickBaseLen {
		base = base[:deterministicNickBaseLen]
	}
	base = strings.Trim(base, "-")

	if base == "" {
		base = "model"
	}

	return base
}

// fallbackNick derives a nick deterministically from modelID for use
// when nick generation cannot supply one. It tries the bare
// [deterministicNickBase] first, then numbered variants ("nick2",
// "nick3", ...) up to [maxDeterministicNickAttempts], checking each
// against sess for uniqueness the same way generateNickFromModel
// does.
func (m *Manager) fallbackNick(ctx context.Context, sess *session.Session, modelID domain.ModelID) (domain.Nick, error) {
	base := deterministicNickBase(modelID)

	for i := range maxDeterministicNickAttempts {
		candidate := base
		if i > 0 {
			suffix := strconv.Itoa(i + 1)
			keep := max(nickMaxLen-len(suffix), 1)
			if len(candidate) > keep {
				candidate = candidate[:keep]
			}
			candidate += suffix
		}

		nick := domain.Nick(candidate)
		if !m.nickIsTaken(ctx, sess, nick) {
			return nick, nil
		}
	}

	return "", fmt.Errorf("%d deterministic candidates exhausted for %q", maxDeterministicNickAttempts, modelID)
}

// nickIsTaken reports whether `nick` is already held by the user
// or any registered model instance. Resolution flows through
// `Session.ResolveNick`, which gives the same answer the
// dispatcher's nick resolver would.
func (m *Manager) nickIsTaken(ctx context.Context, sess *session.Session, nick domain.Nick) bool {
	_, err := sess.ResolveNick(ctx, nick)
	return err == nil
}

// PrepareInstance resolves the persona and unique nick for a new
// model instance. The session's `AddModel` handler calls this
// before attaching the constructed instance to a channel. Nick
// failures and tool-capability validation fail the command; a
// persona the pool could not supply does not, since a model with no
// persona still works, so it comes back as a warning the handler
// answers with a server notice. The supplied session is consulted
// for nick-uniqueness resolution so the manager does not hold a
// back-reference.
func (m *Manager) PrepareInstance(
	ctx context.Context,
	sess *session.Session,
	modelID domain.ModelID,
	persona string,
) (session.PreparedInstance, error) {
	logger := slog.Default().With("component", "modelmanager", "model_id", modelID)

	if err := m.EnsureToolCapableModel(ctx, modelID); err != nil {
		return session.PreparedInstance{}, err
	}

	prepared := session.PreparedInstance{Persona: strings.TrimSpace(persona)}

	if prepared.Persona == "" {
		// A pool that could not be topped up is only a problem if it
		// is also empty, which the draw below is what discovers.
		if err := m.EnsurePersonas(ctx); err != nil {
			logger.WarnContext(ctx, "persona pool generation failed", "error", err)
		}

		p, err := m.RandomPersona(ctx)
		if err != nil {
			logger.WarnContext(ctx, "persona assignment failed, instance will have no persona", "error", err)

			prepared.Warnings = append(prepared.Warnings,
				fmt.Sprintf("no persona was assigned to %s (%v); it joins without one", modelID, err))
		} else {
			prepared.Persona = p.Description
		}
	}

	nick, err := m.generateUniqueNick(ctx, sess, modelID, prepared.Persona, logger)
	if err != nil {
		return prepared, err
	}

	prepared.Nick = nick

	return prepared, nil
}

// Start attaches the boot-time model-instance set to sess. Each
// stored instance receives a freshly constructed
// [modelclient.ModelClient] subscribed to the session; this is the
// "models that came back from disk" affordance the dispatch loop
// relies on. Returns the count of attach attempts plus any failure
// that surfaced.
//
// Failures are logged per-instance and accumulated; the manager
// returns the first error so the connection screen can surface it,
// but later instances still attempt their attach.
func (m *Manager) Start(ctx context.Context, sess *session.Session) error {
	instances, err := m.store.ListInstances(ctx)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}

	logger := slog.Default()

	var firstErr error
	for _, inst := range instances {
		if _, attachErr := m.Attach(ctx, sess, inst); attachErr != nil {
			logger.WarnContext(ctx, "attach boot model client",
				"component", "modelmanager",
				"instance_id", inst.ID(),
				"error", attachErr,
			)
			if firstErr == nil {
				firstErr = attachErr
			}
		}
	}

	return firstErr
}

// Attach satisfies the session-side `ModelClientFactory.Attach`
// contract. It constructs (or returns the existing handle for) the
// [modelclient.ModelClient] backing `inst` and subscribes it to
// `sess`. Idempotent on a repeat call for the same identity.
func (m *Manager) Attach(ctx context.Context, sess *session.Session, inst *domain.Instance) (protocol.Client, error) {
	id := protocol.ClientID(inst.ID())

	m.clientsMu.Lock()
	if existing, ok := m.clients[id]; ok {
		m.clientsMu.Unlock()
		return existing, nil
	}

	mc := modelclient.New(inst, sess, m.APIClientGetter(), m.memory, m.tools, m.EnsureToolCapableModel, m.CachedContextLen, m.baseContext, m.pacer)
	m.clients[id] = mc
	m.clientsMu.Unlock()

	if err := mc.Attach(ctx); err != nil {
		m.clientsMu.Lock()
		delete(m.clients, id)
		m.clientsMu.Unlock()
		return nil, fmt.Errorf("attach model client %q: %w", id, err)
	}

	return mc, nil
}

// Detach releases the model-client for `id`, ending its connection
// without waiting for its dispatch goroutine to finish. That
// goroutine is where a model's own `quit` tool call runs, so this is
// reached from inside it whenever a model ends its own connection;
// waiting here would be that goroutine waiting on itself.
//
// The client moves to the draining set on the way out, so the
// goroutine it is still running stays reachable by
// [Manager.DetachAll]. Idempotent on an unknown id.
func (m *Manager) Detach(id protocol.ClientID) {
	m.clientsMu.Lock()
	mc, ok := m.clients[id]
	if ok {
		delete(m.clients, id)
		m.draining = append(m.draining, mc)
	}
	m.clientsMu.Unlock()

	if !ok {
		return
	}

	mc.Release()
}

// DetachAll ends every model client's connection and joins its
// dispatch goroutine — the attached ones and the ones already
// draining from an earlier QUIT, KILL, send-queue disconnect or
// failed ADDMODEL. It releases them all first, so the turns they are
// in unwind in parallel, then waits for each.
//
// `ctx` bounds that wait, and is what makes the configured drain
// timeout a real bound on shutdown. Releasing a client cancels the
// context its turn runs under, which an upstream call answers by
// returning; a turn that does not answer it would hold the process
// open for as long as it liked. Past the deadline the remaining
// goroutines are abandoned to the exiting process and the returned
// [DrainTimeoutError] names the clients they belong to.
//
// It carries [modelclient.ModelClient.Wait]'s restriction: never
// from a dispatch turn.
func (m *Manager) DetachAll(ctx context.Context) error {
	clients := m.takeAllClients()

	for _, mc := range clients {
		mc.Release()
	}

	pending := make(map[protocol.ClientID]struct{}, len(clients))

	// Buffered to the full count, so every joiner has a slot waiting
	// for it. One whose client finishes after the deadline, with this
	// call already returned and nobody reading, still completes its
	// send and exits.
	joined := make(chan protocol.ClientID, len(clients))

	for _, mc := range clients {
		pending[mc.Identity()] = struct{}{}

		go func() {
			mc.Wait()
			joined <- mc.Identity()
		}()
	}

	for range clients {
		select {
		case id := <-joined:
			delete(pending, id)
		case <-ctx.Done():
			return &DrainTimeoutError{Abandoned: slices.Sorted(maps.Keys(pending)), Err: ctx.Err()}
		}
	}

	return nil
}

// takeAllClients empties both registries and returns everything that
// was in them: the attached clients and the ones already draining.
func (m *Manager) takeAllClients() []*modelclient.ModelClient {
	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()

	clients := make([]*modelclient.ModelClient, 0, len(m.clients)+len(m.draining))
	for _, mc := range m.clients {
		clients = append(clients, mc)
	}
	clients = append(clients, m.draining...)

	m.clients = make(map[protocol.ClientID]*modelclient.ModelClient)
	m.draining = nil

	return clients
}

// DrainTimeoutError reports the model clients whose dispatch
// goroutine was still running when [Manager.DetachAll]'s deadline
// passed. Each is inside a turn that did not answer its
// cancellation. The process is on its way out, so the deadline
// releases the drain and leaves those goroutines running; naming
// them here is what tells the operator which clients that was.
type DrainTimeoutError struct {
	Abandoned []protocol.ClientID
	Err       error
}

func (e *DrainTimeoutError) Error() string {
	ids := make([]string, len(e.Abandoned))
	for i, id := range e.Abandoned {
		ids[i] = string(id)
	}

	return fmt.Sprintf("drain model clients: %v; %d still dispatching: %s",
		e.Err, len(ids), strings.Join(ids, ", "))
}

func (e *DrainTimeoutError) Unwrap() error { return e.Err }

// inSpan brackets fn with a span and result-recording on the
// manager's tracer provider. The fallback error kind is
// [observability.ErrorKindStore] — most manager operations are
// persistence-backed. Sites that need to override (catalogue
// dispatch failures, validation refusals) wrap their returned error
// with [observability.ErrWithKind], which the classifier here
// unwraps.
func (m *Manager) inSpan(
	ctx context.Context,
	op string,
	attrs []attribute.KeyValue,
	fn func(ctx context.Context, span trace.Span) error,
) error {
	return observability.SpanRunner{
		Tracer:         m.tracer.Tracer("github.com/laney/modeloff/internal/modelmanager"),
		DefaultErrKind: observability.ErrorKindStore,
		ClassifyError:  observability.ErrorKindOf,
	}.Run(ctx, op, attrs, fn)
}
