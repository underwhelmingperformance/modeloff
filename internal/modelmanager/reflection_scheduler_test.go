package modelmanager

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

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

func TestManager_schedules_committed_reflection_candidates(t *testing.T) {
	stored, instance := reflectionSchedulerStore(t)
	recordedAt := time.Now()
	observed := make(chan reflectionRunRange, 1)
	manager := New(Config{Store: stored, BaseContext: t.Context})
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
