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

// pokeBackoffCap is the largest multiplier [Session.pokeQuietWindows]
// applies to a channel's base poke cadence. A channel that never
// replies is poked at most an eighth as often, at steady state, as a
// channel that just went quiet — not once every cycle forever.
const pokeBackoffCap = 8

// pokeBackoff tracks one channel's progress through the poke
// scheduler's exponential backoff: how many scheduled-poke cycles it
// has gone without chat traffic (quietTicks), and the cadence
// multiplier that traffic last earned it (multiplier). The zero
// value means "never backed off" — a channel seen quiet for the
// first time is poked on this very cycle, matching the cadence a
// channel already got before backoff existed.
type pokeBackoff struct {
	multiplier int
	quietTicks int
}

// due reports whether a channel carrying this backoff state should
// be poked this cycle, and returns the state to store afterwards.
// Every call advances quietTicks by one cycle; reaching the current
// multiplier pokes the channel, resets the tick count, and doubles
// the multiplier for next time, capped at [pokeBackoffCap].
func (b pokeBackoff) due() (bool, pokeBackoff) {
	if b.multiplier == 0 {
		b.multiplier = 1
	}

	b.quietTicks++
	if b.quietTicks < b.multiplier {
		return false, b
	}

	b.quietTicks = 0
	b.multiplier = min(b.multiplier*2, pokeBackoffCap)

	return true, b
}

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

		woken, ok := s.waitPokeCycle(ctx, delay)
		if !ok {
			return
		}

		if woken {
			// A schedule change interrupted the wait: re-consult it
			// and sleep on the fresh value. This cycle skips the
			// poke and lets the fresh interval govern from here.
			continue
		}

		if !enabled || interval <= 0 {
			continue
		}

		if err := s.pokeQuietWindows(ctx); err != nil {
			slog.Default().ErrorContext(ctx, "scheduled poke", "component", "session", "error", err)
		}
	}
}

// WakePoke interrupts the poke scheduler's current sleep, so a
// [PokeSchedule] change takes effect on this cycle even while a
// longer sleep from the previous cycle is already under way. Safe to
// call whether or not the scheduler is running; a call with nothing
// listening is a no-op.
func (s *Session) WakePoke() {
	select {
	case s.pokeWake <- struct{}{}:
	default:
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
// traffic since the previous cycle and whose backoff says it is due.
// Channels that were active are cleared by the drain, spared this
// round, and have their backoff reset: a channel that just proved
// itself lively is poked again, at the base cadence, as soon as it
// next falls quiet. A channel that stays quiet pass after pass is
// poked on a widening schedule (see [pokeBackoff]), bounded by
// [pokeBackoffCap].
//
// `names` is also the live channel set pokeBackoffState is pruned
// against: a channel destroyed when its last member parts (RFC 2811
// §2) never appears here again, so its entry would otherwise sit in
// the map for the rest of the process — one entry per channel ever
// created.
func (s *Session) pokeQuietWindows(ctx context.Context) error {
	names, err := s.ChannelWindowNames(ctx)
	if err != nil {
		return err
	}

	active := s.drainActiveChannels()

	due := make([]domain.ChannelName, 0, len(names))
	for _, ch := range names {
		if _, busy := active[ch]; busy {
			s.resetPokeBackoff(ch)
			continue
		}

		if s.pokeBackoffDue(ch) {
			due = append(due, ch)
		}
	}

	s.prunePokeBackoff(names)
	s.pokeWindows(ctx, due)

	return nil
}

// prunePokeBackoff drops every pokeBackoffState entry whose channel
// is not in live, the live channel set this cycle's caller already
// read from the store.
func (s *Session) prunePokeBackoff(live []domain.ChannelName) {
	inLive := make(map[domain.ChannelName]struct{}, len(live))
	for _, ch := range live {
		inLive[ch] = struct{}{}
	}

	s.pokeBackoffMu.Lock()
	defer s.pokeBackoffMu.Unlock()

	for ch := range s.pokeBackoffState {
		if _, ok := inLive[ch]; !ok {
			delete(s.pokeBackoffState, ch)
		}
	}
}

// pokeBackoffDue advances `ch`'s backoff by one quiet cycle and
// reports whether it is due a poke this cycle.
func (s *Session) pokeBackoffDue(ch domain.ChannelName) bool {
	s.pokeBackoffMu.Lock()
	defer s.pokeBackoffMu.Unlock()

	due, next := s.pokeBackoffState[ch].due()
	s.pokeBackoffState[ch] = next

	return due
}

// resetPokeBackoff clears `ch`'s backoff, so its next quiet cycle
// pokes it immediately.
func (s *Session) resetPokeBackoff(ch domain.ChannelName) {
	s.pokeBackoffMu.Lock()
	defer s.pokeBackoffMu.Unlock()

	delete(s.pokeBackoffState, ch)
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

// waitPokeCycle waits for `delay`, a [Session.WakePoke] interrupt, or
// `ctx` cancellation, whichever comes first. `ok` reports false only
// on cancellation; `woken` distinguishes an interrupted wait (the
// caller should re-consult the schedule without poking) from one
// that ran its full course.
func (s *Session) waitPokeCycle(ctx context.Context, delay time.Duration) (woken, ok bool) {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false, false
	case <-s.pokeWake:
		return true, true
	case <-timer.C:
		return false, true
	}
}
