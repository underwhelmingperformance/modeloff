package modelmanager

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
)

type reflectionRunRange struct {
	Checkpoint    domain.ReflectionSequence
	Through       domain.ReflectionSequence
	HighWaterMark domain.ReflectionSequence
	Sequences     []domain.ReflectionSequence
}

type reflectionWorkerState struct {
	Runs    []reflectionRunRange
	Workers int
}

type managerReflectionEffect struct {
	Run    reflectionRunRange
	Status store.ReflectionInboxStatus
}

type reflectionDisableEffect struct {
	StartedMode ReflectionMode
	StoppedMode ReflectionMode
	ModelID     domain.ModelID
	Cancelled   bool
	Before      store.PersonaSnapshot
	After       store.PersonaSnapshot
}

type recordingReflectionStore struct {
	*store.SQLiteStore

	recorded chan domain.ReflectionRun
}

func (s *recordingReflectionStore) RecordReflectionRun(
	ctx context.Context,
	run domain.ReflectionRun,
) error {
	if err := s.SQLiteStore.RecordReflectionRun(ctx, run); err != nil {
		return err
	}
	s.recorded <- run

	return nil
}

type reflectionModeEscalationEffect struct {
	Run    domain.ReflectionRun
	Before store.PersonaSnapshot
	After  store.PersonaSnapshot
}

func TestManager_reflection_mode_starts_and_stops_scheduled_work(t *testing.T) {
	stored, instance := reflectionSchedulerStore(t)
	fixed := time.Date(2026, 8, 27, 16, 0, 0, 0, time.UTC)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	client := &apitest.Fake{
		ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
			return []api.ModelInfo{{
				ID:                  "test/reflection",
				SupportedParameters: []string{"tools", "structured_outputs"},
			}}, nil
		},
		ReflectPersonaFn: func(
			ctx context.Context,
			_ domain.ModelID,
			_ domain.InstanceID,
			_ api.ReflectionInput,
			_ ...api.ToolDefinition,
		) (api.ReflectionExploration, error) {
			close(started)
			<-ctx.Done()
			close(cancelled)

			return api.ReflectionExploration{}, ctx.Err()
		},
	}
	manager := New(Config{
		Store: stored, APIClient: client, InitialAPIKey: "configured",
		BaseContext: t.Context, Now: func() time.Time { return fixed },
		ReflectionModel: "test/reflection",
	})
	before, err := stored.PersonaSnapshot(t.Context(), instance.ID())
	require.NoError(t, err)
	require.NoError(t, manager.SetReflectionMode(t.Context(), ReflectionActive))
	startedMode, modelID := manager.ReflectionSettings()
	require.NoError(t, manager.AppendReflectionEvents(
		t.Context(), instance.ID(),
		reflectionCandidates(1, reflectionSubstantiveThreshold, fixed), fixed,
	))
	<-started
	require.NoError(t, manager.SetReflectionMode(t.Context(), ReflectionDisabled))
	<-cancelled
	stoppedMode, _ := manager.ReflectionSettings()
	after, err := stored.PersonaSnapshot(t.Context(), instance.ID())
	require.NoError(t, err)

	require.Equal(t, reflectionDisableEffect{
		StartedMode: ReflectionActive, StoppedMode: ReflectionDisabled,
		ModelID: "test/reflection", Cancelled: true, Before: before, After: before,
	}, reflectionDisableEffect{
		StartedMode: startedMode, StoppedMode: stoppedMode, ModelID: modelID,
		Cancelled: true, Before: before, After: after,
	})
	require.NoError(t, manager.DetachAll(t.Context()))
}

func TestManager_does_not_activate_a_reflection_started_in_shadow_mode(t *testing.T) {
	backing, instance := reflectionSchedulerStore(t)
	stored := &recordingReflectionStore{
		SQLiteStore: backing, recorded: make(chan domain.ReflectionRun, 1),
	}
	fixed := time.Date(2026, 8, 27, 16, 30, 0, 0, time.UTC)
	started := make(chan struct{})
	release := make(chan struct{})
	client := &apitest.Fake{
		ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
			return []api.ModelInfo{{
				ID:                  "test/reflection",
				SupportedParameters: []string{"tools", "structured_outputs"},
			}}, nil
		},
		ReflectPersonaFn: func(
			context.Context,
			domain.ModelID,
			domain.InstanceID,
			api.ReflectionInput,
			...api.ToolDefinition,
		) (api.ReflectionExploration, error) {
			close(started)
			<-release

			return api.ReflectionExploration{}, nil
		},
	}
	manager := New(Config{
		Store: stored, APIClient: client, InitialAPIKey: "configured",
		BaseContext: t.Context, Now: func() time.Time { return fixed },
		ReflectionMode: ReflectionShadow, ReflectionModel: "test/reflection",
		ReflectionRunID: func() domain.ReflectionRunID { return "shadow-1" },
	})
	before, err := stored.PersonaSnapshot(t.Context(), instance.ID())
	require.NoError(t, err)
	require.NoError(t, manager.AppendReflectionEvents(
		t.Context(), instance.ID(),
		reflectionCandidates(1, reflectionSubstantiveThreshold, fixed), fixed,
	))
	<-started
	require.NoError(t, manager.SetReflectionMode(t.Context(), ReflectionActive))
	close(release)
	run := <-stored.recorded
	after, err := stored.PersonaSnapshot(t.Context(), instance.ID())
	require.NoError(t, err)

	require.Equal(t, reflectionModeEscalationEffect{
		Run: domain.ReflectionRun{
			ID: "shadow-1", InstanceID: instance.ID(),
			BaseRevisionID:   before.Revision.ID,
			PriorCheckpoint:  0,
			HighWaterMark:    reflectionSubstantiveThreshold,
			ResultRevisionID: before.Revision.ID,
			ModelID:          "test/reflection", Outcome: domain.ReflectionShadow,
			StartedAt: fixed, FinishedAt: fixed,
		},
		Before: before, After: before,
	}, reflectionModeEscalationEffect{Run: run, Before: before, After: after})
	require.NoError(t, manager.DetachAll(t.Context()))
}

func TestManager_schedules_committed_reflection_candidates(t *testing.T) {
	stored, instance := reflectionSchedulerStore(t)
	recordedAt := time.Now()
	observed := make(chan reflectionRunRange, 1)
	manager := New(Config{
		Store: stored, BaseContext: t.Context,
		ReflectionMode: ReflectionShadow, ReflectionModel: "test/reflection",
	})
	manager.reflections.run = func(
		_ context.Context,
		snapshot store.PendingReflectionSnapshot,
	) {
		observed <- reflectionRange(snapshot)
	}

	require.NoError(t, manager.AppendReflectionEvents(
		t.Context(), instance.ID(),
		reflectionCandidates(1, reflectionSubstantiveThreshold, recordedAt),
		recordedAt,
	))
	run := <-observed
	status, err := stored.ReflectionInboxStatus(t.Context(), instance.ID())
	require.NoError(t, err)
	require.NoError(t, manager.DetachAll(t.Context()))

	require.Equal(t, managerReflectionEffect{
		Run: reflectionRunRange{
			Through:       status.HighWaterMark,
			HighWaterMark: status.HighWaterMark,
			Sequences:     reflectionSequences(1, reflectionSubstantiveThreshold),
		},
		Status: store.ReflectionInboxStatus{
			HighWaterMark:     status.HighWaterMark,
			PendingEvents:     reflectionSubstantiveThreshold,
			SubstantiveEvents: reflectionSubstantiveThreshold,
		},
	}, managerReflectionEffect{Run: run, Status: status})
}

func TestReflectionScheduler_coalesces_work_per_instance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stored, instance := reflectionSchedulerStore(t)
		recordedAt := time.Now()
		require.NoError(t, stored.AppendReflectionEvents(
			t.Context(), instance.ID(),
			reflectionCandidates(1, reflectionSubstantiveThreshold, recordedAt),
			recordedAt,
		))
		initial, err := stored.ReflectionInboxStatus(t.Context(), instance.ID())
		require.NoError(t, err)

		started := make(chan reflectionRunRange, 2)
		release := make(chan struct{})
		var first sync.Once
		scheduler := newReflectionScheduler(
			t.Context(), stored, time.Now,
			func(_ context.Context, snapshot store.PendingReflectionSnapshot) {
				started <- reflectionRange(snapshot)
				first.Do(func() { <-release })
			},
		)
		scheduler.notify(instance.ID())
		firstRun := <-started

		require.NoError(t, stored.AppendReflectionEvents(
			t.Context(), instance.ID(),
			reflectionCandidates(
				reflectionSubstantiveThreshold+1,
				reflectionSubstantiveThreshold+1,
				recordedAt.Add(time.Second),
			),
			recordedAt.Add(time.Second),
		))
		scheduler.notify(instance.ID())
		scheduler.notify(instance.ID())
		close(release)
		synctest.Wait()
		duringCooldown := drainReflectionRanges(started)

		time.Sleep(reflectionCooldown)
		synctest.Wait()
		secondRun := <-started
		require.NoError(t, scheduler.stop(t.Context()))

		scheduler.mu.Lock()
		workers := len(scheduler.workers)
		scheduler.mu.Unlock()
		require.Equal(t, reflectionWorkerState{
			Runs: []reflectionRunRange{
				{
					Through:       initial.HighWaterMark,
					HighWaterMark: initial.HighWaterMark,
					Sequences:     reflectionSequences(1, reflectionSubstantiveThreshold),
				},
				{
					Through:       initial.HighWaterMark + 1,
					HighWaterMark: initial.HighWaterMark + 1,
					Sequences: reflectionSequences(
						1, reflectionSubstantiveThreshold+1,
					),
				},
			},
		}, reflectionWorkerState{
			Runs:    slices.Concat([]reflectionRunRange{firstRun}, duringCooldown, []reflectionRunRange{secondRun}),
			Workers: workers,
		})
	})
}

func TestReflectionScheduler_waits_for_the_cooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stored, instance := reflectionSchedulerStore(t)
		reflectedAt := time.Now()
		snapshot, err := stored.PersonaSnapshot(t.Context(), instance.ID())
		require.NoError(t, err)
		_, err = stored.CommitPersonaReflection(t.Context(), store.PersonaReflectionAcceptance{
			RunID: "initial-reflection", InstanceID: instance.ID(),
			BaseRevisionID: snapshot.Revision.ID,
			ModelID:        "test/reflection", StartedAt: reflectedAt, FinishedAt: reflectedAt,
		})
		require.NoError(t, err)
		require.NoError(t, stored.AppendReflectionEvents(
			t.Context(), instance.ID(),
			reflectionCandidates(1, reflectionSubstantiveThreshold, reflectedAt),
			reflectedAt,
		))

		observed := make(chan reflectionRunRange, 1)
		scheduler := newReflectionScheduler(
			t.Context(), stored, time.Now,
			func(_ context.Context, snapshot store.PendingReflectionSnapshot) {
				observed <- reflectionRange(snapshot)
			},
		)
		scheduler.notify(instance.ID())
		synctest.Wait()
		before := drainReflectionRanges(observed)

		time.Sleep(reflectionCooldown)
		synctest.Wait()
		after := drainReflectionRanges(observed)
		require.NoError(t, scheduler.stop(t.Context()))

		require.Equal(t, reflectionWorkerState{
			Runs: []reflectionRunRange{{
				Through:       reflectionSubstantiveThreshold,
				HighWaterMark: reflectionSubstantiveThreshold,
				Sequences:     reflectionSequences(1, reflectionSubstantiveThreshold),
			}},
		}, reflectionWorkerState{Runs: append(before, after...)})
	})
}

func TestReflectionScheduler_cancels_and_joins_active_work(t *testing.T) {
	stored, instance := reflectionSchedulerStore(t)
	recordedAt := time.Now()
	require.NoError(t, stored.AppendReflectionEvents(
		t.Context(), instance.ID(),
		reflectionCandidates(1, reflectionSubstantiveThreshold, recordedAt),
		recordedAt,
	))

	started := make(chan struct{})
	cancelled := make(chan struct{})
	scheduler := newReflectionScheduler(
		t.Context(), stored, time.Now,
		func(ctx context.Context, _ store.PendingReflectionSnapshot) {
			close(started)
			<-ctx.Done()
			close(cancelled)
		},
	)
	scheduler.notify(instance.ID())
	<-started

	require.NoError(t, scheduler.stop(t.Context()))
	<-cancelled
	scheduler.mu.Lock()
	state := reflectionWorkerState{Workers: len(scheduler.workers)}
	scheduler.mu.Unlock()
	require.Equal(t, reflectionWorkerState{}, state)
}

func reflectionSchedulerStore(
	t *testing.T,
) (*store.SQLiteStore, *domain.Instance) {
	t.Helper()

	stored := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful and curious", nil,
	)
	require.NoError(t, stored.SaveInstance(t.Context(), instance))

	return stored, instance
}

func reflectionCandidates(
	first, last int,
	at time.Time,
) []store.ReflectionEventCandidate {
	candidates := make([]store.ReflectionEventCandidate, 0, last-first+1)
	for id := first; id <= last; id++ {
		candidates = append(candidates, store.ReflectionEventCandidate{
			Source: protocol.ChannelHistoryRef(int64(id), "#dev"),
			Message: protocol.IRCMessage{
				Kind: protocol.KindPrivMsg,
				Source: domain.ClientSource(
					domain.InstanceID("inst-alice"), "alice",
				),
				Target: "#dev", Body: "substantive activity", At: at,
			},
			Substantive: true,
		})
	}

	return candidates
}

func reflectionRange(snapshot store.PendingReflectionSnapshot) reflectionRunRange {
	sequences := make([]domain.ReflectionSequence, 0, len(snapshot.Events))
	for _, event := range snapshot.Events {
		sequences = append(sequences, event.Sequence)
	}

	return reflectionRunRange{
		Checkpoint:    snapshot.Range.Checkpoint,
		Through:       snapshot.Range.Through,
		HighWaterMark: snapshot.Status.HighWaterMark,
		Sequences:     sequences,
	}
}

func reflectionSequences(first, last int) []domain.ReflectionSequence {
	sequences := make([]domain.ReflectionSequence, 0, last-first+1)
	for sequence := first; sequence <= last; sequence++ {
		sequences = append(sequences, domain.ReflectionSequence(sequence))
	}

	return sequences
}

func drainReflectionRanges(ranges <-chan reflectionRunRange) []reflectionRunRange {
	var drained []reflectionRunRange
	for {
		select {
		case observed := <-ranges:
			drained = append(drained, observed)
		default:
			return drained
		}
	}
}

// reflectionRunRecorder is a scheduler runner that records one terminal
// reflection run per attempt, the way `Manager.runReflection` does for
// every outcome.
type reflectionRunRecorder struct {
	stored *store.SQLiteStore
	ranges chan reflectionRunRange
	errs   chan error
	runs   int
}

func (r *reflectionRunRecorder) run(
	ctx context.Context,
	snapshot store.PendingReflectionSnapshot,
) {
	r.runs++
	r.ranges <- reflectionRange(snapshot)
	now := time.Now()
	r.errs <- r.stored.RecordReflectionRun(ctx, domain.ReflectionRun{
		ID:               domain.ReflectionRunID(fmt.Sprintf("shadow-%d", r.runs)),
		InstanceID:       snapshot.Persona.Lineage.InstanceID,
		BaseRevisionID:   snapshot.Persona.Revision.ID,
		PriorCheckpoint:  snapshot.Status.Checkpoint,
		HighWaterMark:    snapshot.Status.HighWaterMark,
		ResultRevisionID: snapshot.Persona.Revision.ID,
		ModelID:          "test/reflection", Outcome: domain.ReflectionShadow,
		StartedAt: now, FinishedAt: now,
	})
}

func TestReflectionScheduler_waits_after_a_run_that_committed_nothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stored, instance := reflectionSchedulerStore(t)
		at := time.Now()
		require.NoError(t, stored.AppendReflectionEvents(
			t.Context(), instance.ID(),
			reflectionCandidates(1, reflectionSubstantiveThreshold, at), at,
		))
		recorder := &reflectionRunRecorder{
			stored: stored,
			ranges: make(chan reflectionRunRange, 8),
			errs:   make(chan error, 8),
		}
		scheduler := newReflectionScheduler(
			t.Context(), stored, time.Now, recorder.run,
		)

		scheduler.notify(instance.ID())
		synctest.Wait()
		for burst := range 5 {
			sequence := reflectionSubstantiveThreshold + 1 + burst
			require.NoError(t, stored.AppendReflectionEvents(
				t.Context(), instance.ID(),
				reflectionCandidates(sequence, sequence, at), at,
			))
			scheduler.notify(instance.ID())
			synctest.Wait()
		}
		require.NoError(t, scheduler.stop(t.Context()))
		close(recorder.errs)
		for err := range recorder.errs {
			require.NoError(t, err)
		}

		require.Equal(t, reflectionWorkerState{
			Runs: []reflectionRunRange{{
				Through:       reflectionSubstantiveThreshold,
				HighWaterMark: reflectionSubstantiveThreshold,
				Sequences:     reflectionSequences(1, reflectionSubstantiveThreshold),
			}},
		}, reflectionWorkerState{Runs: drainReflectionRanges(recorder.ranges)})
	})
}

func TestReflectionScheduler_runs_again_past_the_input_event_limit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stored, instance := reflectionSchedulerStore(t)
		at := time.Now()
		backlog := reflectionInputEventLimit + 5
		require.NoError(t, stored.AppendReflectionEvents(
			t.Context(), instance.ID(),
			reflectionCandidates(1, backlog, at), at,
		))
		recorder := &reflectionRunRecorder{
			stored: stored,
			ranges: make(chan reflectionRunRange, 8),
			errs:   make(chan error, 8),
		}
		scheduler := newReflectionScheduler(
			t.Context(), stored, time.Now, recorder.run,
		)

		scheduler.notify(instance.ID())
		synctest.Wait()
		first := drainReflectionRanges(recorder.ranges)
		require.NoError(t, stored.AppendReflectionEvents(
			t.Context(), instance.ID(),
			reflectionCandidates(backlog+1, backlog+25, at), at,
		))
		scheduler.notify(instance.ID())
		time.Sleep(reflectionCooldown)
		synctest.Wait()
		second := drainReflectionRanges(recorder.ranges)
		require.NoError(t, scheduler.stop(t.Context()))
		close(recorder.errs)
		for err := range recorder.errs {
			require.NoError(t, err)
		}

		require.Equal(t, reflectionWorkerState{
			Runs: []reflectionRunRange{
				{
					Through:       reflectionInputEventLimit,
					HighWaterMark: domain.ReflectionSequence(backlog),
					Sequences:     reflectionSequences(1, reflectionInputEventLimit),
				},
				{
					Through:       reflectionInputEventLimit,
					HighWaterMark: domain.ReflectionSequence(backlog + 25),
					Sequences:     reflectionSequences(1, reflectionInputEventLimit),
				},
			},
		}, reflectionWorkerState{Runs: append(first, second...)})
	})
}

// recordedInboxEffect is what the inbox holds after a dispatch batch, and
// whether the scheduler that would act on it exists, and what enabling
// reflection then does with the backlog.
type recordedInboxEffect struct {
	Mode      ReflectionMode
	Status    store.ReflectionInboxStatus
	Scheduled bool
	Woken     int
}

// TestManager_records_reflection_candidates_while_reflection_is_disabled pins
// that the candidate stream does not follow the mode, and that turning
// reflection on acts on what is already there.
//
// A worker exists only once something wakes it, and the next thing that
// would is the instance's next delivery. Without a wake at the mode
// change, an instance already over its threshold waits for somebody to
// speak to it before the history it has accumulated is looked at.
func TestManager_records_reflection_candidates_while_reflection_is_disabled(t *testing.T) {
	stored, instance := reflectionSchedulerStore(t)
	at := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	manager := New(Config{
		Store: stored, BaseContext: t.Context,
		Now:            func() time.Time { return at },
		ReflectionMode: ReflectionDisabled,
	})

	require.NoError(t, manager.AppendReflectionEvents(
		t.Context(), instance.ID(),
		reflectionCandidates(1, reflectionSubstantiveThreshold, at), at,
	))
	status, err := stored.ReflectionInboxStatus(t.Context(), instance.ID())
	require.NoError(t, err)
	mode, _ := manager.ReflectionSettings()
	scheduled := manager.reflections != nil

	require.NoError(t, manager.SetReflectionMode(t.Context(), ReflectionShadow))
	// `notify` registers the worker before it returns, so the count is
	// settled here and says the stored instance was picked up without
	// waiting for a delivery.
	manager.mu.Lock()
	scheduler := manager.reflections
	manager.mu.Unlock()
	scheduler.mu.Lock()
	woken := len(scheduler.workers)
	scheduler.mu.Unlock()
	require.NoError(t, manager.DetachAll(t.Context()))

	require.Equal(t, recordedInboxEffect{
		Mode: ReflectionDisabled,
		Status: store.ReflectionInboxStatus{
			HighWaterMark:     domain.ReflectionSequence(reflectionSubstantiveThreshold),
			PendingEvents:     reflectionSubstantiveThreshold,
			SubstantiveEvents: reflectionSubstantiveThreshold,
		},
		Woken: 1,
	}, recordedInboxEffect{
		Mode: mode, Status: status, Scheduled: scheduled, Woken: woken,
	})
}
