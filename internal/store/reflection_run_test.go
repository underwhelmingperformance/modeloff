package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
)

type reflectionRunEffects struct {
	Run        domain.ReflectionRun
	Checkpoint domain.ReflectionSequence
	Conflict   bool
}

func TestSQLiteStore_records_a_shadow_reflection_without_advancing_state(t *testing.T) {
	stored := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful and curious", nil,
	)
	require.NoError(t, stored.SaveInstance(t.Context(), instance))
	state, err := stored.PersonaLineage(t.Context(), instance.ID())
	require.NoError(t, err)
	startedAt := time.Date(2026, 8, 26, 19, 0, 0, 0, time.UTC)
	run := domain.ReflectionRun{
		ID: "shadow-12", InstanceID: instance.ID(),
		BaseRevisionID:  state.CurrentRevisionID,
		PriorCheckpoint: 0, HighWaterMark: 12,
		ResultRevisionID: state.CurrentRevisionID,
		ModelID:          "test/reflection", Outcome: domain.ReflectionShadow,
		ProposedExperiences: 2, ProposedAmendments: 1,
		StartedAt: startedAt, FinishedAt: startedAt.Add(time.Second),
	}

	require.NoError(t, stored.RecordReflectionRun(t.Context(), run))
	require.NoError(t, stored.RecordReflectionRun(t.Context(), run))
	recorded, err := stored.ReflectionRun(t.Context(), run.ID)
	require.NoError(t, err)
	after, err := stored.PersonaLineage(t.Context(), instance.ID())
	require.NoError(t, err)
	conflict := run
	conflict.Outcome = domain.ReflectionFailed
	conflictErr := stored.RecordReflectionRun(t.Context(), conflict)

	require.Equal(t, reflectionRunEffects{
		Run: run, Checkpoint: state.Checkpoint, Conflict: true,
	}, reflectionRunEffects{
		Run: recorded, Checkpoint: after.Checkpoint,
		Conflict: errors.Is(conflictErr, storemod.ErrReflectionRunConflict),
	})
}
