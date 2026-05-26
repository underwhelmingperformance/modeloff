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
