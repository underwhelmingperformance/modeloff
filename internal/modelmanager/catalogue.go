package modelmanager

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/observability"
)

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

	if err := m.ensureCatalogueLoaded(ctx, client, modelID); err != nil {
		return err
	}

	if !m.catalogueHas(modelID) {
		return domain.UnsupportedModelError{ModelID: modelID, At: m.now()}
	}

	return nil
}

// EnsureKnownModel validates that modelID appears in the model
// catalogue, lazy-loading it if needed. Unlike
// [Manager.EnsureStructuredOutputModel], it makes no claim about the
// model's capabilities. Callers that need chat-completion or tool-
// call support (nick generation, an invited instance) use
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
// recorded an upstream failure. modelID is logging context only.
func (m *Manager) ensureCatalogueLoaded(ctx context.Context, client api.Client, modelID domain.ModelID) error {
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

	if ready {
		return nil
	}

	models, err := client.ListModels(ctx)
	if err != nil {
		m.transitionListState(ctx, ListStateFailed, err)
		return fmt.Errorf("list models: %w", err)
	}

	m.cacheSupportedModels(ctx, models)

	return nil
}

// catalogueHas reports whether modelID is present in the cached
// catalogue.
func (m *Manager) catalogueHas(modelID domain.ModelID) bool {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()

	_, ok := m.supportedModels[modelID]

	return ok
}

// CachedContextLen returns the context length the catalogue cache
// last recorded for modelID, or 0 if the model is not in the cache
// (never fetched, or absent from the upstream catalogue). It is a
// pure read of whatever [Manager.EnsureStructuredOutputModel] last
// left in the cache and never itself reaches upstream: a
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
