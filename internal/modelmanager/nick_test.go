package modelmanager_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelmanager"
	"github.com/laney/modeloff/internal/session"
)

// newTestSession builds a bare [session.Session] over fx's store,
// using fx's manager as its [session.ModelClientFactory]. Nothing
// is subscribed: [modelmanager.Manager.PrepareInstance] only calls
// [session.Session.ResolveNick], which reads the store directly, so
// no user-client or model-client attach is needed for these tests.
func newTestSession(t *testing.T, fx *managerFixture) *session.Session {
	t.Helper()

	sess := session.New(t.Context, fx.store, fx.mgr, nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	return sess
}

// toolsCatalogue returns a fake ListModels response listing modelID
// with tool-calling support, which
// [modelmanager.Manager.EnsureToolCapableModel] requires before nick
// generation runs.
func toolsCatalogue(modelID domain.ModelID) []api.ModelInfo {
	return []api.ModelInfo{
		{ID: modelID, Name: string(modelID), SupportedParameters: []string{"tools"}},
	}
}

// TestManager_PrepareInstance_nickGeneration_fallsBackToDeterministicNick
// covers the ways nick generation can fail to produce a usable nick:
// an upstream transport error, no API client configured at all, and
// every LLM suggestion colliding with a taken nick. In each case
// PrepareInstance still returns a nick, derived deterministically
// from the model id.
func TestManager_PrepareInstance_nickGeneration_fallsBackToDeterministicNick(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	tests := []struct {
		name         string
		apiKey       string
		withClient   bool
		generateNick func(context.Context, domain.ModelID, string, []domain.Nick) (domain.Nick, error)
		alreadyTaken []domain.Nick
		wantNick     domain.Nick
	}{
		{
			name:       "transport error falls back",
			apiKey:     "sk-test",
			withClient: true,
			generateNick: func(context.Context, domain.ModelID, string, []domain.Nick) (domain.Nick, error) {
				return "", fmt.Errorf("small-model upstream unreachable")
			},
			wantNick: "gpt-5-4",
		},
		{
			name:       "no API client at all falls back",
			apiKey:     "",
			withClient: false,
			wantNick:   "gpt-5-4",
		},
		{
			name:       "every LLM suggestion collides, falls back after exhausting attempts",
			apiKey:     "sk-test",
			withClient: true,
			generateNick: func(context.Context, domain.ModelID, string, []domain.Nick) (domain.Nick, error) {
				return "taken", nil
			},
			alreadyTaken: []domain.Nick{"taken"},
			wantNick:     "gpt-5-4",
		},
		{
			name:       "every LLM suggestion fails the nick grammar, falls back after exhausting attempts",
			apiKey:     "sk-test",
			withClient: true,
			generateNick: func(context.Context, domain.ModelID, string, []domain.Nick) (domain.Nick, error) {
				return "1bot", nil
			},
			wantNick: "gpt-5-4",
		},
		{
			name:       "every LLM suggestion is the reserved anonymous nick, falls back after exhausting attempts",
			apiKey:     "sk-test",
			withClient: true,
			generateNick: func(context.Context, domain.ModelID, string, []domain.Nick) (domain.Nick, error) {
				return "anonymous", nil
			},
			wantNick: "gpt-5-4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := modelmanager.Config{InitialAPIKey: tt.apiKey}
			if tt.withClient {
				cfg.APIClient = &apitest.Fake{
					ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
						return toolsCatalogue(modelID), nil
					},
					GenerateNickFn: tt.generateNick,
				}
			}

			fx := newTestManager(t, cfg)

			ctx := t.Context()
			for _, nick := range tt.alreadyTaken {
				inst := domain.NewModelInstance(domain.GenerateInstanceID(), nick, "other/model", "", nil)
				require.NoError(t, fx.store.SaveInstance(ctx, inst))
			}

			sess := newTestSession(t, fx)

			prepared, err := fx.mgr.PrepareInstance(ctx, sess, modelID, "")
			require.NoError(t, err)
			require.Equal(t, tt.wantNick, prepared.Nick)
		})
	}
}

// TestManager_PrepareInstance_nickGeneration_retriesPastAnInvalidSuggestion
// pins that a suggestion failing [domain.ValidateNick] is retried the
// same way a colliding one is, rather than being handed straight to
// the caller (which would surface as an [domain.ErroneousNicknameError]
// when the session came to claim it) or being asked for again
// unchanged.
func TestManager_PrepareInstance_nickGeneration_retriesPastAnInvalidSuggestion(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	var seenExclusions [][]domain.Nick

	client := &apitest.Fake{
		ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
			return toolsCatalogue(modelID), nil
		},
		GenerateNickFn: func(_ context.Context, _ domain.ModelID, _ string, exclude []domain.Nick) (domain.Nick, error) {
			seenExclusions = append(seenExclusions, append([]domain.Nick(nil), exclude...))
			if len(exclude) == 0 {
				return "1bad", nil
			}

			return "goodnick", nil
		},
	}

	fx := newTestManager(t, modelmanager.Config{
		APIClient:     client,
		InitialAPIKey: "sk-test",
	})
	sess := newTestSession(t, fx)

	prepared, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "")
	require.NoError(t, err)
	require.Equal(t, domain.Nick("goodnick"), prepared.Nick)
	require.Equal(t, [][]domain.Nick{nil, {"1bad"}}, seenExclusions,
		"the invalid suggestion must be carried into the next attempt's exclusion list, the same as a collision")
}

// TestManager_PrepareInstance_deterministicFallback_avoidsCollision
// pins that the deterministic fallback checks uniqueness itself: if
// the bare model-id-derived nick is already taken, it appends a
// numeric suffix (matching the familiar IRC-client convention of
// "nick", "nick2", "nick3", ...) until it finds a free one.
func TestManager_PrepareInstance_deterministicFallback_avoidsCollision(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	client := &apitest.Fake{
		ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
			return toolsCatalogue(modelID), nil
		},
		GenerateNickFn: func(context.Context, domain.ModelID, string, []domain.Nick) (domain.Nick, error) {
			return "", fmt.Errorf("small-model upstream unreachable")
		},
	}

	fx := newTestManager(t, modelmanager.Config{
		APIClient:     client,
		InitialAPIKey: "sk-test",
	})

	ctx := t.Context()
	held := domain.NewModelInstance(domain.GenerateInstanceID(), "gpt-5-4", "other/model", "", nil)
	require.NoError(t, fx.store.SaveInstance(ctx, held))

	sess := newTestSession(t, fx)

	prepared, err := fx.mgr.PrepareInstance(ctx, sess, modelID, "")
	require.NoError(t, err)
	require.Equal(t, domain.Nick("gpt-5-42"), prepared.Nick)
}
