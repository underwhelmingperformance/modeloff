package modelmanager_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/modelmanager"
)

func fakeCatalogue() []api.ModelInfo {
	return []api.ModelInfo{
		{ID: "openai/gpt-5.4-mini", Name: "GPT-5.4 Mini"},
		{ID: "anthropic/claude-3-haiku", Name: "Claude 3 Haiku"},
	}
}

func TestManager_EnsureStructuredOutputModel(t *testing.T) {
	tests := []struct {
		name          string
		apiKey        string
		modelID       domain.ModelID
		wantErr       error
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeAPIClient{listModelsFn: func(context.Context) ([]api.ModelInfo, error) {
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
			client := &fakeAPIClient{listModelsFn: func(context.Context) ([]api.ModelInfo, error) {
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

func TestManager_EmbeddingSearchable_noMemoryStore(t *testing.T) {
	fx := newTestManager(t, modelmanager.Config{})

	searchable, err := fx.mgr.EmbeddingSearchable()
	require.False(t, searchable)
	require.NoError(t, err)
}
