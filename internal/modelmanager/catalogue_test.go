package modelmanager_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/modelmanager"
)

// fakeCatalogue returns four entries deliberately covering every
// combination of the two independent OpenRouter capabilities the
// manager checks: "tools" (chat-dispatch, EnsureToolCapableModel) and
// "structured_outputs" (nick/persona generation via the small model,
// EnsureStructuredOutputModel). vendor/no-tools-model and
// vendor/tools-only-model each carry exactly one of the two, so a
// test exercising either method against either model proves the two
// checks are genuinely independent, not two names for the same test.
func fakeCatalogue() []api.ModelInfo {
	return []api.ModelInfo{
		{ID: "openai/gpt-5.4-mini", Name: "GPT-5.4 Mini", SupportedParameters: []string{"tools", "structured_outputs", "response_format"}},
		{ID: "anthropic/claude-3-haiku", Name: "Claude 3 Haiku", SupportedParameters: []string{"tools", "structured_outputs", "response_format"}},
		{ID: "vendor/no-tools-model", Name: "No Tools Model", SupportedParameters: []string{"structured_outputs", "response_format"}},
		{ID: "vendor/tools-only-model", Name: "Tools Only Model", SupportedParameters: []string{"tools"}},
	}
}

func TestManager_EnsureToolCapableModel(t *testing.T) {
	tests := []struct {
		name          string
		apiKey        string
		modelID       domain.ModelID
		wantErrTarget any
	}{
		{
			name:    "no API key defers validation",
			apiKey:  "",
			modelID: "typo/model",
		},
		{
			name:    "model present in catalogue passes",
			apiKey:  "sk-test",
			modelID: "openai/gpt-5.4-mini",
		},
		{
			name:          "model absent from catalogue is rejected",
			apiKey:        "sk-test",
			modelID:       "typo/model",
			wantErrTarget: &domain.UnsupportedModelError{},
		},
		{
			name:          "model with structured-output support but no tool support is rejected",
			apiKey:        "sk-test",
			modelID:       "vendor/no-tools-model",
			wantErrTarget: &domain.UnsupportedModelError{},
		},
		{
			name:    "model with only tool support passes",
			apiKey:  "sk-test",
			modelID: "vendor/tools-only-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &apitest.Fake{ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
				return fakeCatalogue(), nil
			}}

			fx := newTestManager(t, modelmanager.Config{
				APIClient:     client,
				InitialAPIKey: tt.apiKey,
			})

			err := fx.mgr.EnsureToolCapableModel(t.Context(), tt.modelID)

			if tt.wantErrTarget == nil {
				require.NoError(t, err)
				return
			}

			require.ErrorAs(t, err, tt.wantErrTarget)
			require.Equal(t, &domain.UnsupportedModelError{ModelID: tt.modelID, At: fixedTime}, tt.wantErrTarget)
		})
	}
}

func TestManager_EnsureStructuredOutputModel(t *testing.T) {
	tests := []struct {
		name          string
		apiKey        string
		modelID       domain.ModelID
		wantErrTarget any
	}{
		{
			name:    "no API key defers validation",
			apiKey:  "",
			modelID: "typo/model",
		},
		{
			name:    "model present in catalogue passes",
			apiKey:  "sk-test",
			modelID: "openai/gpt-5.4-mini",
		},
		{
			name:          "model absent from catalogue is rejected",
			apiKey:        "sk-test",
			modelID:       "typo/model",
			wantErrTarget: &domain.UnsupportedModelError{},
		},
		{
			name:          "model with tool support but no structured-output support is rejected",
			apiKey:        "sk-test",
			modelID:       "vendor/tools-only-model",
			wantErrTarget: &domain.UnsupportedModelError{},
		},
		{
			name:    "model with only structured-output support passes",
			apiKey:  "sk-test",
			modelID: "vendor/no-tools-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &apitest.Fake{ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
				return fakeCatalogue(), nil
			}}

			fx := newTestManager(t, modelmanager.Config{
				APIClient:     client,
				InitialAPIKey: tt.apiKey,
			})

			err := fx.mgr.EnsureStructuredOutputModel(t.Context(), tt.modelID)

			if tt.wantErrTarget == nil {
				require.NoError(t, err)
				return
			}

			require.ErrorAs(t, err, tt.wantErrTarget)
			require.Equal(t, &domain.UnsupportedModelError{ModelID: tt.modelID, At: fixedTime}, tt.wantErrTarget)
		})
	}
}

func TestManager_EnsureKnownModel(t *testing.T) {
	tests := []struct {
		name          string
		apiKey        string
		modelID       domain.ModelID
		wantErrTarget any
	}{
		{
			name:    "no API key defers validation",
			apiKey:  "",
			modelID: "typo/embed",
		},
		{
			name:    "model present in catalogue passes",
			apiKey:  "sk-test",
			modelID: "anthropic/claude-3-haiku",
		},
		{
			name:          "model absent from catalogue is rejected",
			apiKey:        "sk-test",
			modelID:       "typo/embed",
			wantErrTarget: &domain.UnknownModelError{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &apitest.Fake{ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
				return fakeCatalogue(), nil
			}}

			fx := newTestManager(t, modelmanager.Config{
				APIClient:     client,
				InitialAPIKey: tt.apiKey,
			})

			err := fx.mgr.EnsureKnownModel(t.Context(), tt.modelID)

			if tt.wantErrTarget == nil {
				require.NoError(t, err)
				return
			}

			require.ErrorAs(t, err, tt.wantErrTarget)
			require.Equal(t, &domain.UnknownModelError{ModelID: tt.modelID, At: fixedTime}, tt.wantErrTarget)
		})
	}
}

// TestManager_EnsureKnownModel_short_circuits_after_ListModels_failure
// mirrors the existing EnsureStructuredOutputModel short-circuit
// coverage in internal/session: once ListModels has failed, a second
// call does not re-hit the upstream.
func TestManager_EnsureKnownModel_short_circuits_after_ListModels_failure(t *testing.T) {
	client := &listModelsCountingClient{err: fmt.Errorf("upstream 503")}

	fx := newTestManager(t, modelmanager.Config{
		APIClient:     client,
		InitialAPIKey: "sk-test",
	})

	err := fx.mgr.EnsureKnownModel(t.Context(), "any/model")
	require.Error(t, err)
	require.Equal(t, int32(1), client.calls.Load())

	err = fx.mgr.EnsureKnownModel(t.Context(), "any/model")
	require.ErrorIs(t, err, modelclient.ErrModelListUnavailable)
	require.Equal(t, int32(1), client.calls.Load(), "second call must not re-hit ListModels")
}

// TestManager_EnsureKnownModel_selfHeals_after_backoff pins that once
// [modelmanager.CatalogueRetryBackoff] has passed after a catalogue
// failure, the next add-model demand retries the upstream.
func TestManager_EnsureKnownModel_selfHeals_after_backoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &listModelsCountingClient{err: fmt.Errorf("upstream 503")}

		fx := newTestManager(t, modelmanager.Config{
			APIClient:     client,
			InitialAPIKey: "sk-test",
		})
		fx.mgr.SetClock(time.Now)

		err := fx.mgr.EnsureKnownModel(t.Context(), "any/model")
		require.Error(t, err)
		require.Equal(t, int32(1), client.calls.Load())

		// Still inside the backoff window: short-circuits, no re-hit.
		err = fx.mgr.EnsureKnownModel(t.Context(), "any/model")
		require.ErrorIs(t, err, modelclient.ErrModelListUnavailable)
		require.Equal(t, int32(1), client.calls.Load())

		time.Sleep(modelmanager.CatalogueRetryBackoff)
		synctest.Wait()

		client.err = nil
		client.infos = fakeCatalogue()

		err = fx.mgr.EnsureKnownModel(t.Context(), "openai/gpt-5.4-mini")
		require.NoError(t, err)
		require.Equal(t, int32(2), client.calls.Load(), "backoff elapsed: upstream must be retried")
		require.Equal(t, modelmanager.ListStateOK, fx.mgr.ListState())
	})
}

// TestManager_EnsureKnownModel_backoff_restarts_on_repeat_failure pins
// that a second failure resets the backoff window from that failure,
// not from the first one.
func TestManager_EnsureKnownModel_backoff_restarts_on_repeat_failure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &listModelsCountingClient{err: fmt.Errorf("upstream 503")}

		fx := newTestManager(t, modelmanager.Config{
			APIClient:     client,
			InitialAPIKey: "sk-test",
		})
		fx.mgr.SetClock(time.Now)

		require.Error(t, fx.mgr.EnsureKnownModel(t.Context(), "any/model"))
		require.Equal(t, int32(1), client.calls.Load())

		time.Sleep(modelmanager.CatalogueRetryBackoff)
		synctest.Wait()

		// The retry after backoff fails again: the window restarts.
		require.Error(t, fx.mgr.EnsureKnownModel(t.Context(), "any/model"))
		require.Equal(t, int32(2), client.calls.Load())

		time.Sleep(modelmanager.CatalogueRetryBackoff - time.Second)
		synctest.Wait()

		err := fx.mgr.EnsureKnownModel(t.Context(), "any/model")
		require.ErrorIs(t, err, modelclient.ErrModelListUnavailable)
		require.Equal(t, int32(2), client.calls.Load(), "backoff window must restart on repeat failure")
	})
}

// TestManager_ensureCatalogueLoaded_singleFlights_concurrent_calls
// pins that concurrent add-model demands inside the same load
// collapse into one upstream ListModels call: every caller waits on
// the one already in flight and none fires its own.
func TestManager_ensureCatalogueLoaded_singleFlights_concurrent_calls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		block := make(chan struct{})
		client := &listModelsCountingClient{infos: fakeCatalogue(), block: block}

		fx := newTestManager(t, modelmanager.Config{
			APIClient:     client,
			InitialAPIKey: "sk-test",
		})

		const callers = 5

		var wg sync.WaitGroup
		errs := make([]error, callers)

		for i := range callers {
			wg.Go(func() {
				errs[i] = fx.mgr.EnsureKnownModel(t.Context(), "openai/gpt-5.4-mini")
			})
		}

		// Every caller is now durably blocked: the first inside
		// ListModels on `block`, the rest inside awaitCatalogueLoad
		// waiting for the first to finish.
		synctest.Wait()
		require.Equal(t, int32(1), client.calls.Load(), "concurrent callers must not each start their own fetch")

		close(block)
		wg.Wait()

		for _, err := range errs {
			require.NoError(t, err)
		}
		require.Equal(t, int32(1), client.calls.Load(), "still exactly one upstream fetch after every caller returns")
	})
}

func TestManager_EmbeddingSearchable_noMemoryStore(t *testing.T) {
	fx := newTestManager(t, modelmanager.Config{})

	searchable, err := fx.mgr.EmbeddingSearchable()
	require.False(t, searchable)
	require.NoError(t, err)
}
