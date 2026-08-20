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
)

// TestManager_PrepareInstance_nickGeneration_prefers_NickReasonGenerator
// covers the connection point item 9 adds: a Client that implements
// the optional api.NickReasonGenerator capability is asked through it
// instead of plain GenerateNick, and each retry carries the specific
// reason the previous suggestion was rejected. A grammar failure is
// worded differently from a plain collision, rather than one fixed
// "already taken" sentence for both.
func TestManager_PrepareInstance_nickGeneration_prefers_NickReasonGenerator(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	var seenExclusions [][]api.RejectedNick

	client := &apitest.ReasonAware{
		Fake: apitest.Fake{
			ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
				return toolsCatalogue(modelID), nil
			},
		},
		GenerateNickWithReasonsFn: func(_ context.Context, _ domain.ModelID, _ string, excluded []api.RejectedNick) (domain.Nick, error) {
			seenExclusions = append(seenExclusions, append([]api.RejectedNick(nil), excluded...))

			switch len(excluded) {
			case 0:
				return "1bad", nil
			case 1:
				return "taken", nil
			default:
				return "goodnick", nil
			}
		},
	}

	fx := newTestManager(t, modelmanager.Config{
		APIClient:     client,
		InitialAPIKey: "sk-test",
	})

	ctx := t.Context()
	held := domain.NewModelInstance(domain.GenerateInstanceID(), "taken", "other/model", "", nil)
	require.NoError(t, fx.store.SaveInstance(ctx, held))

	sess := newTestSession(t, fx)

	prepared, err := fx.mgr.PrepareInstance(ctx, sess, modelID, "")
	require.NoError(t, err)
	require.Equal(t, domain.Nick("goodnick"), prepared.Nick)

	require.Equal(t, [][]api.RejectedNick{
		nil,
		{{Nick: "1bad", Reason: fmt.Sprintf("doesn't satisfy the nick grammar: %s", domain.NickBadFirstCharacter.String())}},
		{
			{Nick: "1bad", Reason: fmt.Sprintf("doesn't satisfy the nick grammar: %s", domain.NickBadFirstCharacter.String())},
			{Nick: "taken", Reason: "is already taken"},
		},
	}, seenExclusions)
}

// TestManager_PrepareInstance_nickGeneration_falls_back_without_NickReasonGenerator
// pins the other side: a Client that only implements plain
// api.Client (every fake elsewhere in this package, and every
// provider besides OpenRouterClient) is still asked through
// GenerateNick, unchanged.
func TestManager_PrepareInstance_nickGeneration_falls_back_without_NickReasonGenerator(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	var seenExclusions [][]domain.Nick

	client := &apitest.Fake{
		ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
			return toolsCatalogue(modelID), nil
		},
		GenerateNickFn: func(_ context.Context, _ domain.ModelID, _ string, exclude []domain.Nick) (domain.Nick, error) {
			seenExclusions = append(seenExclusions, append([]domain.Nick(nil), exclude...))

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
	require.Equal(t, [][]domain.Nick{nil}, seenExclusions)
}
