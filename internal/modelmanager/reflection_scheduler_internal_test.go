package modelmanager

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/store"
)

// quietReflectionStore answers with a snapshot below the activity
// threshold, which is what parks a worker.
type quietReflectionStore struct{}

func (quietReflectionStore) PendingReflectionSnapshot(
	context.Context,
	domain.InstanceID,
	int,
) (store.PendingReflectionSnapshot, error) {
	return store.PendingReflectionSnapshot{
		Status: store.ReflectionInboxStatus{
			HighWaterMark: 1, PendingEvents: 1, SubstantiveEvents: 1,
		},
	}, nil
}

// TestReflectionScheduler_forget_releases_a_parked_worker pins that a
// deleted instance takes its worker with it.
//
// A worker below the activity threshold waits to be woken, and only a
// recorded candidate wakes it. A deleted instance records nothing more,
// so without this the goroutine and its map entry would live until the
// manager drains, and creating and deleting quiet instances would leak
// one goroutine each.
func TestReflectionScheduler_forget_releases_a_parked_worker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const instanceID = domain.InstanceID("inst-botty")

		scheduler := newReflectionScheduler(
			t.Context(), quietReflectionStore{}, systemReflectionClock{},
			func(context.Context, store.PendingReflectionSnapshot) {},
		)

		scheduler.notify(instanceID)
		synctest.Wait()
		scheduler.mu.Lock()
		parked := len(scheduler.workers)
		scheduler.mu.Unlock()

		scheduler.forget(instanceID)
		synctest.Wait()
		// The wait returns only once the worker's goroutine has finished.
		// While it is still parked this deadlocks, which is what synctest
		// reports.
		scheduler.wg.Wait()

		scheduler.mu.Lock()
		remaining := len(scheduler.workers)
		scheduler.mu.Unlock()

		require.Equal(t, []int{1, 0}, []int{parked, remaining})
	})
}

// cooldownReflectionStore answers above the activity threshold with an
// attempt recorded at recordedAt, which puts a worker reading it inside
// its cooldown.
type cooldownReflectionStore struct {
	recordedAt time.Time
}

func (s *cooldownReflectionStore) PendingReflectionSnapshot(
	context.Context,
	domain.InstanceID,
	int,
) (store.PendingReflectionSnapshot, error) {
	recorded := s.recordedAt

	return store.PendingReflectionSnapshot{
		Status: store.ReflectionInboxStatus{
			HighWaterMark:     reflectionSubstantiveThreshold,
			PendingEvents:     reflectionSubstantiveThreshold,
			SubstantiveEvents: reflectionSubstantiveThreshold,
			LastAttemptAt:     &recorded,
		},
	}, nil
}

// stoppedClock never advances and its wait never fires.
type stoppedClock struct {
	at    time.Time
	waits chan time.Duration
}

// Now reports the instant this clock stopped at.
func (c *stoppedClock) Now() time.Time { return c.at }

// After records the wait and returns a channel that never receives. The
// send cannot block, or the worker would park inside the wait being
// observed. An overflowed buffer fails the effect comparison.
func (c *stoppedClock) After(delay time.Duration) <-chan time.Time {
	select {
	case c.waits <- delay:
	default:
	}

	return make(chan time.Time)
}

// cooldownWaitEffect records the delays requested and the runs made.
type cooldownWaitEffect struct {
	Delays    []time.Duration
	Reflected int64
}

// TestReflectionScheduler_waits_out_the_cooldown_on_its_own_clock pins
// that the comparison and the wait come from the clock the constructor
// was given.
//
// It asks twice because `notify` both starts the worker and wakes it,
// and the buffered wake is taken by the first wait; the second is the
// one the worker parks on.
func TestReflectionScheduler_waits_out_the_cooldown_on_its_own_clock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const instanceID = domain.InstanceID("inst-botty")
		frozen := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
		stored := &cooldownReflectionStore{recordedAt: frozen.Add(-reflectionCooldown / 2)}
		clock := &stoppedClock{at: frozen, waits: make(chan time.Duration, 8)}

		var reflected atomic.Int64
		scheduler := newReflectionScheduler(
			t.Context(), stored, clock,
			func(context.Context, store.PendingReflectionSnapshot) {
				reflected.Add(1)
			},
		)

		scheduler.notify(instanceID)
		synctest.Wait()

		close(clock.waits)
		var delays []time.Duration
		for delay := range clock.waits {
			delays = append(delays, delay)
		}

		require.Equal(t, cooldownWaitEffect{
			Delays: []time.Duration{
				reflectionCooldown / 2, reflectionCooldown / 2,
			},
			Reflected: 0,
		}, cooldownWaitEffect{Delays: delays, Reflected: reflected.Load()})
	})
}
