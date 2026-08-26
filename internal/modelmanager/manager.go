// Package modelmanager owns the LLM-specific state. The session
// router handles only IRC protocol state. This package owns the
// OpenRouter [api.Client] and its rebuild factory, the persona pool,
// the small-model id used for nick generation and persona seeding, the
// cached supported-models catalogue, and the per-instance
// [modelclient.ModelClient] registry that implements
// [session.ModelClientFactory].
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
	"errors"
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
	"github.com/laney/modeloff/internal/store"
)

// Store is the persistence surface the manager depends on. The
// concrete `*store.SQLiteStore` satisfies it implicitly. Persona
// arbitration writes through the store; the per-instance client
// registry reads the boot-time instance list to attach existing
// model clients.
type Store interface {
	ListInstances(ctx context.Context) ([]*domain.Instance, error)
	ListPendingInstanceDeletions(ctx context.Context) ([]domain.InstanceID, error)
	ListPendingMemoryDeletions(ctx context.Context) ([]domain.InstanceID, error)
	DeleteInstanceByID(ctx context.Context, id domain.InstanceID) error
	DeleteMemoriesByInstance(ctx context.Context, id domain.InstanceID) error
	DeletePendingMemoryDeletion(ctx context.Context, id domain.InstanceID) error

	ListPersonas(ctx context.Context) ([]domain.Persona, error)
	SavePersona(ctx context.Context, p domain.Persona) error
	DeletePersonasByOrigin(ctx context.Context, origin domain.PersonaOrigin) error
	ReplaceGeneratedPersonas(ctx context.Context, personas []domain.Persona) error
	AppendReflectionEvents(
		ctx context.Context,
		instanceID domain.InstanceID,
		candidates []store.ReflectionEventCandidate,
		createdAt time.Time,
	) error
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
// [session.ModelClientFactory] via [Manager.Attach], [Manager.Detach]
// and [Manager.Forget] so a single value passes to `session.New`.
type Manager struct {
	store            Store
	memory           memory.Store
	tools            *modelclient.ToolRegistry
	baseContext      func() context.Context
	lifecycleContext context.Context
	cancelLifecycle  context.CancelFunc
	now              func() time.Time
	tracer           trace.TracerProvider
	pacer            *modelclient.Pacer

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

	// clients holds the attached model-clients; attaching records
	// clients still loading their history; draining holds the ones
	// already released, whose dispatch goroutines and deferred state
	// deletion are on their way out. stopping becomes true before
	// DetachAll snapshots either client set, so no later attach can
	// fall outside that snapshot. All four are guarded by clientsMu.
	//
	// A released client leaves `clients` the moment it is released,
	// because its identity is free again from that point. It moves to
	// `draining` so [Manager.DetachAll] can wait for the background
	// finaliser. A client dropped at release would take its goroutine
	// and any pending deletion out of shutdown's reach.
	clientsMu sync.Mutex
	clients   map[protocol.ClientID]*modelclient.ModelClient
	attaching map[protocol.ClientID]*clientAttachment
	draining  map[protocol.ClientID]*drainingClient
	// pendingMemoryDeletes records attached clients whose instance
	// deletion has committed. Detach transfers the obligation to the
	// client's draining entry before it releases the dispatch goroutine.
	pendingMemoryDeletes map[protocol.ClientID]struct{}
	stopping             bool
}

type clientAttachment struct {
	client protocol.Client
	err    error
	done   chan struct{}
}

type drainingClient struct {
	client    *modelclient.ModelClient
	forget    bool
	abandoned bool
	done      chan struct{}
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

	lifecycleBase := context.Background()
	if cfg.BaseContext != nil {
		if base := cfg.BaseContext(); base != nil {
			lifecycleBase = context.WithoutCancel(base)
		}
	}
	lifecycleContext, cancelLifecycle := context.WithCancel(lifecycleBase)

	manager := &Manager{
		store:                cfg.Store,
		memory:               cfg.Memory,
		tools:                cfg.Tools,
		baseContext:          cfg.BaseContext,
		lifecycleContext:     lifecycleContext,
		cancelLifecycle:      cancelLifecycle,
		now:                  now,
		tracer:               tracer,
		pacer:                pacer,
		api:                  cfg.APIClient,
		apiKey:               strings.TrimSpace(cfg.InitialAPIKey),
		smallModel:           smallModel,
		factory:              cfg.APIFactory,
		clients:              make(map[protocol.ClientID]*modelclient.ModelClient),
		attaching:            make(map[protocol.ClientID]*clientAttachment),
		draining:             make(map[protocol.ClientID]*drainingClient),
		pendingMemoryDeletes: make(map[protocol.ClientID]struct{}),
	}
	manager.sizeCompletions(cfg.APIClient)

	return manager
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
		m.sizeCompletions(nextClient)
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
			m.sizeCompletions(client)
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
// [maxNickGenerationAttempts] times if the suggested nick fails
// [domain.ValidateNick] or is already taken. Either way the rejected
// suggestion is carried into the next attempt's exclusion list, so a
// nick that would be refused with [domain.ErroneousNicknameError]
// (432) is never handed to the caller any more than one that would
// collide with [domain.NickInUseError] (433) is.
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

		var rejected []api.RejectedNick

		for attempt := 1; attempt <= maxNickGenerationAttempts; attempt++ {
			result, err := callGenerateNick(generateCtx, client, small, persona, rejected)
			if err != nil {
				logger.ErrorContext(ctx, "generate nick failed",
					"error", err,
					"attempt", attempt,
				)
				return observability.ErrWithKind(fmt.Errorf("generate nick: %w", err), observability.ErrorKindDispatch)
			}

			result.Usage.SetSpanAttributes(generateSpan, result.RequestID)

			if rejection := domain.ValidateNick(result.Nick); rejection != domain.NickAccepted {
				logger.InfoContext(ctx, "generated nick fails the nick grammar",
					"nick", result.Nick,
					"attempt", attempt,
					"reason", rejection.String(),
				)
				rejected = append(rejected, api.RejectedNick{
					Nick:   result.Nick,
					Reason: "doesn't satisfy the nick grammar: " + rejection.String(),
				})

				continue
			}

			if !m.nickIsTaken(ctx, sess, result.Nick) {
				nick = result.Nick
				return nil
			}

			logger.InfoContext(ctx, "generated nick already in use",
				"nick", result.Nick,
				"attempt", attempt,
			)
			rejected = append(rejected, api.RejectedNick{Nick: result.Nick, Reason: "is already taken"})
		}

		return observability.ErrWithKind(
			fmt.Errorf("generate nick: %d attempts exhausted, no suggestion was usable", maxNickGenerationAttempts),
			observability.ErrorKindDispatch,
		)
	})

	return nick, err
}

// callGenerateNick asks client for a nickname, carrying each
// rejected suggestion's reason through [api.NickReasonGenerator] when
// client implements that optional capability, so the retry prompt
// can tell a grammar rejection from a plain collision. A client that
// does not, which is every fake and every provider besides
// [github.com/laney/modeloff/internal/api.OpenRouterClient], falls
// back to plain [api.Client.GenerateNick], whose fixed wording covers
// only a collision.
func callGenerateNick(
	ctx context.Context,
	client api.Client,
	smallModel domain.ModelID,
	persona string,
	rejected []api.RejectedNick,
) (api.NicknameResult, error) {
	if reasoner, ok := client.(api.NickReasonGenerator); ok {
		return reasoner.GenerateNickWithReasons(ctx, smallModel, persona, rejected)
	}

	var plain []domain.Nick
	for _, r := range rejected {
		plain = append(plain, r.Nick)
	}

	return client.GenerateNick(ctx, smallModel, persona, plain)
}

// deterministicNickBase derives a nick-safe base string from a model
// id: the segment after the last `/`, lowercased, with every
// character outside the nick charset (`[a-z0-9_-]`) replaced by `-`,
// trimmed of leading and trailing `-`, and capped at
// [deterministicNickBaseLen] so a numeric collision suffix still fits
// within the 12-character nick limit. [sanitizeNickBase] then fixes
// what that substitution can still leave behind, so the result always
// satisfies [domain.ValidateNick].
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

	return sanitizeNickBase(base)
}

// sanitizeNickBase guarantees its result satisfies
// [domain.ValidateNick], given a base already restricted to the nick
// charset (`[a-z0-9_-]`) and at most [deterministicNickBaseLen]
// characters, which is the shape deterministicNickBase builds before
// calling it. Within that precondition, the only failures left to fix
// are an empty result, a base that folded onto the reserved
// [domain.AnonymousNick], and a digit-leading one: RFC 2812 §2.3.1
// requires a letter or one of the specials first; the substitution
// step admits digits into that charset, but the grammar refuses them
// in the first position.
func sanitizeNickBase(base string) string {
	if base == "" || domain.EqualNick(domain.Nick(base), domain.AnonymousNick) {
		return "model"
	}

	if domain.ValidateNick(domain.Nick(base)) == domain.NickBadFirstCharacter {
		base = "m" + base
		if len(base) > deterministicNickBaseLen {
			base = base[:deterministicNickBaseLen]
		}
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

// nickIsTaken reports whether `nick` is already claimed, by the user
// or any registered model instance. A claim exists before the model
// connection attaches, so reachability is not the relevant test.
// This call only screens a candidate before it is proposed;
// `requireNickAvailable` is the check that actually claims a nick and
// is the one that has to be right.
func (m *Manager) nickIsTaken(ctx context.Context, sess *session.Session, nick domain.Nick) bool {
	claimed, err := sess.NickClaimed(ctx, nick)
	return err != nil || claimed
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

	resolvedPersona, assigned, err := m.resolvePersona(ctx, persona)
	if err != nil {
		return session.PreparedInstance{}, err
	}

	prepared := session.PreparedInstance{Persona: resolvedPersona}
	if assigned {
		if reason := domain.ValidatePersona(prepared.Persona); reason != domain.PersonaAccepted {
			return prepared, domain.ErroneousPersonaError{Reason: reason, At: m.now()}
		}
	}

	if err := m.EnsureToolCapableModel(ctx, modelID); err != nil {
		return session.PreparedInstance{}, err
	}

	if !assigned {
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

	if reason := domain.ValidatePersona(prepared.Persona); reason != domain.PersonaAccepted {
		return prepared, domain.ErroneousPersonaError{Reason: reason, At: m.now()}
	}

	nick, err := m.generateUniqueNick(ctx, sess, modelID, prepared.Persona, logger)
	if err != nil {
		return prepared, err
	}

	prepared.Nick = nick

	return prepared, nil
}

// resolvePersona copies a persona template when requested is its exact ID.
// Text that does not identify a template remains a literal persona.
func (m *Manager) resolvePersona(ctx context.Context, requested string) (string, bool, error) {
	if requested == "" {
		return "", false, nil
	}

	personas, err := m.store.ListPersonas(ctx)
	if err != nil {
		return "", false, fmt.Errorf("resolve persona %q: %w", requested, err)
	}

	for _, persona := range personas {
		if persona.ID == requested {
			return persona.Description, true, nil
		}
	}

	return requested, true, nil
}

// Start attaches the boot-time model-instance set to sess. Each
// stored instance receives a freshly constructed
// [modelclient.ModelClient] subscribed to the session; this is the
// "models that came back from disk" affordance the dispatch loop
// relies on. Returns the count of attach attempts plus any failure
// that surfaced.
//
// An instance the session already holds a client for is skipped, so
// a client that registered its own row and connected before the boot
// runs keeps the one subscription it has.
//
// Failures are logged per-instance and accumulated, but later instances still
// attempt their attach. [StartError] distinguishes cleanup failures from an
// attachment failure that leaves stored membership without a live client.
func (m *Manager) Start(ctx context.Context, sess *session.Session) error {
	cleanupErr := m.deletePendingInstances(ctx)
	if err := m.deletePendingMemoryCollections(ctx); cleanupErr == nil {
		cleanupErr = err
	}

	attachmentErr := sess.StartModelClients(ctx)
	if cleanupErr == nil && attachmentErr == nil {
		return nil
	}

	return &StartError{cleanup: cleanupErr, attachment: attachmentErr}
}

// StartError reports partial failures from [Manager.Start]. Cleanup failures
// leave usable attached clients in place. An attachment failure leaves a stored
// actor without the live client required by its channel membership.
type StartError struct {
	cleanup    error
	attachment error
}

func (e *StartError) Error() string {
	return errors.Join(e.cleanup, e.attachment).Error()
}

func (e *StartError) Unwrap() []error {
	return []error{e.cleanup, e.attachment}
}

// AttachmentFailed reports whether startup left a stored actor unattached.
func (e *StartError) AttachmentFailed() bool {
	return e.attachment != nil
}

func (m *Manager) deletePendingInstances(ctx context.Context) error {
	ids, err := m.store.ListPendingInstanceDeletions(ctx)
	if err != nil {
		return fmt.Errorf("list pending instance deletions: %w", err)
	}

	var firstErr error
	for _, id := range ids {
		if err := m.store.DeleteInstanceByID(ctx, id); err != nil {
			slog.Default().WarnContext(ctx, "delete pending instance",
				"component", "modelmanager",
				"instance_id", id,
				"error", err,
			)
			if firstErr == nil {
				firstErr = fmt.Errorf("delete pending instance %q: %w", id, err)
			}
			continue
		}

	}

	return firstErr
}

func (m *Manager) deletePendingMemoryCollections(ctx context.Context) error {
	ids, err := m.store.ListPendingMemoryDeletions(ctx)
	if err != nil {
		return fmt.Errorf("list pending memory deletions: %w", err)
	}

	var firstErr error
	for _, id := range ids {
		if err := m.finishMemoryDeletion(ctx, id); err != nil {
			slog.Default().WarnContext(ctx, "delete pending memory collection",
				"component", "modelmanager",
				"instance_id", id,
				"error", err,
			)
			if firstErr == nil {
				firstErr = fmt.Errorf("delete pending memory collection %q: %w", id, err)
			}
		}
	}

	return firstErr
}

// Attach satisfies the session-side `ModelClientFactory.Attach`
// contract. It constructs (or returns the existing handle for) the
// [modelclient.ModelClient] backing `inst` and subscribes it to
// `sess`. Idempotent on a repeat call for the same identity.
func (m *Manager) Attach(
	ctx context.Context,
	sess *session.Session,
	inst *domain.Instance,
	attachment *protocol.Attachment,
) (protocol.Client, error) {
	id := protocol.ClientID(inst.ID())

	m.clientsMu.Lock()
	if m.stopping {
		m.clientsMu.Unlock()
		return nil, &ManagerDrainingError{InstanceID: inst.ID()}
	}
	if attaching, ok := m.attaching[id]; ok {
		m.clientsMu.Unlock()

		select {
		case <-attaching.done:
			return attaching.client, attaching.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if existing, ok := m.clients[id]; ok {
		m.clientsMu.Unlock()
		return existing, nil
	}

	mc := modelclient.New(modelclient.Config{
		Instance:        inst,
		Attachment:      attachment,
		Session:         sess,
		APIClient:       m.APIClientGetter(),
		Memory:          m.memory,
		Tools:           m.tools,
		EnsureModel:     m.EnsureToolCapableModel,
		ContextLen:      m.CachedContextLen,
		LifetimeContext: m.baseContext,
		JournalContext:  m.lifecycleContext,
		Pacer:           m.pacer,
		Journal:         sess,
		Contexts:        sess,
		Reflections:     m.store,
		Now:             m.now,
	})
	attaching := &clientAttachment{done: make(chan struct{})}
	m.clients[id] = mc
	m.attaching[id] = attaching
	m.clientsMu.Unlock()

	attachErr := mc.Attach(ctx)

	m.clientsMu.Lock()
	forgetAfterAttach := false
	current := m.clients[id]
	switch {
	case m.stopping:
		attaching.err = &ManagerDrainingError{InstanceID: inst.ID()}
	case current != mc:
		if attachErr == nil {
			attachErr = modelclient.ErrReleased
		}
		attaching.err = fmt.Errorf("attach model client %q: %w", id, attachErr)
	case attachErr != nil:
		delete(m.clients, id)
		_, forgetAfterAttach = m.pendingMemoryDeletes[id]
		delete(m.pendingMemoryDeletes, id)
		attaching.err = fmt.Errorf("attach model client %q: %w", id, attachErr)
	default:
		attaching.client = mc
	}
	delete(m.attaching, id)
	close(attaching.done)
	m.clientsMu.Unlock()
	if forgetAfterAttach {
		m.Forget(id)
	}

	return attaching.client, attaching.err
}

// InterruptTurn cancels the current provider turn for `id` without
// ending its connection.
func (m *Manager) InterruptTurn(id protocol.ClientID) {
	m.clientsMu.Lock()
	mc := m.clients[id]
	if mc == nil {
		if entry := m.draining[id]; entry != nil {
			mc = entry.client
		}
	}
	m.clientsMu.Unlock()

	if mc != nil {
		mc.InterruptTurn()
	}
}

// InterruptWindow cancels `id`'s provider turn when it belongs to
// `window`, without ending the model-client's connection.
func (m *Manager) InterruptWindow(id protocol.ClientID, window domain.ChannelName) {
	m.clientsMu.Lock()
	mc := m.clients[id]
	if mc == nil {
		if entry := m.draining[id]; entry != nil {
			mc = entry.client
		}
	}
	m.clientsMu.Unlock()

	if mc != nil {
		mc.InterruptWindow(window)
	}
}

// InstanceDeleted records that `id`'s instance has been deleted. The
// cleanup obligation remains until the dispatch goroutine has stopped.
func (m *Manager) InstanceDeleted(id protocol.ClientID) {
	m.clientsMu.Lock()
	mc := m.clients[id]
	entry := m.draining[id]
	deleteNow := mc == nil && entry == nil
	if mc != nil {
		m.pendingMemoryDeletes[id] = struct{}{}
	} else if entry != nil {
		entry.forget = true
	}
	m.clientsMu.Unlock()

	if deleteNow {
		m.Forget(id)
	}
}

// Detach releases the model-client for `id`, ending its connection
// without deleting its model-owned state.
func (m *Manager) Detach(id protocol.ClientID) {
	m.detach(id, false)
}

// DetachAndForget releases the model-client for `id` and deletes its
// model-owned state after the dispatch goroutine has stopped. The wait
// runs on a separate goroutine because a model can reach this method
// from its own `quit` tool call.
func (m *Manager) DetachAndForget(id protocol.ClientID) {
	if m.detach(id, true) {
		return
	}

	m.Forget(id)
}

func (m *Manager) detach(id protocol.ClientID, forget bool) bool {
	m.clientsMu.Lock()
	_, pendingForget := m.pendingMemoryDeletes[id]
	delete(m.pendingMemoryDeletes, id)
	forget = forget || pendingForget
	mc, ok := m.clients[id]
	if ok {
		delete(m.clients, id)
		entry := &drainingClient{client: mc, forget: forget, done: make(chan struct{})}
		m.draining[id] = entry
		m.clientsMu.Unlock()

		mc.Release()
		go m.finishDrain(id, entry)

		return true
	}

	if entry, draining := m.draining[id]; draining {
		entry.forget = entry.forget || forget
		m.clientsMu.Unlock()

		return true
	}
	m.clientsMu.Unlock()

	return false
}

func (m *Manager) finishDrain(id protocol.ClientID, entry *drainingClient) {
	entry.client.Wait()

	m.clientsMu.Lock()
	current, ok := m.draining[id]
	if !ok || current != entry {
		m.clientsMu.Unlock()
		return
	}
	if entry.abandoned {
		delete(m.draining, id)
		close(entry.done)
		m.clientsMu.Unlock()

		return
	}
	forget := entry.forget
	if !forget {
		delete(m.draining, id)
		close(entry.done)
		m.clientsMu.Unlock()

		return
	}
	m.clientsMu.Unlock()

	m.Forget(id)

	m.clientsMu.Lock()
	if m.draining[id] == entry {
		delete(m.draining, id)
		close(entry.done)
	}
	m.clientsMu.Unlock()
}

// Forget removes model-owned state after the session has deleted the
// corresponding instance row. Connection release is separate because
// a server-forced disconnect must still reap a dead client when the
// row deletion fails.
func (m *Manager) Forget(id protocol.ClientID) {
	ctx, cancel := m.memoryDeletionContext()
	defer cancel()

	if err := m.finishMemoryDeletion(ctx, domain.InstanceID(id)); err != nil {
		slog.Default().WarnContext(ctx, "delete memory collection",
			"component", "modelmanager",
			"instance_id", string(id),
			"error", err,
		)
	}
}

func (m *Manager) memoryDeletionContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(m.lifecycleContext, config.DefaultDrainTimeout)
}

// deleteMemoryCollection removes id's memory-index state through
// [memory.InstanceDeleter]. A store without that capability cannot
// confirm that an existing index was removed, so the pending marker
// remains for a later indexed store to retry.
func (m *Manager) deleteMemoryCollection(ctx context.Context, id domain.InstanceID) (bool, error) {
	deleter, ok := m.memory.(memory.InstanceDeleter)
	if !ok {
		return false, nil
	}

	return true, deleter.DeleteInstance(ctx, id)
}

func (m *Manager) finishMemoryDeletion(ctx context.Context, id domain.InstanceID) error {
	collectionDeleted, collectionErr := m.deleteMemoryCollection(ctx, id)
	backingErr := m.store.DeleteMemoriesByInstance(ctx, id)
	if err := errors.Join(collectionErr, backingErr); err != nil {
		return err
	}
	if !collectionDeleted {
		return nil
	}

	return m.store.DeletePendingMemoryDeletion(ctx, id)
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
	defer m.cancelLifecycle()

	clients, started, deleted := m.beginAllDrains()

	for id, entry := range started {
		entry.client.Release()
		go m.finishDrain(id, entry)
	}
	for _, id := range deleted {
		m.Forget(id)
	}

	pending := make(map[protocol.ClientID]struct{}, len(clients))
	joined := make(chan protocol.ClientID, len(clients))
	for id, entry := range clients {
		pending[id] = struct{}{}

		go func() {
			<-entry.done
			joined <- id
		}()
	}

	for range clients {
		select {
		case id := <-joined:
			delete(pending, id)
		case <-ctx.Done():
			m.abandonDrains(pending, clients)
			return &DrainTimeoutError{Abandoned: slices.Sorted(maps.Keys(pending)), Err: ctx.Err()}
		}
	}

	return nil
}

// beginAllDrains moves every attached client into the draining set
// and returns all work that DetachAll must join.
func (m *Manager) beginAllDrains() (
	map[protocol.ClientID]*drainingClient,
	map[protocol.ClientID]*drainingClient,
	[]protocol.ClientID,
) {
	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()
	m.stopping = true

	clients := make(map[protocol.ClientID]*drainingClient, len(m.clients)+len(m.draining))
	started := make(map[protocol.ClientID]*drainingClient, len(m.clients))
	for id, entry := range m.draining {
		if !entry.abandoned {
			clients[id] = entry
		}
	}
	for id, mc := range m.clients {
		_, forget := m.pendingMemoryDeletes[id]
		delete(m.pendingMemoryDeletes, id)
		entry := &drainingClient{client: mc, forget: forget, done: make(chan struct{})}
		m.draining[id] = entry
		clients[id] = entry
		started[id] = entry
	}

	m.clients = make(map[protocol.ClientID]*modelclient.ModelClient)
	deleted := slices.Sorted(maps.Keys(m.pendingMemoryDeletes))
	m.pendingMemoryDeletes = make(map[protocol.ClientID]struct{})

	return clients, started, deleted
}

func (m *Manager) abandonDrains(
	pending map[protocol.ClientID]struct{},
	clients map[protocol.ClientID]*drainingClient,
) {
	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()

	for id := range pending {
		entry := clients[id]
		if m.draining[id] == entry {
			entry.abandoned = true
		}
	}
}

// ManagerDrainingError reports an attach attempted after the manager
// began its terminal drain.
type ManagerDrainingError struct {
	InstanceID domain.InstanceID
}

func (e *ManagerDrainingError) Error() string {
	return fmt.Sprintf("attach model client %q: model manager is draining", e.InstanceID)
}

// DrainTimeoutError reports the model clients whose finaliser was
// still running when [Manager.DetachAll]'s deadline passed. A client
// may still be inside a turn that did not answer cancellation or
// deleting model-owned state after that turn ended.
type DrainTimeoutError struct {
	Abandoned []protocol.ClientID
	Err       error
}

func (e *DrainTimeoutError) Error() string {
	ids := make([]string, len(e.Abandoned))
	for i, id := range e.Abandoned {
		ids[i] = string(id)
	}

	return fmt.Sprintf("drain model clients: %v; %d still draining: %s",
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

// sizeCompletions tells a client where to look up a model's context
// window, so it can cap a completion by what the request's own prompt
// leaves. The catalogue cache is the same number the dispatch planner
// reserves against, which is what keeps the reserve and the cap from
// diverging.
func (m *Manager) sizeCompletions(client api.Client) {
	if lookup, ok := client.(api.ContextWindowLookup); ok {
		lookup.SetContextWindows(m.CachedContextLen)
	}
}
