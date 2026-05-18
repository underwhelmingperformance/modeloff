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
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
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

// ListState reports the manager's view of the cached model
// catalogue. The values let tests assert short-circuit behaviour
// in [Manager.EnsureStructuredOutputModel] after an upstream
// failure or a fresh `SetAPIKey`.
type ListState uint32

const (
	// ListStateNone is the initial state: the catalogue has never
	// been fetched. The next add-model lazy-loads.
	ListStateNone ListState = iota
	// ListStateOK reflects the last successful upstream round-trip.
	ListStateOK
	// ListStateFailed marks the catalogue as known-stale after an
	// upstream failure. The manager short-circuits with
	// [modelclient.ErrModelListUnavailable] until a `SetAPIKey` /
	// `Reset` invalidates the cache.
	ListStateFailed
)

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
	_, span := m.startSpan(ctx, "modelmanager.set_api_key",
		attribute.String(observability.AttrOperation, "modelmanager.set_api_key"))
	defer span.End()

	apiKey = strings.TrimSpace(apiKey)

	m.mu.Lock()
	nextClient := m.api
	if apiKey != "" && m.factory != nil {
		client, err := m.factory(apiKey, baseURL)
		if err != nil {
			m.mu.Unlock()
			setSpanError(span, err, observability.ErrorKindValidation)
			return fmt.Errorf("build api client: %w", err)
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

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
	return nil
}

// SetBaseURL rebuilds the API client with the given base URL if a
// factory and an API key are configured.
func (m *Manager) SetBaseURL(ctx context.Context, baseURL string) error {
	_, span := m.startSpan(ctx, "modelmanager.set_base_url",
		attribute.String(observability.AttrOperation, "modelmanager.set_base_url"))
	defer span.End()

	baseURL = strings.TrimSpace(baseURL)

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.factory != nil && m.apiKey != "" {
		client, err := m.factory(m.apiKey, baseURL)
		if err != nil {
			setSpanError(span, err, observability.ErrorKindValidation)
			return fmt.Errorf("build api client: %w", err)
		}
		m.api = client
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
	return nil
}

// SetSmallModel updates the model id the manager uses for nick
// generation and persona seeding.
func (m *Manager) SetSmallModel(ctx context.Context, modelID domain.ModelID) {
	_, span := m.startSpan(ctx, "modelmanager.set_small_model",
		attribute.String(observability.AttrOperation, "modelmanager.set_small_model"),
		attribute.String(observability.AttrModelID, string(modelID)))
	defer span.End()

	m.mu.Lock()
	m.smallModel = modelID
	m.mu.Unlock()

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
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

// ListModels fetches the live model catalogue from the upstream
// API and caches it. Returns [modelclient.ErrNoAPIKey] when no API
// key is configured.
func (m *Manager) ListModels(ctx context.Context) ([]api.ModelInfo, error) {
	ctx, span := m.startSpan(ctx, "modelmanager.list_models",
		attribute.String(observability.AttrOperation, "modelmanager.list_models"))
	defer span.End()

	client, key := m.snapshotAPI()
	if key == "" || client == nil {
		setSpanError(span, modelclient.ErrNoAPIKey, observability.ErrorKindValidation)
		return nil, modelclient.ErrNoAPIKey
	}

	models, err := client.ListModels(ctx)
	if err != nil {
		m.transitionListState(ctx, ListStateFailed, err)
		setSpanError(span, err, observability.ErrorKindDispatch)
		return nil, err
	}

	m.cacheSupportedModels(ctx, models)

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
	return models, nil
}

// EnsureStructuredOutputModel validates that the given model
// supports structured outputs, lazy-loading the catalogue if
// needed. Returns [modelclient.ErrModelListUnavailable] when the
// cached state recorded an upstream failure;
// [modelclient.ErrNoAPIKey] when no key is configured (silently —
// no API key means no LLM concerns, so callers ignore the check);
// or [domain.UnsupportedModelError] when the catalogue does not
// include the model.
func (m *Manager) EnsureStructuredOutputModel(ctx context.Context, modelID domain.ModelID) error {
	client, key := m.snapshotAPI()
	if key == "" || client == nil {
		return nil
	}

	if ListState(m.state.Load()) == ListStateFailed {
		slog.Default().InfoContext(ctx, "add-model short-circuited: model list unavailable",
			"component", "modelmanager",
			"model_id", string(modelID),
		)
		return modelclient.ErrModelListUnavailable
	}

	m.cacheMu.Lock()
	ready := m.supportedModelsReady
	m.cacheMu.Unlock()

	if !ready {
		models, err := client.ListModels(ctx)
		if err != nil {
			m.transitionListState(ctx, ListStateFailed, err)
			return fmt.Errorf("list models: %w", err)
		}
		m.cacheSupportedModels(ctx, models)
	}

	m.cacheMu.Lock()
	_, ok := m.supportedModels[modelID]
	m.cacheMu.Unlock()

	if !ok {
		return domain.UnsupportedModelError{ModelID: modelID, At: m.now()}
	}

	return nil
}

// ListState reports the manager's current catalogue state. Tests
// use it to assert the manager's view of the upstream after a
// `ListModels` or `EnsureStructuredOutputModel` call.
func (m *Manager) ListState() ListState {
	return ListState(m.state.Load())
}

// SupportedModelsReady reports whether the catalogue cache has
// been populated by a successful round-trip. Tests use it to pin
// `SetAPIKey` and `Reset` cache invalidation behaviour.
func (m *Manager) SupportedModelsReady() bool {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()

	return m.supportedModelsReady
}

// SupportedModels returns a snapshot of the cached supported-model
// set. The returned map is shared with the cache; callers should
// not mutate it. Tests use this to assert the contents after a
// successful `ListModels`.
func (m *Manager) SupportedModels() map[domain.ModelID]struct{} {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()

	return m.supportedModels
}

// invalidateCatalogue clears the supported-models cache and resets
// the catalogue state to "never attempted".
func (m *Manager) invalidateCatalogue(ctx context.Context) {
	m.cacheMu.Lock()
	m.supportedModels = nil
	m.supportedModelsReady = false
	m.cacheMu.Unlock()

	m.transitionListState(ctx, ListStateNone, nil)
}

func (m *Manager) cacheSupportedModels(ctx context.Context, models []api.ModelInfo) {
	cache := make(map[domain.ModelID]struct{}, len(models))
	for _, model := range models {
		cache[model.ID] = struct{}{}
	}

	m.cacheMu.Lock()
	m.supportedModels = cache
	m.supportedModelsReady = true
	m.cacheMu.Unlock()

	m.transitionListState(ctx, ListStateOK, nil)
}

// transitionListState atomically updates the catalogue state and
// logs the transition so operators can correlate add-model short-
// circuits with the upstream failure that put the catalogue into a
// known-stale state.
func (m *Manager) transitionListState(ctx context.Context, to ListState, err error) {
	from := ListState(m.state.Swap(uint32(to)))

	if from == to {
		return
	}

	attrs := []any{
		"component", "modelmanager",
		"from", listStateName(from),
		"to", listStateName(to),
	}

	if err != nil {
		attrs = append(attrs, "error", err)
	}

	if to == ListStateFailed {
		slog.Default().WarnContext(ctx, "model list state transitioned", attrs...)
		return
	}

	slog.Default().InfoContext(ctx, "model list state transitioned", attrs...)
}

func listStateName(s ListState) string {
	switch s {
	case ListStateNone:
		return "none"
	case ListStateOK:
		return "ok"
	case ListStateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// snapshotAPI atomically reads the current API client + key pair
// under the manager's read lock.
func (m *Manager) snapshotAPI() (api.Client, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.api, m.apiKey
}

// EnsurePersonas populates the persona pool if it is empty. It
// calls the API to generate personas and saves each to the store.
func (m *Manager) EnsurePersonas(ctx context.Context) (retErr error) {
	ctx, span := m.startSpan(ctx, "modelmanager.ensure_personas",
		attribute.String(observability.AttrOperation, "modelmanager.ensure_personas"),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	existing, err := m.store.ListPersonas(ctx)
	if err != nil {
		return fmt.Errorf("list personas: %w", err)
	}

	if len(existing) > 0 {
		return nil
	}

	client, _ := m.snapshotAPI()
	if client == nil {
		return fmt.Errorf("generate personas: api client not configured")
	}

	personas, err := client.GeneratePersonas(ctx, m.SmallModel())
	if err != nil {
		return fmt.Errorf("generate personas: %w", err)
	}

	for _, p := range personas {
		if err := m.store.SavePersona(ctx, p); err != nil {
			return fmt.Errorf("save persona %q: %w", p.ID, err)
		}
	}

	return nil
}

// RandomPersona picks a random persona from the store pool.
func (m *Manager) RandomPersona(ctx context.Context) (_ domain.Persona, retErr error) {
	ctx, span := m.startSpan(ctx, "modelmanager.random_persona",
		attribute.String(observability.AttrOperation, "modelmanager.random_persona"),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	personas, err := m.store.ListPersonas(ctx)
	if err != nil {
		return domain.Persona{}, fmt.Errorf("list personas: %w", err)
	}

	if len(personas) == 0 {
		return domain.Persona{}, fmt.Errorf("no personas available")
	}

	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(personas))))
	if err != nil {
		return domain.Persona{}, fmt.Errorf("random selection: %w", err)
	}

	return personas[n.Int64()], nil
}

// RegeneratePersonas generates a fresh set of personas via the
// API, then replaces all generated personas in the store. The API
// call happens first so that the existing pool is preserved if
// generation fails. User-defined personas are never touched.
func (m *Manager) RegeneratePersonas(ctx context.Context) (_ []domain.Persona, retErr error) {
	ctx, span := m.startSpan(ctx, "modelmanager.regenerate_personas",
		attribute.String(observability.AttrOperation, "modelmanager.regenerate_personas"),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	client, _ := m.snapshotAPI()
	if client == nil {
		return nil, fmt.Errorf("generate personas: api client not configured")
	}

	personas, err := client.GeneratePersonas(ctx, m.SmallModel())
	if err != nil {
		return nil, fmt.Errorf("generate personas: %w", err)
	}

	if err := m.store.ReplaceGeneratedPersonas(ctx, personas); err != nil {
		return nil, fmt.Errorf("replace generated personas: %w", err)
	}

	return personas, nil
}

// SetPersona saves a user-defined persona to the store.
func (m *Manager) SetPersona(ctx context.Context, id string, description string) (retErr error) {
	ctx, span := m.startSpan(ctx, "modelmanager.set_persona",
		attribute.String(observability.AttrOperation, "modelmanager.set_persona"),
		attribute.String("persona.id", id),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	p := domain.Persona{
		ID:          id,
		Description: description,
		Origin:      domain.PersonaUser,
	}

	return m.store.SavePersona(ctx, p)
}

// ListPersonas returns all personas from the store.
func (m *Manager) ListPersonas(ctx context.Context) (_ []domain.Persona, retErr error) {
	ctx, span := m.startSpan(ctx, "modelmanager.list_personas",
		attribute.String(observability.AttrOperation, "modelmanager.list_personas"),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	return m.store.ListPersonas(ctx)
}

// ResetPersonas removes all user-defined personas from the store,
// leaving only generated ones. It returns the number of personas
// that were removed.
func (m *Manager) ResetPersonas(ctx context.Context) (_ int, retErr error) {
	ctx, span := m.startSpan(ctx, "modelmanager.reset_personas",
		attribute.String(observability.AttrOperation, "modelmanager.reset_personas"),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	personas, err := m.store.ListPersonas(ctx)
	if err != nil {
		return 0, fmt.Errorf("list personas: %w", err)
	}

	count := 0
	for _, p := range personas {
		if p.Origin == domain.PersonaUser {
			count++
		}
	}

	if err := m.store.DeletePersonasByOrigin(ctx, domain.PersonaUser); err != nil {
		return 0, err
	}

	return count, nil
}

// Reset clears the store, the memory backend, and the supported-
// models cache. The chat-screen's `/config --reset` semantics rely
// on this returning the application to a fresh state.
func (m *Manager) Reset(ctx context.Context) error {
	ctx, span := m.startSpan(ctx, "modelmanager.reset",
		attribute.String(observability.AttrOperation, "modelmanager.reset"))
	defer span.End()

	if err := m.store.Reset(ctx); err != nil {
		setSpanError(span, err, observability.ErrorKindStore)
		return fmt.Errorf("reset store: %w", err)
	}

	if m.memory != nil {
		if err := m.memory.Reset(ctx); err != nil {
			setSpanError(span, err, observability.ErrorKindStore)
			return fmt.Errorf("reset memories: %w", err)
		}
	}

	m.invalidateCatalogue(ctx)

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
	return nil
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
	generateCtx, generateSpan := m.startSpan(
		ctx,
		"modelmanager.generate_nick",
		attribute.String(observability.AttrOperation, "modelmanager.generate_nick"),
		attribute.String(observability.AttrModelID, string(modelID)),
	)
	defer generateSpan.End()

	client, _ := m.snapshotAPI()
	if client == nil {
		err := fmt.Errorf("generate nick: api client not configured")
		setSpanError(generateSpan, err, observability.ErrorKindValidation)
		return "", err
	}

	small := m.SmallModel()

	var rejected []domain.Nick

	for attempt := 1; attempt <= maxNickGenerationAttempts; attempt++ {
		result, err := client.GenerateNick(generateCtx, small, persona, rejected)
		if err != nil {
			setSpanError(generateSpan, err, observability.ErrorKindDispatch)
			logger.ErrorContext(ctx, "generate nick failed",
				"error", err,
				"attempt", attempt,
			)
			return "", fmt.Errorf("generate nick: %w", err)
		}

		result.Usage.SetSpanAttributes(generateSpan, result.RequestID)

		if !m.nickIsTaken(ctx, sess, result.Nick) {
			generateSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
			return result.Nick, nil
		}

		logger.InfoContext(ctx, "generated nick already in use",
			"nick", result.Nick,
			"attempt", attempt,
		)
		rejected = append(rejected, result.Nick)
	}

	err := fmt.Errorf("generate nick: %d attempts exhausted, all suggestions collided", maxNickGenerationAttempts)
	setSpanError(generateSpan, err, observability.ErrorKindDispatch)
	return "", err
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

func (m *Manager) startSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	tracer := m.tracer.Tracer("github.com/laney/modeloff/internal/modelmanager")
	ctx, span := tracer.Start(ctx, name)
	span.SetAttributes(attrs...)

	return ctx, span
}

// endSpan finalises the span with ok/error status. The fallback
// errorKind is attached as AttrErrorKind when the deferred error
// is non-nil and does not already carry a kind via *kindError.
func endSpan(span trace.Span, errPtr *error, errorKind string) {
	err := *errPtr

	if err == nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
		span.End()
		return
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))

	kind := errorKind
	var ke *kindError
	if errors.As(err, &ke) {
		kind = ke.kind
	}

	if kind != "" {
		span.SetAttributes(attribute.String(observability.AttrErrorKind, kind))
	}

	span.SetStatus(codes.Error, err.Error())
	span.End()
}

func setSpanError(span trace.Span, err error, errorKind string) {
	span.SetAttributes(
		attribute.String(observability.AttrResult, observability.ResultError),
		attribute.String(observability.AttrErrorKind, errorKind),
	)
	span.SetStatus(codes.Error, err.Error())
}

type kindError struct {
	kind string
	err  error
}

func (e *kindError) Error() string { return e.err.Error() }
func (e *kindError) Unwrap() error { return e.err }
