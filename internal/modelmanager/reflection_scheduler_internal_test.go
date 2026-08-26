package modelmanager

import (
	"context"
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
			t.Context(), quietReflectionStore{}, time.Now,
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
