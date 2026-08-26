package store_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
)

const testReflectionRunRetention = 100

func TestSQLiteStore_bounds_reflection_run_diagnostics_per_instance(t *testing.T) {
	stored := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful", nil,
	)
	require.NoError(t, stored.SaveInstance(t.Context(), instance))
	state, err := stored.PersonaLineage(t.Context(), instance.ID())
	require.NoError(t, err)
	startedAt := time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)

	for index := 1; index <= testReflectionRunRetention+1; index++ {
		at := startedAt.Add(time.Duration(index) * time.Second)
		require.NoError(t, stored.RecordReflectionRun(t.Context(), domain.ReflectionRun{
			ID:         domain.ReflectionRunID(fmt.Sprintf("run-%03d", index)),
			InstanceID: instance.ID(), BaseRevisionID: state.CurrentRevisionID,
			ResultRevisionID: state.CurrentRevisionID,
			ModelID:          "test/reflection", Outcome: domain.ReflectionFailed,
			RejectionReason: "upstream_failed", StartedAt: at, FinishedAt: at,
		}))
	}

	got, err := stored.ReflectionRuns(
		t.Context(), instance.ID(), testReflectionRunRetention+10,
	)
	require.NoError(t, err)
	want := make([]domain.ReflectionRun, 0, testReflectionRunRetention)
	for index := testReflectionRunRetention + 1; index >= 2; index-- {
		at := startedAt.Add(time.Duration(index) * time.Second)
		want = append(want, domain.ReflectionRun{
			ID:         domain.ReflectionRunID(fmt.Sprintf("run-%03d", index)),
			InstanceID: instance.ID(), BaseRevisionID: state.CurrentRevisionID,
			ResultRevisionID: state.CurrentRevisionID,
			ModelID:          "test/reflection", Outcome: domain.ReflectionFailed,
			RejectionReason: "upstream_failed", StartedAt: at, FinishedAt: at,
		})
	}
	require.Equal(t, want, got)
}

type reflectionRunEffects struct {
	Run        domain.ReflectionRun
	Checkpoint domain.ReflectionSequence
	Conflict   bool
}

type recentReflectionRunsEffect struct {
	Runs []domain.ReflectionRun
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

func TestSQLiteStore_lists_recent_reflection_runs_for_one_instance(t *testing.T) {
	stored := storetest.NewMemoryStore(t)
	botty := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful and curious", nil,
	)
	alice := domain.NewModelInstance(
		"inst-alice", "alice", "test/model", "direct and patient", nil,
	)
	require.NoError(t, stored.SaveInstance(t.Context(), botty))
	require.NoError(t, stored.SaveInstance(t.Context(), alice))
	bottyState, err := stored.PersonaLineage(t.Context(), botty.ID())
	require.NoError(t, err)
	aliceState, err := stored.PersonaLineage(t.Context(), alice.ID())
	require.NoError(t, err)
	startedAt := time.Date(2026, 8, 27, 14, 0, 0, 0, time.UTC)
	first := domain.ReflectionRun{
		ID: "botty-1", InstanceID: botty.ID(),
		BaseRevisionID:   bottyState.CurrentRevisionID,
		ResultRevisionID: bottyState.CurrentRevisionID,
		ModelID:          "test/reflection", Outcome: domain.ReflectionShadow,
		StartedAt: startedAt, FinishedAt: startedAt.Add(time.Second),
	}
	second := first
	second.ID = "botty-2"
	second.StartedAt = startedAt.Add(time.Minute)
	second.FinishedAt = startedAt.Add(time.Minute + time.Second)
	third := first
	third.ID = "botty-3"
	third.StartedAt = startedAt.Add(2 * time.Minute)
	third.FinishedAt = startedAt.Add(2*time.Minute + time.Second)
	aliceRun := domain.ReflectionRun{
		ID: "alice-1", InstanceID: alice.ID(),
		BaseRevisionID:   aliceState.CurrentRevisionID,
		ResultRevisionID: aliceState.CurrentRevisionID,
		ModelID:          "test/reflection", Outcome: domain.ReflectionFailed,
		StartedAt:  startedAt.Add(3 * time.Minute),
		FinishedAt: startedAt.Add(3*time.Minute + time.Second),
	}
	for _, run := range []domain.ReflectionRun{first, second, third, aliceRun} {
		require.NoError(t, stored.RecordReflectionRun(t.Context(), run))
	}

	runs, err := stored.ReflectionRuns(t.Context(), botty.ID(), 2)
	require.NoError(t, err)

	require.Equal(t, recentReflectionRunsEffect{
		Runs: []domain.ReflectionRun{third, second},
	}, recentReflectionRunsEffect{Runs: runs})
}
