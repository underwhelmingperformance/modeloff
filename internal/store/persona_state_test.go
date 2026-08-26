package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
)

type personaFoundationState struct {
	Lineage  domain.PersonaLineage
	Revision domain.PersonaRevision
}

func TestSQLiteStore_creates_immutable_revision_zero_with_a_model_instance(t *testing.T) {
	store := storetest.NewMemoryStore(t)
	ctx := t.Context()
	instance := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful and curious", nil,
	)

	require.NoError(t, store.SaveInstance(ctx, instance))
	state, err := store.PersonaLineage(ctx, instance.ID())
	require.NoError(t, err)
	revision, err := store.PersonaRevision(ctx, state.CurrentRevisionID)
	require.NoError(t, err)

	want := personaFoundationState{
		Lineage: domain.PersonaLineage{
			InstanceID:        "inst-botty",
			Baseline:          "careful and curious",
			CurrentRevisionID: state.CurrentRevisionID,
			CreatedAt:         time.Time{},
		},
		Revision: domain.PersonaRevision{
			ID:                  state.CurrentRevisionID,
			InstanceID:          "inst-botty",
			Description:         "careful and curious",
			DescriptionEvidence: []domain.ExperienceID{},
			ExperienceIDs:       []domain.ExperienceID{},
			AmendmentIDs:        []domain.PersonaAmendmentID{},
			CreatedAt:           time.Time{},
		},
	}
	require.Equal(t, want, personaFoundationState{Lineage: state, Revision: revision})

	// Reading the lineage alone would pass against an implementation that
	// rewrote revision zero's own description, which is what the name
	// says cannot happen.
	instance.SetPersona("a later display value")
	require.NoError(t, store.SaveInstance(ctx, instance))
	unchangedLineage, err := store.PersonaLineage(ctx, instance.ID())
	require.NoError(t, err)
	unchangedRevision, err := store.PersonaRevision(ctx, state.CurrentRevisionID)
	require.NoError(t, err)

	require.Equal(t, want, personaFoundationState{
		Lineage: unchangedLineage, Revision: unchangedRevision,
	})
}

func TestSQLiteStore_instance_deletion_removes_persona_lineage(t *testing.T) {
	store := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful and curious", nil,
	)
	require.NoError(t, store.SaveInstance(t.Context(), instance))
	base, err := store.PersonaLineage(t.Context(), instance.ID())
	require.NoError(t, err)
	at := time.Date(2026, 8, 26, 11, 0, 0, 0, time.UTC)
	appendReflectionSources(t, store, instance.ID(), 1, at)
	_, err = store.CommitPersonaReflection(t.Context(), storemod.PersonaReflectionAcceptance{
		RunID: "reflection-before-delete", InstanceID: instance.ID(),
		BaseRevisionID:  base.CurrentRevisionID,
		PriorCheckpoint: 0, HighWaterMark: 1,
		ModelID: "test/reflection", StartedAt: at, FinishedAt: at,
		Experiences: []storemod.PersonaExperienceDraft{{
			Key: "event", Kind: domain.ExperienceObservation,
			Summary: "A substantive event occurred.", Confidence: domain.ConfidenceHigh,
			OccurredAt: at, Sources: []domain.ReflectionEventRef{{Sequence: 1}},
		}},
	})
	require.NoError(t, err)
	require.NoError(t, store.DeleteInstanceByID(t.Context(), instance.ID()))

	_, stateErr := store.PersonaLineage(t.Context(), instance.ID())
	require.True(t, errors.Is(stateErr, storemod.ErrNoPersonaLineage))
	transitions, err := store.PersonaTransitions(t.Context(), instance.ID())
	require.NoError(t, err)
	require.Equal(t, []domain.PersonaTransition{}, transitions)
}

func TestSQLiteStore_commits_reflection_state_atomically_and_idempotently(t *testing.T) {
	store := storetest.NewMemoryStore(t)
	ctx := t.Context()
	instance := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful and curious", nil,
	)
	require.NoError(t, store.SaveInstance(ctx, instance))
	base, err := store.PersonaLineage(ctx, instance.ID())
	require.NoError(t, err)

	startedAt := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Second)
	occurredAt := startedAt.Add(-time.Minute)
	appendReflectionSources(t, store, instance.ID(), 3, occurredAt)
	subject := domain.InstanceID("inst-alice")
	acceptance := storemod.PersonaReflectionAcceptance{
		RunID: "reflection-1", InstanceID: instance.ID(),
		BaseRevisionID:  base.CurrentRevisionID,
		PriorCheckpoint: base.Checkpoint, HighWaterMark: 3,
		ModelID: "test/reflection", StartedAt: startedAt, FinishedAt: finishedAt,
		Experiences: []storemod.PersonaExperienceDraft{{
			Key: "patient-correction", Kind: domain.ExperienceRelationship,
			Summary:   "Alice's patient reproduction made the correction easy to trust.",
			SubjectID: &subject, Confidence: domain.ConfidenceHigh,
			OccurredAt: occurredAt,
			Sources:    []domain.ReflectionEventRef{{Sequence: 2}, {Sequence: 3}},
		}},
		Amendments: []storemod.PersonaAmendmentDraft{{
			Scope: domain.AmendmentRelationship, Counterpart: &subject,
			Tendency:     "Usually trusts Alice's technical corrections when they include a reproduction.",
			Confidence:   domain.ConfidenceMedium,
			EvidenceKeys: []string{"patient-correction"},
		}},
	}

	got, err := store.CommitPersonaReflection(ctx, acceptance)
	require.NoError(t, err)
	parent := base.CurrentRevisionID
	want := storemod.PersonaReflectionCommit{
		Lineage: domain.PersonaLineage{
			InstanceID: instance.ID(), Baseline: "careful and curious",
			CurrentRevisionID: got.Revision.ID, Checkpoint: 3,
			CreatedAt: time.Time{}, ReflectedAt: &finishedAt,
		},
		Revision: domain.PersonaRevision{
			ID: got.Revision.ID, InstanceID: instance.ID(), ParentID: &parent,
			Description:         "careful and curious",
			DescriptionEvidence: []domain.ExperienceID{},
			ExperienceIDs:       []domain.ExperienceID{got.Experiences[0].ID},
			AmendmentIDs:        []domain.PersonaAmendmentID{got.Amendments[0].ID},
			CreatedAt:           finishedAt,
		},
		Experiences: []domain.Experience{{
			ID: got.Experiences[0].ID, InstanceID: instance.ID(),
			Kind:      domain.ExperienceRelationship,
			Summary:   "Alice's patient reproduction made the correction easy to trust.",
			SubjectID: &subject, Confidence: domain.ConfidenceHigh,
			OccurredAt: occurredAt, CreatedAt: finishedAt,
			Sources: []domain.ReflectionEventRef{{Sequence: 2}, {Sequence: 3}},
		}},
		Amendments: []domain.PersonaAmendment{{
			ID: got.Amendments[0].ID, InstanceID: instance.ID(),
			Scope: domain.AmendmentRelationship, Counterpart: &subject,
			Tendency:   "Usually trusts Alice's technical corrections when they include a reproduction.",
			Confidence: domain.ConfidenceMedium,
			Evidence:   []domain.ExperienceID{got.Experiences[0].ID},
			CreatedAt:  finishedAt,
		}},
		Run: domain.ReflectionRun{
			ID: "reflection-1", InstanceID: instance.ID(),
			BaseRevisionID:  base.CurrentRevisionID,
			PriorCheckpoint: 0, HighWaterMark: 3,
			ResultRevisionID: got.Revision.ID, ModelID: "test/reflection",
			Outcome:             domain.ReflectionAccepted,
			ProposedExperiences: 1, AcceptedExperiences: 1,
			ProposedAmendments: 1, AcceptedAmendments: 1,
			StartedAt: startedAt, FinishedAt: finishedAt,
		},
	}
	require.Equal(t, want, got)

	repeated := acceptance
	repeated.HighWaterMark = 99
	repeated.Experiences = nil
	repeated.Amendments = nil
	replayed, err := store.CommitPersonaReflection(ctx, repeated)
	require.NoError(t, err)
	require.Equal(t, want, replayed)

	state, err := store.PersonaLineage(ctx, instance.ID())
	require.NoError(t, err)
	revision, err := store.PersonaRevision(ctx, got.Revision.ID)
	require.NoError(t, err)
	require.Equal(t, personaFoundationState{Lineage: want.Lineage, Revision: want.Revision},
		personaFoundationState{Lineage: state, Revision: revision})
}

func TestSQLiteStore_no_change_advances_only_the_reflection_checkpoint(t *testing.T) {
	store := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance("inst-botty", "botty", "test/model", "quiet", nil)
	require.NoError(t, store.SaveInstance(t.Context(), instance))
	base, err := store.PersonaLineage(t.Context(), instance.ID())
	require.NoError(t, err)
	startedAt := time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(time.Second)

	got, err := store.CommitPersonaReflection(t.Context(), storemod.PersonaReflectionAcceptance{
		RunID: "reflection-empty", InstanceID: instance.ID(),
		BaseRevisionID:  base.CurrentRevisionID,
		PriorCheckpoint: base.Checkpoint, HighWaterMark: 12,
		ModelID: "test/reflection", StartedAt: startedAt, FinishedAt: finishedAt,
	})
	require.NoError(t, err)
	want := storemod.PersonaReflectionCommit{
		Lineage: domain.PersonaLineage{
			InstanceID: instance.ID(), Baseline: "quiet",
			CurrentRevisionID: base.CurrentRevisionID, Checkpoint: 12,
			CreatedAt: time.Time{}, ReflectedAt: &finishedAt,
		},
		Revision: domain.PersonaRevision{
			ID: base.CurrentRevisionID, InstanceID: instance.ID(),
			Description:         "quiet",
			DescriptionEvidence: []domain.ExperienceID{},
			ExperienceIDs:       []domain.ExperienceID{},
			AmendmentIDs:        []domain.PersonaAmendmentID{}, CreatedAt: time.Time{},
		},
		Experiences: []domain.Experience{}, Amendments: []domain.PersonaAmendment{},
		Run: domain.ReflectionRun{
			ID: "reflection-empty", InstanceID: instance.ID(),
			BaseRevisionID: base.CurrentRevisionID,
			HighWaterMark:  12, ResultRevisionID: base.CurrentRevisionID,
			ModelID: "test/reflection", Outcome: domain.ReflectionNoChange,
			StartedAt: startedAt, FinishedAt: finishedAt,
		},
	}
	require.Equal(t, want, got)

	_, staleErr := store.CommitPersonaReflection(t.Context(), storemod.PersonaReflectionAcceptance{
		RunID: "reflection-stale", InstanceID: instance.ID(),
		BaseRevisionID:  base.CurrentRevisionID,
		PriorCheckpoint: 0, HighWaterMark: 13,
		ModelID: "test/reflection", StartedAt: startedAt, FinishedAt: finishedAt,
	})
	require.True(t, errors.Is(staleErr, storemod.ErrPersonaLineageChanged))
	unchanged, err := store.PersonaLineage(t.Context(), instance.ID())
	require.NoError(t, err)
	require.Equal(t, want.Lineage, unchanged)
}

// TestSQLiteStore_successive_reflections_accumulate_experiences pins that
// a revision carries its parent's experiences forward alongside the ones
// its own run accepted. A run proposes at most a handful of experiences,
// so without the union an instance's active set would be whatever the
// latest run happened to produce and its history would be unreadable
// after one more reflection.
func TestSQLiteStore_successive_reflections_accumulate_experiences(t *testing.T) {
	store := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance("inst-botty", "botty", "test/model", "quiet", nil)
	require.NoError(t, store.SaveInstance(t.Context(), instance))
	base, err := store.PersonaLineage(t.Context(), instance.ID())
	require.NoError(t, err)

	firstAt := time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC)
	appendReflectionSources(t, store, instance.ID(), 2, firstAt)

	first, err := store.CommitPersonaReflection(t.Context(), storemod.PersonaReflectionAcceptance{
		RunID: "reflection-first", InstanceID: instance.ID(),
		BaseRevisionID:  base.CurrentRevisionID,
		PriorCheckpoint: 0, HighWaterMark: 1,
		ModelID: "test/reflection", StartedAt: firstAt, FinishedAt: firstAt,
		Experiences: []storemod.PersonaExperienceDraft{{
			Key: "first", Kind: domain.ExperienceObservation,
			Summary:    "A calm discussion ended with a useful answer.",
			Confidence: domain.ConfidenceHigh, OccurredAt: firstAt,
			Sources: []domain.ReflectionEventRef{{Sequence: 1}},
		}},
	})
	require.NoError(t, err)

	secondAt := firstAt.Add(time.Hour)
	second, err := store.CommitPersonaReflection(t.Context(), storemod.PersonaReflectionAcceptance{
		RunID: "reflection-second", InstanceID: instance.ID(),
		BaseRevisionID:  first.Revision.ID,
		PriorCheckpoint: 1, HighWaterMark: 2,
		ModelID: "test/reflection", StartedAt: secondAt, FinishedAt: secondAt,
		Experiences: []storemod.PersonaExperienceDraft{{
			Key: "second", Kind: domain.ExperienceInterpretation,
			Summary:    "Asking one more question made the explanation clearer.",
			Confidence: domain.ConfidenceMedium, OccurredAt: secondAt,
			Sources: []domain.ReflectionEventRef{{Sequence: 2}},
		}},
	})
	require.NoError(t, err)

	snapshot, err := store.PersonaSnapshot(t.Context(), instance.ID())
	require.NoError(t, err)

	reflectedAt := secondAt
	require.Equal(t, storemod.PersonaSnapshot{
		Lineage: domain.PersonaLineage{
			InstanceID: instance.ID(), Baseline: "quiet",
			CurrentRevisionID: second.Revision.ID, Checkpoint: 2,
			CreatedAt: time.Time{}, ReflectedAt: &reflectedAt,
		},
		Revision: domain.PersonaRevision{
			ID: second.Revision.ID, InstanceID: instance.ID(),
			ParentID:            &first.Revision.ID,
			Description:         "quiet",
			DescriptionEvidence: []domain.ExperienceID{},
			ExperienceIDs:       []domain.ExperienceID{1, 2},
			AmendmentIDs:        []domain.PersonaAmendmentID{},
			CreatedAt:           secondAt,
		},
		Experiences: []domain.Experience{
			{
				ID: 1, InstanceID: instance.ID(),
				Kind:       domain.ExperienceObservation,
				Summary:    "A calm discussion ended with a useful answer.",
				Confidence: domain.ConfidenceHigh,
				OccurredAt: firstAt, CreatedAt: firstAt,
				Sources: []domain.ReflectionEventRef{{Sequence: 1}},
			},
			{
				ID: 2, InstanceID: instance.ID(),
				Kind:       domain.ExperienceInterpretation,
				Summary:    "Asking one more question made the explanation clearer.",
				Confidence: domain.ConfidenceMedium,
				OccurredAt: secondAt, CreatedAt: secondAt,
				Sources: []domain.ReflectionEventRef{{Sequence: 2}},
			},
		},
		Amendments: []domain.PersonaAmendment{},
	}, snapshot)
}

func TestSQLiteStore_rollback_and_reset_select_exact_immutable_revisions(t *testing.T) {
	store := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance("inst-botty", "botty", "test/model", "quiet", nil)
	require.NoError(t, store.SaveInstance(t.Context(), instance))
	base, err := store.PersonaLineage(t.Context(), instance.ID())
	require.NoError(t, err)
	firstAt := time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC)
	appendReflectionSources(t, store, instance.ID(), 2, firstAt)

	first, err := store.CommitPersonaReflection(t.Context(), storemod.PersonaReflectionAcceptance{
		RunID: "reflection-first", InstanceID: instance.ID(),
		BaseRevisionID:  base.CurrentRevisionID,
		PriorCheckpoint: 0, HighWaterMark: 1,
		ModelID: "test/reflection", StartedAt: firstAt, FinishedAt: firstAt,
		Experiences: []storemod.PersonaExperienceDraft{{
			Key: "first", Kind: domain.ExperienceObservation,
			Summary:    "A calm discussion ended with a useful answer.",
			Confidence: domain.ConfidenceHigh, OccurredAt: firstAt,
			Sources: []domain.ReflectionEventRef{{Sequence: 1}},
		}},
	})
	require.NoError(t, err)
	secondAt := firstAt.Add(time.Hour)
	second, err := store.CommitPersonaReflection(t.Context(), storemod.PersonaReflectionAcceptance{
		RunID: "reflection-second", InstanceID: instance.ID(),
		BaseRevisionID:  first.Revision.ID,
		PriorCheckpoint: 1, HighWaterMark: 2,
		ModelID: "test/reflection", StartedAt: secondAt, FinishedAt: secondAt,
		Experiences: []storemod.PersonaExperienceDraft{{
			Key: "second", Kind: domain.ExperienceInterpretation,
			Summary:    "Asking one more question made the explanation clearer.",
			Confidence: domain.ConfidenceMedium, OccurredAt: secondAt,
			Sources: []domain.ReflectionEventRef{{Sequence: 2}},
		}},
	})
	require.NoError(t, err)

	rollbackAt := secondAt.Add(time.Hour)
	rolledBack, err := store.RollbackPersona(
		t.Context(), instance.ID(), second.Revision.ID, first.Revision.ID, rollbackAt,
	)
	require.NoError(t, err)
	wantRolledBack := first.Lineage
	wantRolledBack.Checkpoint = 2
	wantRolledBack.ReflectedAt = &secondAt
	require.Equal(t, wantRolledBack, rolledBack)
	rolledBackSnapshot, err := store.PersonaSnapshot(t.Context(), instance.ID())
	require.NoError(t, err)
	require.Equal(t, storemod.PersonaSnapshot{
		Lineage: wantRolledBack, Revision: first.Revision,
		Experiences: first.Experiences, Amendments: first.Amendments,
	}, rolledBackSnapshot)

	resetAt := rollbackAt.Add(time.Hour)
	reset, err := store.ResetPersona(
		t.Context(), instance.ID(), first.Revision.ID, resetAt,
	)
	require.NoError(t, err)
	wantReset := wantRolledBack
	wantReset.CurrentRevisionID = base.CurrentRevisionID
	require.Equal(t, wantReset, reset)
	resetSnapshot, err := store.PersonaSnapshot(t.Context(), instance.ID())
	require.NoError(t, err)
	require.Equal(t, storemod.PersonaSnapshot{
		Lineage: wantReset,
		Revision: domain.PersonaRevision{
			ID: base.CurrentRevisionID, InstanceID: instance.ID(),
			Description:         "quiet",
			DescriptionEvidence: []domain.ExperienceID{},
			ExperienceIDs:       []domain.ExperienceID{},
			AmendmentIDs:        []domain.PersonaAmendmentID{}, CreatedAt: time.Time{},
		},
		Experiences: []domain.Experience{}, Amendments: []domain.PersonaAmendment{},
	}, resetSnapshot)

	transitions, err := store.PersonaTransitions(t.Context(), instance.ID())
	require.NoError(t, err)
	require.Equal(t, []domain.PersonaTransition{
		{ID: 1, InstanceID: instance.ID(),
			FromRevisionID: base.CurrentRevisionID, ToRevisionID: first.Revision.ID,
			Kind: domain.PersonaTransitionReflection, At: firstAt},
		{ID: 2, InstanceID: instance.ID(),
			FromRevisionID: first.Revision.ID, ToRevisionID: second.Revision.ID,
			Kind: domain.PersonaTransitionReflection, At: secondAt},
		{ID: 3, InstanceID: instance.ID(),
			FromRevisionID: second.Revision.ID, ToRevisionID: first.Revision.ID,
			Kind: domain.PersonaTransitionRollback, At: rollbackAt},
		{ID: 4, InstanceID: instance.ID(),
			FromRevisionID: first.Revision.ID, ToRevisionID: base.CurrentRevisionID,
			Kind: domain.PersonaTransitionReset, At: resetAt},
	}, transitions)
	recent, err := store.RecentPersonaTransitions(t.Context(), instance.ID(), 2)
	require.NoError(t, err)
	require.Equal(t, []domain.PersonaTransition{
		{ID: 3, InstanceID: instance.ID(),
			FromRevisionID: second.Revision.ID, ToRevisionID: first.Revision.ID,
			Kind: domain.PersonaTransitionRollback, At: rollbackAt},
		{ID: 4, InstanceID: instance.ID(),
			FromRevisionID: first.Revision.ID, ToRevisionID: base.CurrentRevisionID,
			Kind: domain.PersonaTransitionReset, At: resetAt},
	}, recent)
}

func appendReflectionSources(
	t *testing.T,
	stored *storemod.SQLiteStore,
	instanceID domain.InstanceID,
	count int,
	at time.Time,
) {
	t.Helper()

	candidates := make([]storemod.ReflectionEventCandidate, 0, count)
	for sequence := 1; sequence <= count; sequence++ {
		candidates = append(candidates, storemod.ReflectionEventCandidate{
			Source: protocol.ChannelHistoryRef(int64(sequence), "#dev"),
			Message: protocol.IRCMessage{
				Kind:   protocol.KindPrivMsg,
				Source: domain.ClientSource("inst-alice", "alice"),
				Target: "#dev", Body: "reflection evidence", At: at,
			},
			Substantive: true,
		})
	}
	require.NoError(t, stored.AppendReflectionEvents(
		t.Context(), instanceID, candidates, at,
	))
}

// TestSQLiteStore_persona_edit_alone_creates_a_revision covers a reflection
// run whose only proposal is a new description. The prompt reads the active
// revision's description, so a run that created no revision would leave the
// instance speaking under the parent's text and record the outcome as no
// change.
func TestSQLiteStore_persona_edit_alone_creates_a_revision(t *testing.T) {
	stored := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance("inst-botty", "botty", "test/model", "quiet", nil)
	require.NoError(t, stored.SaveInstance(t.Context(), instance))
	base, err := stored.PersonaLineage(t.Context(), instance.ID())
	require.NoError(t, err)

	at := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	edited := "cares more about being right than about being easy to be around"
	commit, err := stored.CommitPersonaReflection(t.Context(), storemod.PersonaReflectionAcceptance{
		RunID: "reflection-persona-only", InstanceID: instance.ID(),
		BaseRevisionID:  base.CurrentRevisionID,
		PriorCheckpoint: 0, HighWaterMark: 1,
		ModelID: "test/reflection", StartedAt: at, FinishedAt: at,
		Description: &edited,
	})
	require.NoError(t, err)

	revisionID := base.CurrentRevisionID + 1
	reflectedAt := at
	require.Equal(t, storemod.PersonaReflectionCommit{
		Lineage: domain.PersonaLineage{
			InstanceID: instance.ID(), Baseline: "quiet",
			CurrentRevisionID: revisionID, Checkpoint: 1,
			CreatedAt: time.Time{}, ReflectedAt: &reflectedAt,
		},
		Revision: domain.PersonaRevision{
			ID: revisionID, InstanceID: instance.ID(),
			ParentID: &base.CurrentRevisionID, Description: edited,
			DescriptionEvidence: []domain.ExperienceID{},
			ExperienceIDs:       []domain.ExperienceID{},
			AmendmentIDs:        []domain.PersonaAmendmentID{},
			CreatedAt:           at,
		},
		Experiences: []domain.Experience{},
		Amendments:  []domain.PersonaAmendment{},
		Run: domain.ReflectionRun{
			ID: "reflection-persona-only", InstanceID: instance.ID(),
			BaseRevisionID: base.CurrentRevisionID, PriorCheckpoint: 0,
			HighWaterMark: 1, ResultRevisionID: revisionID,
			ModelID: "test/reflection", Outcome: domain.ReflectionAccepted,
			StartedAt: at, FinishedAt: at,
		},
	}, commit)
}

// TestSQLiteStore_reset_restores_revision_zeros_description covers the
// description a reset lands on. ResetPersona finds revision zero
// structurally, by its absent parent, so revision zero must carry the
// baseline text for a reset to restore the persona the instance started with.
func TestSQLiteStore_reset_restores_revision_zeros_description(t *testing.T) {
	stored := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance("inst-botty", "botty", "test/model", "quiet", nil)
	require.NoError(t, stored.SaveInstance(t.Context(), instance))
	base, err := stored.PersonaLineage(t.Context(), instance.ID())
	require.NoError(t, err)

	at := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	edited := "cares more about being right than about being easy to be around"
	commit, err := stored.CommitPersonaReflection(t.Context(), storemod.PersonaReflectionAcceptance{
		RunID: "reflection-persona-only", InstanceID: instance.ID(),
		BaseRevisionID:  base.CurrentRevisionID,
		PriorCheckpoint: 0, HighWaterMark: 1,
		ModelID: "test/reflection", StartedAt: at, FinishedAt: at,
		Description: &edited,
	})
	require.NoError(t, err)

	resetAt := at.Add(time.Hour)
	_, err = stored.ResetPersona(
		t.Context(), instance.ID(), commit.Revision.ID, resetAt,
	)
	require.NoError(t, err)
	snapshot, err := stored.PersonaSnapshot(t.Context(), instance.ID())
	require.NoError(t, err)

	reflectedAt := at
	require.Equal(t, storemod.PersonaSnapshot{
		Lineage: domain.PersonaLineage{
			InstanceID: instance.ID(), Baseline: "quiet",
			CurrentRevisionID: base.CurrentRevisionID, Checkpoint: 1,
			CreatedAt: time.Time{}, ReflectedAt: &reflectedAt,
		},
		Revision: domain.PersonaRevision{
			ID: base.CurrentRevisionID, InstanceID: instance.ID(),
			Description:         "quiet",
			DescriptionEvidence: []domain.ExperienceID{},
			ExperienceIDs:       []domain.ExperienceID{},
			AmendmentIDs:        []domain.PersonaAmendmentID{},
			CreatedAt:           time.Time{},
		},
		Experiences: []domain.Experience{},
		Amendments:  []domain.PersonaAmendment{},
	}, snapshot)
}

// TestSQLiteStore_repeating_the_active_description_is_no_change covers a run
// that proposes the description already in force. The revision pointer stays
// where it is, so an unchanged character does not accumulate revisions.
func TestSQLiteStore_repeating_the_active_description_is_no_change(t *testing.T) {
	stored := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance("inst-botty", "botty", "test/model", "quiet", nil)
	require.NoError(t, stored.SaveInstance(t.Context(), instance))
	base, err := stored.PersonaLineage(t.Context(), instance.ID())
	require.NoError(t, err)

	at := time.Date(2026, 8, 27, 11, 0, 0, 0, time.UTC)
	unchanged := "quiet"
	commit, err := stored.CommitPersonaReflection(t.Context(), storemod.PersonaReflectionAcceptance{
		RunID: "reflection-same-persona", InstanceID: instance.ID(),
		BaseRevisionID:  base.CurrentRevisionID,
		PriorCheckpoint: 0, HighWaterMark: 1,
		ModelID: "test/reflection", StartedAt: at, FinishedAt: at,
		Description: &unchanged,
	})
	require.NoError(t, err)

	reflectedAt := at
	require.Equal(t, storemod.PersonaReflectionCommit{
		Lineage: domain.PersonaLineage{
			InstanceID: instance.ID(), Baseline: "quiet",
			CurrentRevisionID: base.CurrentRevisionID, Checkpoint: 1,
			CreatedAt: time.Time{}, ReflectedAt: &reflectedAt,
		},
		Revision: domain.PersonaRevision{
			ID: base.CurrentRevisionID, InstanceID: instance.ID(),
			Description:         "quiet",
			DescriptionEvidence: []domain.ExperienceID{},
			ExperienceIDs:       []domain.ExperienceID{},
			AmendmentIDs:        []domain.PersonaAmendmentID{},
			CreatedAt:           time.Time{},
		},
		Experiences: []domain.Experience{},
		Amendments:  []domain.PersonaAmendment{},
		Run: domain.ReflectionRun{
			ID: "reflection-same-persona", InstanceID: instance.ID(),
			BaseRevisionID: base.CurrentRevisionID, PriorCheckpoint: 0,
			HighWaterMark: 1, ResultRevisionID: base.CurrentRevisionID,
			ModelID: "test/reflection", Outcome: domain.ReflectionNoChange,
			StartedAt: at, FinishedAt: at,
		},
	}, commit)
	transitions, err := stored.PersonaTransitions(t.Context(), instance.ID())
	require.NoError(t, err)
	require.Equal(t, []domain.PersonaTransition{}, transitions)
}

// consolidatedAmendmentEffect pairs the state a consolidation leaves with
// the amendments a rollback past it restores. The rollback is how the
// test reaches the folded amendment's row: the revision decides which
// amendments apply, and the consolidation mark stays on the row either
// way.
type consolidatedAmendmentEffect struct {
	Snapshot   storemod.PersonaSnapshot
	Amendments []domain.PersonaAmendment
}

func TestSQLiteStore_consolidation_drains_the_active_set_and_keeps_the_row(t *testing.T) {
	store := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance("inst-botty", "botty", "test/model", "quiet", nil)
	require.NoError(t, store.SaveInstance(t.Context(), instance))
	base, err := store.PersonaLineage(t.Context(), instance.ID())
	require.NoError(t, err)

	firstAt := time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC)
	appendReflectionSources(t, store, instance.ID(), 2, firstAt)

	first, err := store.CommitPersonaReflection(t.Context(), storemod.PersonaReflectionAcceptance{
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
	})
	require.NoError(t, err)
	folded := first.Amendments[0].ID

	secondAt := firstAt.Add(time.Hour)
	description := "Cares more about being right than about being easy to be around."
	second, err := store.CommitPersonaReflection(t.Context(), storemod.PersonaReflectionAcceptance{
		RunID: "reflection-second", InstanceID: instance.ID(),
		BaseRevisionID:  first.Revision.ID,
		PriorCheckpoint: 1, HighWaterMark: 2,
		ModelID: "test/reflection", StartedAt: secondAt, FinishedAt: secondAt,
		Description: &description,
		Consolidate: []domain.PersonaAmendmentID{folded},
	})
	require.NoError(t, err)

	snapshot, err := store.PersonaSnapshot(t.Context(), instance.ID())
	require.NoError(t, err)
	rolledBackAt := secondAt.Add(time.Hour)
	_, err = store.RollbackPersona(
		t.Context(), instance.ID(),
		second.Revision.ID, first.Revision.ID, rolledBackAt,
	)
	require.NoError(t, err)
	restored, err := store.PersonaSnapshot(t.Context(), instance.ID())
	require.NoError(t, err)

	reflectedAt := secondAt
	require.Equal(t, consolidatedAmendmentEffect{
		Snapshot: storemod.PersonaSnapshot{
			Lineage: domain.PersonaLineage{
				InstanceID: instance.ID(), Baseline: "quiet",
				CurrentRevisionID: second.Revision.ID, Checkpoint: 2,
				ReflectedAt: &reflectedAt,
			},
			Revision: domain.PersonaRevision{
				ID: second.Revision.ID, InstanceID: instance.ID(),
				ParentID:            &first.Revision.ID,
				Description:         description,
				DescriptionEvidence: []domain.ExperienceID{},
				ExperienceIDs:       []domain.ExperienceID{1},
				AmendmentIDs:        []domain.PersonaAmendmentID{},
				CreatedAt:           secondAt,
			},
			Experiences: []domain.Experience{{
				ID: 1, InstanceID: instance.ID(),
				Kind:       domain.ExperienceObservation,
				Summary:    "A figure came with its risk attached.",
				Confidence: domain.ConfidenceHigh,
				OccurredAt: firstAt, CreatedAt: firstAt,
				Sources: []domain.ReflectionEventRef{{Sequence: 1}},
			}},
			Amendments: []domain.PersonaAmendment{},
		},
		Amendments: []domain.PersonaAmendment{{
			ID: folded, InstanceID: instance.ID(),
			Scope:      domain.AmendmentGlobal,
			Tendency:   "Usually gives people a figure and its risk.",
			Confidence: domain.ConfidenceMedium,
			Evidence:   []domain.ExperienceID{1},
			CreatedAt:  firstAt, ConsolidatedAt: &secondAt,
		}},
	}, consolidatedAmendmentEffect{
		Snapshot: snapshot, Amendments: restored.Amendments,
	})
}

// personaDescriptionEvidenceEffect pairs the revision that changed the
// description with the one after it that left the description alone.
type personaDescriptionEvidenceEffect struct {
	Changed   domain.PersonaRevision
	Unchanged domain.PersonaRevision
}

// TestSQLiteStore_a_revision_keeps_the_citations_for_its_description covers
// the audit trail behind an accepted persona change. A description that folds
// in no tendency has no amendment to carry its evidence, so the citations
// belong to the revision itself. A later run that proposes no description
// keeps the text in force and keeps those citations with it, so the
// description in force always names what it was built from.
func TestSQLiteStore_a_revision_keeps_the_citations_for_its_description(t *testing.T) {
	store := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance("inst-botty", "botty", "test/model", "quiet", nil)
	require.NoError(t, store.SaveInstance(t.Context(), instance))
	base, err := store.PersonaLineage(t.Context(), instance.ID())
	require.NoError(t, err)

	firstAt := time.Date(2026, 8, 27, 14, 0, 0, 0, time.UTC)
	appendReflectionSources(t, store, instance.ID(), 3, firstAt)

	description := "Cares more about being right than about being easy to be around."
	changed, err := store.CommitPersonaReflection(t.Context(), storemod.PersonaReflectionAcceptance{
		RunID: "reflection-described", InstanceID: instance.ID(),
		BaseRevisionID:  base.CurrentRevisionID,
		PriorCheckpoint: 0, HighWaterMark: 2,
		ModelID: "test/reflection", StartedAt: firstAt, FinishedAt: firstAt,
		Description:             &description,
		DescriptionEvidenceKeys: []string{"correction", "pushback"},
		Experiences: []storemod.PersonaExperienceDraft{
			{
				Key: "correction", Kind: domain.ExperienceObservation,
				Summary:    "A correction landed better than a softer answer.",
				Confidence: domain.ConfidenceHigh, OccurredAt: firstAt,
				Sources: []domain.ReflectionEventRef{{Sequence: 1}},
			},
			{
				Key: "pushback", Kind: domain.ExperienceObservation,
				Summary:    "Pushback on a figure produced a better one.",
				Confidence: domain.ConfidenceMedium, OccurredAt: firstAt,
				Sources: []domain.ReflectionEventRef{{Sequence: 2}},
			},
		},
	})
	require.NoError(t, err)

	secondAt := firstAt.Add(time.Hour)
	unchanged, err := store.CommitPersonaReflection(t.Context(), storemod.PersonaReflectionAcceptance{
		RunID: "reflection-tendency-only", InstanceID: instance.ID(),
		BaseRevisionID:  changed.Revision.ID,
		PriorCheckpoint: 2, HighWaterMark: 3,
		ModelID: "test/reflection", StartedAt: secondAt, FinishedAt: secondAt,
		Amendments: []storemod.PersonaAmendmentDraft{{
			Scope:       domain.AmendmentGlobal,
			Tendency:    "Usually says the awkward thing early.",
			Confidence:  domain.ConfidenceMedium,
			EvidenceIDs: []domain.ExperienceID{1},
		}},
	})
	require.NoError(t, err)

	require.Equal(t, personaDescriptionEvidenceEffect{
		Changed: domain.PersonaRevision{
			ID: changed.Revision.ID, InstanceID: instance.ID(),
			ParentID:            &base.CurrentRevisionID,
			Description:         description,
			DescriptionEvidence: []domain.ExperienceID{1, 2},
			ExperienceIDs:       []domain.ExperienceID{1, 2},
			AmendmentIDs:        []domain.PersonaAmendmentID{},
			CreatedAt:           firstAt,
		},
		Unchanged: domain.PersonaRevision{
			ID: unchanged.Revision.ID, InstanceID: instance.ID(),
			ParentID:            &changed.Revision.ID,
			Description:         description,
			DescriptionEvidence: []domain.ExperienceID{1, 2},
			ExperienceIDs:       []domain.ExperienceID{1, 2},
			AmendmentIDs:        []domain.PersonaAmendmentID{1},
			CreatedAt:           secondAt,
		},
	}, personaDescriptionEvidenceEffect{
		Changed: changed.Revision, Unchanged: unchanged.Revision,
	})
}

// operatorDescriptionEffect pairs the state after an operator edit with the
// revision it wrote and the transitions behind it.
type operatorDescriptionEffect struct {
	Lineage     domain.PersonaLineage
	Revision    domain.PersonaRevision
	Transitions []domain.PersonaTransition
	Repeated    domain.PersonaLineage
}

// TestSQLiteStore_an_operator_description_is_a_revision_in_the_same_lineage
// covers an operator giving an instance a replacement description. It lands
// as a child of the active revision, carrying the experiences and tendencies
// in force forward and citing nothing, so a rollback reaches it like any
// other revision. Writing the description already in force writes no
// revision at all.
func TestSQLiteStore_an_operator_description_is_a_revision_in_the_same_lineage(t *testing.T) {
	store := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance("inst-botty", "botty", "test/model", "quiet", nil)
	require.NoError(t, store.SaveInstance(t.Context(), instance))
	base, err := store.PersonaLineage(t.Context(), instance.ID())
	require.NoError(t, err)

	reflectedAt := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	appendReflectionSources(t, store, instance.ID(), 1, reflectedAt)
	reflected, err := store.CommitPersonaReflection(t.Context(), storemod.PersonaReflectionAcceptance{
		RunID: "reflection-before-the-edit", InstanceID: instance.ID(),
		BaseRevisionID:  base.CurrentRevisionID,
		PriorCheckpoint: 0, HighWaterMark: 1,
		ModelID: "test/reflection", StartedAt: reflectedAt, FinishedAt: reflectedAt,
		Experiences: []storemod.PersonaExperienceDraft{{
			Key: "reproduction", Kind: domain.ExperienceObservation,
			Summary:    "Alice supplied a reproduction.",
			Confidence: domain.ConfidenceHigh, OccurredAt: reflectedAt,
			Sources: []domain.ReflectionEventRef{{Sequence: 1}},
		}},
		Amendments: []storemod.PersonaAmendmentDraft{{
			Scope:        domain.AmendmentGlobal,
			Tendency:     "Usually asks for a reproduction.",
			Confidence:   domain.ConfidenceMedium,
			EvidenceKeys: []string{"reproduction"},
		}},
	})
	require.NoError(t, err)

	editedAt := reflectedAt.Add(time.Hour)
	written := "cares more about being right than about being easy to be around"
	state, err := store.WritePersonaDescription(
		t.Context(), instance.ID(), reflected.Revision.ID, written, editedAt,
	)
	require.NoError(t, err)
	snapshot, err := store.PersonaSnapshot(t.Context(), instance.ID())
	require.NoError(t, err)
	repeated, err := store.WritePersonaDescription(
		t.Context(), instance.ID(), state.CurrentRevisionID, written,
		editedAt.Add(time.Hour),
	)
	require.NoError(t, err)
	transitions, err := store.PersonaTransitions(t.Context(), instance.ID())
	require.NoError(t, err)

	edited := reflected.Revision.ID + 1
	require.Equal(t, operatorDescriptionEffect{
		Lineage: domain.PersonaLineage{
			InstanceID: instance.ID(), Baseline: "quiet",
			CurrentRevisionID: edited, Checkpoint: 1,
			ReflectedAt: &reflectedAt,
		},
		Revision: domain.PersonaRevision{
			ID: edited, InstanceID: instance.ID(),
			ParentID:            &reflected.Revision.ID,
			Description:         written,
			DescriptionEvidence: []domain.ExperienceID{},
			ExperienceIDs:       []domain.ExperienceID{1},
			AmendmentIDs:        []domain.PersonaAmendmentID{1},
			CreatedAt:           editedAt,
		},
		Transitions: []domain.PersonaTransition{
			{
				ID: 1, InstanceID: instance.ID(),
				FromRevisionID: base.CurrentRevisionID,
				ToRevisionID:   reflected.Revision.ID,
				Kind:           domain.PersonaTransitionReflection, At: reflectedAt,
			},
			{
				ID: 2, InstanceID: instance.ID(),
				FromRevisionID: reflected.Revision.ID, ToRevisionID: edited,
				Kind: domain.PersonaTransitionOperator, At: editedAt,
			},
		},
		Repeated: domain.PersonaLineage{
			InstanceID: instance.ID(), Baseline: "quiet",
			CurrentRevisionID: edited, Checkpoint: 1,
			ReflectedAt: &reflectedAt,
		},
	}, operatorDescriptionEffect{
		Lineage: state, Revision: snapshot.Revision,
		Transitions: transitions, Repeated: repeated,
	})
}

// TestSQLiteStore_an_operator_description_refuses_a_stale_revision covers
// two edits against the same revision. Each commits against the revision it
// read, so the second is refused instead of overwriting the first.
func TestSQLiteStore_an_operator_description_refuses_a_stale_revision(t *testing.T) {
	store := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance("inst-botty", "botty", "test/model", "quiet", nil)
	require.NoError(t, store.SaveInstance(t.Context(), instance))
	base, err := store.PersonaLineage(t.Context(), instance.ID())
	require.NoError(t, err)

	at := time.Date(2026, 8, 28, 11, 0, 0, 0, time.UTC)
	_, err = store.WritePersonaDescription(
		t.Context(), instance.ID(), base.CurrentRevisionID, "terse", at,
	)
	require.NoError(t, err)

	_, err = store.WritePersonaDescription(
		t.Context(), instance.ID(), base.CurrentRevisionID, "chatty",
		at.Add(time.Minute),
	)

	require.ErrorIs(t, err, storemod.ErrPersonaLineageChanged)
}

// TestSQLiteStore_an_operator_description_refuses_a_revision_a_reflection_moved
// covers the case the sequential test does not: an edit whose revision a
// reflection has already replaced.
//
// The edit compares only the revision, and a reflection that changes the
// description moves it, so the edit is refused. A reflection that changes
// nothing else still advances the checkpoint, which the edit does not read,
// so those two can both land; the compare-and-swap the edit runs under is
// not the one a reflection runs under.
func TestSQLiteStore_an_operator_description_refuses_a_revision_a_reflection_moved(t *testing.T) {
	ctx := t.Context()
	store := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance("inst-botty", "botty", "test/model", "quiet", nil)
	require.NoError(t, store.SaveInstance(ctx, instance))
	base, err := store.PersonaLineage(ctx, instance.ID())
	require.NoError(t, err)

	at := time.Date(2026, 8, 28, 11, 0, 0, 0, time.UTC)
	appendReflectionSources(t, store, instance.ID(), 1, at)
	description := "Cares more about being right than about being easy to be around."
	_, err = store.CommitPersonaReflection(ctx, storemod.PersonaReflectionAcceptance{
		RunID: "reflection-before-the-edit", InstanceID: instance.ID(),
		BaseRevisionID:  base.CurrentRevisionID,
		PriorCheckpoint: 0, HighWaterMark: 1,
		ModelID: "test/reflection", StartedAt: at, FinishedAt: at,
		Description: &description,
		Experiences: []storemod.PersonaExperienceDraft{{
			Key: "reproduction", Kind: domain.ExperienceObservation,
			Summary:    "Alice supplied a reproduction.",
			Confidence: domain.ConfidenceHigh, OccurredAt: at,
			Sources: []domain.ReflectionEventRef{{Sequence: 1}},
		}},
		DescriptionEvidenceKeys: []string{"reproduction"},
	})
	require.NoError(t, err)

	_, err = store.WritePersonaDescription(
		ctx, instance.ID(), base.CurrentRevisionID, "terse", at.Add(time.Minute),
	)

	require.ErrorIs(t, err, storemod.ErrPersonaLineageChanged)
}
