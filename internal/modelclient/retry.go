package modelclient

import (
	"context"
	"time"

	"github.com/laney/modeloff/internal/api"
)

// dispatchRetryDelay is the fallback wait before redispatching a turn
// once after a transient upstream failure. A provider's Retry-After
// response overrides it.
const dispatchRetryDelay = 3 * time.Second

// dispatchRetryJitter is the spread applied either side of the
// fallback [dispatchRetryDelay]. Every model in a channel sees the
// same burst, so a provider outage fails all their turns at once;
// without the spread they would come back together and arrive at
// whatever is still recovering as a single wave.
const dispatchRetryJitter = time.Second

// retryPolicy decides how long a failed request waits before its single
// retry. A provider-directed delay takes precedence. Without one, the
// wait is `Delay + j`, where `j` is uniform in `[-Jitter, +Jitter]`.
// A zero `Jitter` or a nil `Rng` leaves the fallback delay flat.
type retryPolicy struct {
	Delay  time.Duration
	Jitter time.Duration
	Rng    Randomiser
	Waiter retryWaiter
}

type retryWaiter interface {
	Wait(ctx context.Context, delay time.Duration) error
}

type timerRetryWaiter struct{}

func (timerRetryWaiter) Wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// defaultRetryPolicy is the tuning every attached model-client runs
// with.
func defaultRetryPolicy() retryPolicy {
	return retryPolicy{
		Delay:  dispatchRetryDelay,
		Jitter: dispatchRetryJitter,
		Rng:    NewRandRandomiser(),
		Waiter: timerRetryWaiter{},
	}
}

// durationFor returns the provider's requested retry delay when the
// response supplied one. Other transient failures use the local
// jittered delay.
func (p retryPolicy) durationFor(err error) (time.Duration, error) {
	if delay, ok := api.RetryAfter(err, time.Now()); ok {
		return delay, nil
	}

	return p.duration()
}

func (p retryPolicy) wait(ctx context.Context, err error) error {
	delay, durationErr := p.durationFor(err)
	if durationErr != nil {
		return durationErr
	}

	return p.waitFor(ctx, delay)
}

func (p retryPolicy) waitFor(ctx context.Context, delay time.Duration) error {
	waiter := p.Waiter
	if waiter == nil {
		waiter = timerRetryWaiter{}
	}

	return waiter.Wait(ctx, delay)
}

// duration returns the wait before the re-dispatch, and surfaces a
// failure of the entropy source. The caller abandons the retry on
// one: an unjittered wave of re-dispatches is the thing the spread
// exists to prevent, so a draw that cannot be made ends the schedule.
func (p retryPolicy) duration() (time.Duration, error) {
	if p.Jitter <= 0 || p.Rng == nil {
		return p.Delay, nil
	}

	f, err := p.Rng.Float64()
	if err != nil {
		return 0, err
	}

	return p.Delay + time.Duration((f*2-1)*float64(p.Jitter)), nil
}
