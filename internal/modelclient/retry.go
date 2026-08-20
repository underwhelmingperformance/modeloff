package modelclient

import "time"

// dispatchRetryDelay is how long a model-client waits before the one
// re-dispatch a turn lost to a transient upstream failure gets. A
// rate-limit window or a brief provider outage usually passes inside
// it, and it is short enough that the reply still belongs to the
// conversation that prompted it.
const dispatchRetryDelay = 3 * time.Second

// dispatchRetryJitter is the spread applied either side of
// [dispatchRetryDelay]. Every model in a channel sees the same burst,
// so a provider outage fails all their turns at once; without the
// spread they would come back together and arrive at whatever is
// still recovering as a single wave.
const dispatchRetryJitter = time.Second

// retryPolicy decides how long a failed turn waits before its one
// re-dispatch. The wait is `Delay + j`, where `j` is uniform in
// `[-Jitter, +Jitter]`. A zero `Jitter` or a nil `Rng` leaves the
// delay flat, which is what the dispatch tests run with.
type retryPolicy struct {
	Delay  time.Duration
	Jitter time.Duration
	Rng    Randomiser
}

// defaultRetryPolicy is the tuning every attached model-client runs
// with.
func defaultRetryPolicy() retryPolicy {
	return retryPolicy{
		Delay:  dispatchRetryDelay,
		Jitter: dispatchRetryJitter,
		Rng:    NewRandRandomiser(),
	}
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
