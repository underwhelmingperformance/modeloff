package modelmanager

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/store"
)

const reflectionInputEventLimit = 100

type reflectionSnapshotStore interface {
	PendingReflectionSnapshot(
		ctx context.Context,
		instanceID domain.InstanceID,
		limit int,
	) (store.PendingReflectionSnapshot, error)
}

type reflectionStateStore interface {
	reflectionSnapshotStore
	reflectionRecallStore
	CommitPersonaReflection(
		ctx context.Context,
		acceptance store.PersonaReflectionAcceptance,
	) (store.PersonaReflectionCommit, error)
	RecordReflectionRun(ctx context.Context, run domain.ReflectionRun) error
}

type reflectionSnapshotRunner func(context.Context, store.PendingReflectionSnapshot)

// reflectionClock is the scheduler's source of time. A run is due at an
// instant derived from stored wall-clock times, and the worker compares
// that instant against Now and waits on After to reach it. One type
// supplies both so a caller cannot pair a clock with another clock's
// wait.
//
// The stored times come from the manager's own clock, which production
// takes from the wall clock as this does. A caller that freezes that
// one shifts the cooldown; it cannot make a worker wait for an instant
// its own clock will never reach.
type reflectionClock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

// systemReflectionClock is the wall clock. Under testing/synctest both
// halves are virtualised together, so a test drives a cooldown by
// sleeping.
type systemReflectionClock struct{}

// Now reports the current wall-clock time.
func (systemReflectionClock) Now() time.Time { return time.Now() }

// After reports a channel that receives once d has elapsed.
func (systemReflectionClock) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}

type reflectionWorker struct {
	wake chan struct{}

	// done is closed when the instance this worker watches is deleted. A
	// worker below the activity threshold is parked until it is woken,
	// and a deleted instance records nothing more, so nothing would wake
	// it: without this the goroutine and its map entry would live until
	// the manager drains.
	done chan struct{}

	// ctx is what a run for this instance carries, and cancel is what
	// [reflectionScheduler.forget] ends it with. A run holds a snapshot
	// read before the deletion, and the state it would write from that
	// snapshot outlives the rows the deletion removed.
	ctx    context.Context
	cancel context.CancelFunc

	// finished is closed once the worker's goroutine has returned, so
	// forget can wait for a run in flight before its caller removes what
	// the run was working on.
	finished chan struct{}
}

type reflectionScheduler struct {
	ctx    context.Context
	cancel context.CancelFunc
	store  reflectionSnapshotStore
	clock  reflectionClock
	run    reflectionSnapshotRunner

	mu      sync.Mutex
	workers map[domain.InstanceID]*reflectionWorker

	// forgotten holds the finish signal of every worker cancelled by a
	// deletion, until the cleanup that deletion started has waited for
	// it.
	forgotten map[domain.InstanceID]chan struct{}
	stopping  bool
	wg        sync.WaitGroup
}

func newReflectionScheduler(
	parent context.Context,
	stored reflectionSnapshotStore,
	clock reflectionClock,
	run reflectionSnapshotRunner,
) *reflectionScheduler {
	ctx, cancel := context.WithCancel(parent)

	return &reflectionScheduler{
		ctx: ctx, cancel: cancel, store: stored, clock: clock, run: run,
		workers:   make(map[domain.InstanceID]*reflectionWorker),
		forgotten: make(map[domain.InstanceID]chan struct{}),
	}
}

// forget cancels the worker watching one instance and stops tracking it.
// The instance is gone, so there is nothing left for it to reflect on
// and nothing that could wake it.
//
// It does not wait. The caller is a KILL or a QUIT, which is a person
// waiting on a command, and a run cancelled mid-request still writes its
// terminal record under a timeout of its own. What the run must not
// outlive is the removal of the instance's derived state, and
// [reflectionScheduler.awaitForgotten] is where that wait belongs.
func (s *reflectionScheduler) forget(instanceID domain.InstanceID) {
	s.mu.Lock()
	worker := s.workers[instanceID]
	delete(s.workers, instanceID)
	if worker != nil {
		s.forgotten[instanceID] = worker.finished
	}
	s.mu.Unlock()

	if worker == nil {
		return
	}

	worker.cancel()
	close(worker.done)
}

// awaitForgotten waits for a forgotten instance's run to return, under
// the caller's context.
//
// A run holds a snapshot read before the deletion, and its derived
// writes, the semantic index among them, would land after the cleanup
// that was supposed to take them. The wait is short because the run is
// already cancelled; the context is what bounds it when the run is not
// coming back.
func (s *reflectionScheduler) awaitForgotten(ctx context.Context, instanceID domain.InstanceID) {
	s.mu.Lock()
	finished := s.forgotten[instanceID]
	delete(s.forgotten, instanceID)
	s.mu.Unlock()

	if finished == nil {
		return
	}

	select {
	case <-finished:
	case <-ctx.Done():
	}
}

func (s *reflectionScheduler) notify(instanceID domain.InstanceID) {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return
	}
	worker := s.workers[instanceID]
	if worker == nil {
		workerCtx, cancel := context.WithCancel(s.ctx)
		worker = &reflectionWorker{
			wake:     make(chan struct{}, 1),
			done:     make(chan struct{}),
			ctx:      workerCtx,
			cancel:   cancel,
			finished: make(chan struct{}),
		}
		s.workers[instanceID] = worker
		s.wg.Add(1)
		go s.work(instanceID, worker)
	}
	s.mu.Unlock()

	select {
	case worker.wake <- struct{}{}:
	default:
	}
}

func (s *reflectionScheduler) work(
	instanceID domain.InstanceID,
	worker *reflectionWorker,
) {
	defer s.wg.Done()
	defer close(worker.finished)
	defer worker.cancel()
	defer func() {
		s.mu.Lock()
		if s.workers[instanceID] == worker {
			delete(s.workers, instanceID)
		}
		s.mu.Unlock()
	}()

	var lastAttempt time.Time
	for {
		snapshot, err := s.store.PendingReflectionSnapshot(
			worker.ctx, instanceID, reflectionInputEventLimit,
		)
		if errors.Is(err, store.ErrNoPersonaLineage) || errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			slog.Default().ErrorContext(worker.ctx, "read reflection snapshot",
				"component", "modelmanager",
				"instance_id", instanceID,
				"error", err,
			)
			if !s.waitForWake(worker) {
				return
			}
			continue
		}

		schedule := reflectionScheduleFor(snapshot.Status, lastAttempt, s.clock.Now())
		if !schedule.Ready {
			if !s.waitUntilReady(worker, schedule) {
				return
			}
			continue
		}

		s.run(worker.ctx, snapshot)
		lastAttempt = s.clock.Now()
	}
}

func (s *reflectionScheduler) waitUntilReady(
	worker *reflectionWorker,
	schedule reflectionSchedule,
) bool {
	if schedule.SubstantiveEvents < reflectionSubstantiveThreshold || schedule.DueAt.IsZero() {
		return s.waitForWake(worker)
	}

	delay := schedule.DueAt.Sub(s.clock.Now())
	if delay <= 0 {
		return true
	}

	select {
	case <-s.ctx.Done():
		return false
	case <-worker.done:
		return false
	case <-worker.wake:
		return true
	case <-s.clock.After(delay):
		return true
	}
}

func (s *reflectionScheduler) waitForWake(worker *reflectionWorker) bool {
	select {
	case <-s.ctx.Done():
		return false
	case <-worker.done:
		return false
	case <-worker.wake:
		return true
	}
}

func (s *reflectionScheduler) stop(ctx context.Context) error {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	s.cancel()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
