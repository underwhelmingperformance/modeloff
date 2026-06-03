package session

import (
	"context"
	cryptorand "crypto/rand"
	"log/slog"
	"math/big"
	"time"

	"github.com/laney/modeloff/internal/domain"
)

// pokeDisabledPollInterval is how long the scheduler waits before
// re-consulting its [PokeSchedule] while poking is paused — no API
// key, or a non-positive interval.
const pokeDisabledPollInterval = time.Minute

// PokeSchedule supplies the live poke cadence to the session's
// background scheduler. The session consults it once per cycle, so a
// `/config poke-interval` change or a freshly-set API key takes
// effect on the next tick. It reports the base interval and whether
// poking is currently enabled; a non-positive interval or
// enabled==false pauses the scheduler.
type PokeSchedule func(ctx context.Context) (interval time.Duration, enabled bool)

// StartPoking launches the session-owned poke scheduler in a
// background goroutine. The scheduler manufactures spontaneous
// activity (AGENTS.md point 12): on a perturbed cadence it pokes
// every channel window that has stayed quiet for a full cycle, so
// dead channels get a nudge without the user driving it.
//
// `ctx` bounds the scheduler's lifetime: cancelling it stops the
// goroutine. Production passes the same signal-derived context the
// rest of the app shuts down on.
func (s *Session) StartPoking(ctx context.Context, schedule PokeSchedule) {
	go s.runPokeLoop(ctx, schedule)
}

func (s *Session) runPokeLoop(ctx context.Context, schedule PokeSchedule) {
	for {
		interval, enabled := schedule(ctx)

		delay := pokeDisabledPollInterval
		if enabled && interval > 0 {
			delay = perturbDuration(interval)
		}

		if !sleepOrDone(ctx, delay) {
			return
		}

		if !enabled || interval <= 0 {
			continue
		}

		if err := s.pokeQuietWindows(ctx); err != nil {
			slog.Default().ErrorContext(ctx, "scheduled poke", "component", "session", "error", err)
		}
	}
}

// PokeNow pokes every channel window immediately, regardless of
// recent activity.
func (s *Session) PokeNow(ctx context.Context) error {
	names, err := s.ChannelWindowNames(ctx)
	if err != nil {
		return err
	}

	s.pokeWindows(ctx, names)

	return nil
}

// pokeQuietWindows pokes each channel window that saw no chat
// traffic since the previous cycle. Channels that were active are
// cleared by the drain and spared this round; if they fall silent
// they are poked on a later cycle.
func (s *Session) pokeQuietWindows(ctx context.Context) error {
	names, err := s.ChannelWindowNames(ctx)
	if err != nil {
		return err
	}

	active := s.drainActiveChannels()

	quiet := make([]domain.ChannelName, 0, len(names))
	for _, ch := range names {
		if _, busy := active[ch]; busy {
			continue
		}

		quiet = append(quiet, ch)
	}

	s.pokeWindows(ctx, quiet)

	return nil
}

// pokeWindows emits a [domain.PokeEvent] for each named channel on
// the protocol bus, stamped with the session clock. The membership
// filter delivers each poke only to the model-clients in the
// channel.
func (s *Session) pokeWindows(ctx context.Context, names []domain.ChannelName) {
	now := s.now()

	for _, ch := range names {
		s.emit(ctx, domain.PokeEvent{Channel: ch, At: now})
	}
}

// noteChatActivity flags the target channel as live when `pe` is a
// channel chat message, feeding the scheduler's quiescence check.
// DM and non-message events do not count as channel activity.
func (s *Session) noteChatActivity(pe domain.ProtocolEvent) {
	msg, ok := pe.(domain.Message)
	if !ok {
		return
	}

	if domain.InferChannelKind(msg.Target) != domain.KindChannel {
		return
	}

	s.markChannelActivity(msg.Target)
}

// markChannelActivity records that `ch` saw chat traffic during the
// current poke cycle.
func (s *Session) markChannelActivity(ch domain.ChannelName) {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()

	s.activeChannels[ch] = struct{}{}
}

// drainActiveChannels returns the set of channels that saw traffic
// since the previous drain and resets the tracker.
func (s *Session) drainActiveChannels() map[domain.ChannelName]struct{} {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()

	active := s.activeChannels
	s.activeChannels = make(map[domain.ChannelName]struct{})

	return active
}

// perturbDuration jitters `interval` by up to ±10% so pokes don't
// land in lockstep across channels and restarts.
func perturbDuration(interval time.Duration) time.Duration {
	delta := interval / 10
	if delta <= 0 {
		return interval
	}

	n, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(delta*2)+1))
	if err != nil {
		return interval
	}

	offset := time.Duration(n.Int64()) - delta

	return interval + offset
}

// sleepOrDone waits for `delay` or until `ctx` is cancelled. It
// reports false when the wait was cut short by cancellation.
func sleepOrDone(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
