package modelmanager

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/observability"
)

// CatalogueRetryBackoff is the minimum time
// [Manager.EnsureToolCapableModel], [Manager.EnsureStructuredOutputModel],
// and [Manager.EnsureKnownModel] wait after a failed catalogue fetch
// before the next add-model demand retries the upstream. It bounds
// how long [ListStateFailed] can stay latched after a transient
// outage: without it, every later add-model would short-circuit with
// [modelclient.ErrModelListUnavailable] for as long as the process
// runs, unless a `SetAPIKey` cleared the cache.
const CatalogueRetryBackoff = 30 * time.Second

// ListState reports the manager's view of the cached model
// catalogue. The values let tests assert short-circuit behaviour in
// [Manager.EnsureToolCapableModel] or [Manager.EnsureStructuredOutputModel]
// after an upstream failure or a fresh `SetAPIKey`.
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

// ListModels fetches the live model catalogue from the upstream
// API and caches it. Returns [modelclient.ErrNoAPIKey] when no API
// key is configured.
func (m *Manager) ListModels(ctx context.Context) ([]api.ModelInfo, error) {
	var models []api.ModelInfo

	err := m.inSpan(ctx, "modelmanager.list_models", nil, func(ctx context.Context, _ trace.Span) error {
		client, key := m.snapshotAPI()
		if key == "" || client == nil {
			return errWithKind(modelclient.ErrNoAPIKey, observability.ErrorKindValidation)
		}

		fetched, err := client.ListModels(ctx)
		if err != nil {
			m.transitionListState(ctx, ListStateFailed, err)
			return errWithKind(err, observability.ErrorKindDispatch)
		}

		m.cacheSupportedModels(ctx, fetched)
		models = fetched

		return nil
	})

	return models, err
}

// EnsureToolCapableModel validates that the given model supports
// tool calling, lazy-loading the catalogue if needed. This is the
// capability an invited instance's chat dispatch depends on:
// OpenRouterClient.SendEvents drives every turn through tool calls
// (`msg`, `me`, `pass`, the channel-management and memory tools), so
// a model missing tool support validates but then fails every
// dispatch upstream. Returns [modelclient.ErrModelListUnavailable]
// when the cached state recorded an upstream failure;
// [modelclient.ErrNoAPIKey] when no key is configured (silently: no
// API key means no LLM concerns, so callers ignore the check); or
// [domain.UnsupportedModelError] when the catalogue does not include
// the model, or lists it without OpenRouter's `tools` entry in
// `supported_parameters`.
func (m *Manager) EnsureToolCapableModel(ctx context.Context, modelID domain.ModelID) error {
	client, key := m.snapshotAPI()
	if key == "" || client == nil {
		return nil
	}

	if err := m.ensureCatalogueLoaded(ctx, client, modelID); err != nil {
		return err
	}

	info, ok := m.catalogueLookup(modelID)
	if !ok || !info.SupportsTools() {
		return domain.UnsupportedModelError{ModelID: modelID, At: m.now()}
	}

	return nil
}

// EnsureStructuredOutputModel validates that the given model
// supports strict JSON-schema structured outputs, lazy-loading the
// catalogue if needed. This is the capability the small model's own
// calls depend on: GenerateNick and GeneratePersonas both set a
// strict `json_schema` ResponseFormat and never set Tools, so a
// model missing structured-output support validates but then fails
// every nick or persona call upstream. Returns
// [modelclient.ErrModelListUnavailable] when the cached state
// recorded an upstream failure; [modelclient.ErrNoAPIKey] when no
// key is configured (silently: no API key means no LLM concerns, so
// callers ignore the check); or [domain.UnsupportedModelError] when
// the catalogue does not include the model, or lists it without
// OpenRouter's `structured_outputs` entry in `supported_parameters`.
func (m *Manager) EnsureStructuredOutputModel(ctx context.Context, modelID domain.ModelID) error {
	client, key := m.snapshotAPI()
	if key == "" || client == nil {
		return nil
	}

	if err := m.ensureCatalogueLoaded(ctx, client, modelID); err != nil {
		return err
	}

	info, ok := m.catalogueLookup(modelID)
	if !ok || !info.SupportsStructuredOutputs() {
		return domain.UnsupportedModelError{ModelID: modelID, At: m.now()}
	}

	return nil
}

// EnsureKnownModel validates that modelID appears in the model
// catalogue, lazy-loading it if needed. Unlike
// [Manager.EnsureToolCapableModel] and [Manager.EnsureStructuredOutputModel],
// it makes no claim about the model's capabilities. Callers that need
// tool-calling support (an invited instance's chat dispatch) use
// EnsureToolCapableModel; callers that need strict structured-output
// support (nick and persona generation via the small model) use
// EnsureStructuredOutputModel; this method is for callers that only
// need the id to be real, such as the embedding-model id `/config`
// sets. Returns [modelclient.ErrModelListUnavailable] when the
// cached state recorded an upstream failure, nil when no API key is
// configured (validation is deferred until one exists), or
// [domain.UnknownModelError] when the catalogue does not include the
// model.
func (m *Manager) EnsureKnownModel(ctx context.Context, modelID domain.ModelID) error {
	client, key := m.snapshotAPI()
	if key == "" || client == nil {
		return nil
	}

	if err := m.ensureCatalogueLoaded(ctx, client, modelID); err != nil {
		return err
	}

	if !m.catalogueHas(modelID) {
		return domain.UnknownModelError{ModelID: modelID, At: m.now()}
	}

	return nil
}

// ensureCatalogueLoaded lazy-loads the model catalogue if it has not
// been fetched yet, short-circuiting with
// [modelclient.ErrModelListUnavailable] when the cached state
// recorded an upstream failure less than [CatalogueRetryBackoff] ago.
// Once that window has passed, this call falls through and retries
// the upstream itself, so a transient outage self-heals on the next
// add-model demand. A caller that finds a fetch already in flight
// waits for it via [Manager.awaitCatalogueLoad] and does not start a
// second one, so concurrent add-model demands single-flight into one
// upstream round trip; this recurs every retry window after a
// failure, which is exactly when several demands are likely to land
// together. modelID is logging context only.
func (m *Manager) ensureCatalogueLoaded(ctx context.Context, client api.Client, modelID domain.ModelID) error {
	if ListState(m.state.Load()) == ListStateFailed && !m.catalogueRetryDue() {
		slog.Default().InfoContext(ctx, "add-model short-circuited: model list unavailable",
			"component", "modelmanager",
			"model_id", string(modelID),
		)
		return modelclient.ErrModelListUnavailable
	}

	m.cacheMu.Lock()
	if m.supportedModelsReady {
		m.cacheMu.Unlock()
		return nil
	}

	if inFlight := m.catalogueLoadDone; inFlight != nil {
		m.cacheMu.Unlock()
		return m.awaitCatalogueLoad(ctx, inFlight)
	}

	done := make(chan struct{})
	m.catalogueLoadDone = done
	m.cacheMu.Unlock()

	models, err := client.ListModels(ctx)

	// The cache and state must be fully settled before done closes:
	// a waiter woken by the close reads supportedModelsReady straight
	// away, with no synchronisation of its own to order that read
	// after this goroutine's write.
	if err != nil {
		m.transitionListState(ctx, ListStateFailed, err)
	} else {
		m.cacheSupportedModels(ctx, models)
	}

	m.cacheMu.Lock()
	m.catalogueLoadDone = nil
	m.cacheMu.Unlock()
	close(done)

	if err != nil {
		return fmt.Errorf("list models: %w", err)
	}

	return nil
}

// awaitCatalogueLoad blocks until the in-flight catalogue fetch
// signalled by done completes, then reports its outcome without
// issuing an upstream call of its own. ctx cancellation while
// waiting returns ctx.Err(), independently of how the in-flight
// fetch itself resolves.
func (m *Manager) awaitCatalogueLoad(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}

	m.cacheMu.Lock()
	ready := m.supportedModelsReady
	m.cacheMu.Unlock()

	if ready {
		return nil
	}

	return modelclient.ErrModelListUnavailable
}

// catalogueHas reports whether modelID is present in the cached
// catalogue.
func (m *Manager) catalogueHas(modelID domain.ModelID) bool {
	_, ok := m.catalogueLookup(modelID)
	return ok
}

// catalogueLookup returns the cached catalogue entry for modelID,
// and whether it is present.
func (m *Manager) catalogueLookup(modelID domain.ModelID) (api.ModelInfo, bool) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()

	info, ok := m.supportedModels[modelID]

	return info, ok
}

// CachedContextLen returns the context length the catalogue cache
// last recorded for modelID, or 0 if the model is not in the cache
// (never fetched, or absent from the upstream catalogue). It is a
// pure read of whatever [Manager.EnsureToolCapableModel] last left
// in the cache for the dispatching instance's model id, and never
// itself reaches upstream: a
// model-client's dispatch loop calls this every burst to keep
// [modelclient.ModelClient]'s transcript token budget current with
// the catalogue, and a network round trip on that path would defeat
// the point. The zero return matches [modelclient.ModelClient]'s
// "unknown context length" contract, so a caller that has never seen
// a successful catalogue load is a safe no-op.
func (m *Manager) CachedContextLen(modelID domain.ModelID) int {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()

	return m.supportedModels[modelID].ContextLen
}

// ListState reports the manager's current catalogue state. Tests
// use it to assert the manager's view of the upstream after a
// `ListModels`, `EnsureToolCapableModel`, or `EnsureStructuredOutputModel`
// call.
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

// SupportedModels returns a snapshot of the cached catalogue, keyed
// by model id. The returned map is shared with the cache; callers
// should not mutate it. Tests use this to assert the contents after
// a successful `ListModels`.
func (m *Manager) SupportedModels() map[domain.ModelID]api.ModelInfo {
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
	cache := make(map[domain.ModelID]api.ModelInfo, len(models))
	for _, model := range models {
		cache[model.ID] = model
	}

	m.cacheMu.Lock()
	m.supportedModels = cache
	m.supportedModelsReady = true
	m.cacheMu.Unlock()

	m.transitionListState(ctx, ListStateOK, nil)
}

// catalogueRetryDue reports whether at least [CatalogueRetryBackoff]
// has passed since the catalogue's most recent transition into
// [ListStateFailed].
func (m *Manager) catalogueRetryDue() bool {
	failedAtNano := m.stateFailedAt.Load()
	if failedAtNano == 0 {
		return true
	}

	return m.now().Sub(time.Unix(0, failedAtNano)) >= CatalogueRetryBackoff
}

// transitionListState atomically updates the catalogue state and
// logs the transition so operators can correlate add-model short-
// circuits with the upstream failure that put the catalogue into a
// known-stale state. Every transition into [ListStateFailed] stamps
// `stateFailedAt`, so a second failure restarts the backoff window
// from itself.
func (m *Manager) transitionListState(ctx context.Context, to ListState, err error) {
	from := ListState(m.state.Swap(uint32(to)))

	if to == ListStateFailed {
		m.stateFailedAt.Store(m.now().UnixNano())
	}

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
