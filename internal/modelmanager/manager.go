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
	"errors"
	"fmt"
	"log/slog"
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

	Reset(ctx context.Context) error
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
	supportedModels      map[domain.ModelID]struct{}
	supportedModelsReady bool
	state                atomic.Uint32

	clientsMu sync.Mutex
	clients   map[protocol.ClientID]*modelclient.ModelClient
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
				return errWithKind(fmt.Errorf("build api client: %w", err), observability.ErrorKindValidation)
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
// factory and an API key are configured.
func (m *Manager) SetBaseURL(ctx context.Context, baseURL string) error {
	return m.inSpan(ctx, "modelmanager.set_base_url", nil, func(_ context.Context, _ trace.Span) error {
		baseURL = strings.TrimSpace(baseURL)

		m.mu.Lock()
		defer m.mu.Unlock()

		if m.factory != nil && m.apiKey != "" {
			client, err := m.factory(m.apiKey, baseURL)
			if err != nil {
				return errWithKind(fmt.Errorf("build api client: %w", err), observability.ErrorKindValidation)
			}
			m.api = client
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

// Reset clears the store, the memory backend, and the supported-
// models cache. The chat-screen's `/config --reset` semantics rely
// on this returning the application to a fresh state.
func (m *Manager) Reset(ctx context.Context) error {
	return m.inSpan(ctx, "modelmanager.reset", nil, func(ctx context.Context, _ trace.Span) error {
		if err := m.store.Reset(ctx); err != nil {
			return fmt.Errorf("reset store: %w", err)
		}

		if m.memory != nil {
			if err := m.memory.Reset(ctx); err != nil {
				return fmt.Errorf("reset memories: %w", err)
			}
		}

		m.invalidateCatalogue(ctx)

		return nil
	})
}

// maxNickGenerationAttempts caps the number of times the small
// model is asked for a nickname before [Manager.generateUniqueNick]
// gives up. Each retry carries the previously rejected suggestion
// as a follow-up turn so the model picks something different — the
// user's full nick list is intentionally never sent to the model.
const maxNickGenerationAttempts = 3

// generateUniqueNick asks the small model for a nickname guided by
// the assigned persona and retries up to [maxNickGenerationAttempts]
// times if the suggested nick is already taken.
func (m *Manager) generateUniqueNick(
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
			return errWithKind(fmt.Errorf("generate nick: api client not configured"), observability.ErrorKindValidation)
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
				return errWithKind(fmt.Errorf("generate nick: %w", err), observability.ErrorKindDispatch)
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

		return errWithKind(
			fmt.Errorf("generate nick: %d attempts exhausted, all suggestions collided", maxNickGenerationAttempts),
			observability.ErrorKindDispatch,
		)
	})

	return nick, err
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
// before attaching the constructed instance to a channel. Errors
// in persona generation are swallowed (the instance gets an empty
// persona); nick failures and structured-output validation are
// surfaced. The supplied session is consulted for nick-uniqueness
// resolution so the manager does not hold a back-reference.
func (m *Manager) PrepareInstance(
	ctx context.Context,
	sess *session.Session,
	modelID domain.ModelID,
	persona string,
) (domain.Nick, string, error) {
	logger := slog.Default().With("component", "modelmanager", "model_id", modelID)

	if err := m.EnsureStructuredOutputModel(ctx, modelID); err != nil {
		return "", "", err
	}

	assigned := strings.TrimSpace(persona)

	if assigned == "" {
		if err := m.EnsurePersonas(ctx); err != nil {
			logger.WarnContext(ctx, "persona pool generation failed", "error", err)
		}

		if p, err := m.RandomPersona(ctx); err == nil {
			assigned = p.Description
		}
	}

	nick, err := m.generateUniqueNick(ctx, sess, modelID, assigned, logger)
	if err != nil {
		return "", assigned, err
	}

	return nick, assigned, nil
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

	mc := modelclient.New(inst, sess, m.APIClientGetter(), m.memory, m.tools, m.EnsureStructuredOutputModel, m.baseContext, m.pacer)
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

// Detach releases the model-client for `id`. Idempotent on an
// unknown id.
func (m *Manager) Detach(id protocol.ClientID) {
	m.clientsMu.Lock()
	mc, ok := m.clients[id]
	if ok {
		delete(m.clients, id)
	}
	m.clientsMu.Unlock()

	if !ok {
		return
	}

	mc.Detach()
}

// DetachAll releases every attached model client. Test fixtures
// call it on cleanup so the per-instance dispatch goroutines join
// before the next test starts.
func (m *Manager) DetachAll() {
	m.clientsMu.Lock()
	clients := make([]*modelclient.ModelClient, 0, len(m.clients))
	for _, mc := range m.clients {
		clients = append(clients, mc)
	}
	m.clients = make(map[protocol.ClientID]*modelclient.ModelClient)
	m.clientsMu.Unlock()

	for _, mc := range clients {
		mc.Detach()
	}
}

// inSpan brackets fn with a span and result-recording on the
// manager's tracer provider. The fallback error kind is
// [observability.ErrorKindStore] — most manager operations are
// persistence-backed. Sites that need to override (catalogue
// dispatch failures, validation refusals) wrap their returned error
// with [errWithKind], which the classifier here unwraps.
func (m *Manager) inSpan(
	ctx context.Context,
	op string,
	attrs []attribute.KeyValue,
	fn func(ctx context.Context, span trace.Span) error,
) error {
	return observability.SpanRunner{
		Tracer:         m.tracer.Tracer("github.com/laney/modeloff/internal/modelmanager"),
		DefaultErrKind: observability.ErrorKindStore,
		ClassifyError:  classifyManagerError,
	}.Run(ctx, op, attrs, fn)
}

func classifyManagerError(err error) string {
	if ke, ok := errors.AsType[*kindError](err); ok {
		return ke.kind
	}
	return ""
}

// kindError tags an error with an observability error kind so the
// span classifier can attach `AttrErrorKind` without every call site
// having to pass the kind through an auxiliary return value.
type kindError struct {
	kind string
	err  error
}

func (e *kindError) Error() string { return e.err.Error() }
func (e *kindError) Unwrap() error { return e.err }

// errWithKind annotates err with the given observability error kind.
// Returns nil when err is nil so it can wrap the tail of a return.
func errWithKind(err error, kind string) error {
	if err == nil {
		return nil
	}
	return &kindError{kind: kind, err: err}
}
