package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
)

// recalledSalienceEffect is when an experience was last cited and where
// that puts it in the ranking.
type recalledSalienceEffect struct {
	LastCitedAt time.Time
	SalienceAt  time.Time
}

// TestCommitPersonaReflection_refreshes_a_recalled_experience pins that
// going back to read an old episode keeps it live.
//
// No reflection removes an experience, so what decides whether it
// reaches a prompt is its salience, and the only thing that raises
// salience after the experience is written is the instance returning to
// the events
// behind it. A recall during a run that never commits changes nothing.
func TestCommitPersonaReflection_refreshes_a_recalled_experience(t *testing.T) {
	ctx := t.Context()
	stored := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance("inst-botty", "botty", "test/model", "quiet", nil)
	require.NoError(t, stored.SaveInstance(ctx, instance))
	base, err := stored.PersonaLineage(ctx, instance.ID())
	require.NoError(t, err)

	firstAt := time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC)
	appendReflectionSources(t, stored, instance.ID(), 2, firstAt)
	first, err := stored.CommitPersonaReflection(ctx, storemod.PersonaReflectionAcceptance{
		RunID: "reflection-first", InstanceID: instance.ID(),
		BaseRevisionID:  base.CurrentRevisionID,
		PriorCheckpoint: 0, HighWaterMark: 1,
		ModelID: "test/reflection", StartedAt: firstAt, FinishedAt: firstAt,
		Experiences: []storemod.PersonaExperienceDraft{{
			Key: "first", Kind: domain.ExperienceObservation,
			Summary:    "A calm discussion ended with a useful answer.",
			Confidence: domain.ConfidenceLow, OccurredAt: firstAt,
			Sources: []domain.ReflectionEventRef{{Sequence: 1}},
		}},
	})
	require.NoError(t, err)

	// A later run reads sequence 1 back through its recall tools and
	// commits.
	laterAt := firstAt.Add(90 * 24 * time.Hour)
	_, err = stored.CommitPersonaReflection(ctx, storemod.PersonaReflectionAcceptance{
		RunID: "reflection-second", InstanceID: instance.ID(),
		BaseRevisionID:  first.Revision.ID,
		PriorCheckpoint: 1, HighWaterMark: 2,
		ModelID: "test/reflection", StartedAt: laterAt, FinishedAt: laterAt,
		RecalledSources: []domain.ReflectionSequence{1},
	})
	require.NoError(t, err)

	snapshot, err := stored.PersonaSnapshot(ctx, instance.ID())
	require.NoError(t, err)
	require.Len(t, snapshot.Experiences, 1)

	require.Equal(t, recalledSalienceEffect{
		LastCitedAt: laterAt,
		SalienceAt:  domain.SalienceAt(domain.ConfidenceLow, laterAt),
	}, recalledSalienceEffect{
		LastCitedAt: snapshot.Experiences[0].LastCitedAt,
		SalienceAt:  snapshot.Experiences[0].SalienceAt,
	})
}

// replayedAmendmentEffect records the amendments a run-id replay
// reports.
type replayedAmendmentEffect struct {
	Amendments []domain.PersonaAmendment
}

// TestCommitPersonaReflection_replays_a_run_as_it_committed pins that
// retrying an accepted commit answers with the state that commit
// produced.
//
// The replay reads the amendment rows as they stand now, and
// consolidation writes to one of them after the fact. Reading that back
// would make the retry report a tendency the earlier run had folded into
// nothing, which is the one thing the run-id replay exists to rule out.
func TestCommitPersonaReflection_replays_a_run_as_it_committed(t *testing.T) {
	ctx := t.Context()
	stored := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance("inst-botty", "botty", "test/model", "quiet", nil)
	require.NoError(t, stored.SaveInstance(ctx, instance))
	base, err := stored.PersonaLineage(ctx, instance.ID())
	require.NoError(t, err)

	firstAt := time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC)
	appendReflectionSources(t, stored, instance.ID(), 2, firstAt)
	original := storemod.PersonaReflectionAcceptance{
		RunID: "reflection-first", InstanceID: instance.ID(),
		BaseRevisionID:  base.CurrentRevisionID,
		PriorCheckpoint: 0, HighWaterMark: 1,
		ModelID: "test/reflection", StartedAt: firstAt, FinishedAt: firstAt,
		Experiences: []storemod.PersonaExperienceDraft{{
			Key: "first", Kind: domain.ExperienceObservation,
			Summary:    "A figure came with its risk attached.",
			Confidence: domain.ConfidenceHigh, OccurredAt: firstAt,
			Sources: []domain.ReflectionEventRef{{Sequence: 1}},
		}},
		Amendments: []storemod.PersonaAmendmentDraft{{
			Scope:        domain.AmendmentGlobal,
			Tendency:     "Usually gives people a figure and its risk.",
			Confidence:   domain.ConfidenceMedium,
			EvidenceKeys: []string{"first"},
		}},
	}
	first, err := stored.CommitPersonaReflection(ctx, original)
	require.NoError(t, err)

	secondAt := firstAt.Add(time.Hour)
	description := "Cares more about being right than about being easy to be around."
	_, err = stored.CommitPersonaReflection(ctx, storemod.PersonaReflectionAcceptance{
		RunID: "reflection-second", InstanceID: instance.ID(),
		BaseRevisionID:  first.Revision.ID,
		PriorCheckpoint: 1, HighWaterMark: 2,
		ModelID: "test/reflection", StartedAt: secondAt, FinishedAt: secondAt,
		Description: &description,
		Consolidate: []domain.PersonaAmendmentID{first.Amendments[0].ID},
	})
	require.NoError(t, err)

	replayed, err := stored.CommitPersonaReflection(ctx, original)
	require.NoError(t, err)

	require.Equal(t, replayedAmendmentEffect{
		Amendments: []domain.PersonaAmendment{{
			ID: first.Amendments[0].ID, InstanceID: instance.ID(),
			Scope:      domain.AmendmentGlobal,
			Tendency:   "Usually gives people a figure and its risk.",
			Confidence: domain.ConfidenceMedium,
			Evidence:   []domain.ExperienceID{1},
			CreatedAt:  firstAt,
		}},
	}, replayedAmendmentEffect{Amendments: replayed.Amendments})
}
