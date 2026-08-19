package session

import (
	"context"
	"sync"
	"time"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// floodPolicy is the server's flood-control setting. RFC 1459 §8.10
// gives each connection a message timer: the server moves the timer
// up to the current time when it has fallen behind, processes
// messages while the timer is no more than `maxLead` ahead of now,
// and charges `penalty` to the timer for each message it processes.
//
// The session has one such setting and applies it to every
// connection. There is no per-client variant of it.
type floodPolicy struct {
	penalty time.Duration
	maxLead time.Duration
}

// rfcFloodPolicy is the pacing RFC 1459 §8.10 recommends, and what
// every session runs with. Two seconds a message with ten seconds of
// lead lets a client send five or six commands at once and one every
// two seconds after that.
var rfcFloodPolicy = floodPolicy{
	penalty: 2 * time.Second,
	maxLead: 10 * time.Second,
}

// enforced reports whether this policy paces anything. A zero
// penalty leaves the message timer at the current time forever, so
// nothing is ever held back. That is what the zero policy means, and
// [New] never installs it.
func (p floodPolicy) enforced() bool {
	return p.penalty > 0
}

// penaltyTimer is one connection's message timer. The same
// accounting applies to the user-client and to every model-client,
// because how fast a client sends is a property of the connection.
// Nothing here reads what kind of actor is on the other end of it.
// The asymmetry the app wants follows from behaviour: a person
// typing never reaches the threshold, two models answering each
// other do.
//
// The timer reads the monotonic clock, not [Session.now]. Making the
// two agree is a tempting simplification, and it would break the
// timer: `Session.now` is the clock that stamps domain events, and it
// is commonly frozen at a fixed instant, whereas this timer measures
// elapsed time between commands. Frozen, it would hold every command
// after the opening burst forever.
type penaltyTimer struct {
	mu sync.Mutex

	// since is the message timer: the time up to which this
	// connection has already spent its allowance.
	since time.Time

	// throttled records whether the connection is in a throttle
	// episode, so the server warns it once as the episode opens and
	// not once per held-back command.
	throttled bool
}

// charge bills one command to the timer and reports how long the
// command must be held back, along with whether this command opened
// a throttle episode.
//
// A held-back command is delayed, never dropped. The server reads a
// flooding connection more slowly; it does not decide on the
// client's behalf that a message it sent did not happen.
//
// An episode ends only when the timer has drained the whole way back
// to now, which takes as long as the client spent building it up. A
// client that pauses for one penalty period drops under the lead
// threshold and stops being held back, but stays inside the episode,
// so alternating between bursts and short pauses raises one warning
// and not one per burst.
func (t *penaltyTimer) charge(policy floodPolicy, now time.Time) (delay time.Duration, opened bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// A timer behind the current time is one the client has stopped
	// spending against for long enough to clear everything it owed.
	if drained := t.since.Before(now); drained {
		t.since = now
		t.throttled = false
	}

	lead := t.since.Sub(now)
	t.since = t.since.Add(policy.penalty)

	if lead <= policy.maxLead {
		return 0, false
	}

	opened = !t.throttled
	t.throttled = true

	return lead - policy.maxLead, opened
}

// throttleCommand bills one command to the issuing connection's
// message timer and holds the command for as long as the timer says
// before the dispatcher runs it. It returns the delay it imposed, so
// the caller can record it on the command's span.
//
// This runs on the caller's goroutine, before the command reaches
// [Session.onWriter]. That placement is the whole of the deadlock
// argument, and it has four parts:
//
//   - The command loop never reaches here. Code on the loop may not
//     call `Handle` or a client's `Send`, so no wait can start with
//     the loop's turn in hand.
//   - Nothing is held across the wait. `charge` releases the timer's
//     lock before returning, and the notice below reaches the client
//     through the subscription's send queue, which no producer waits
//     on.
//   - A model-client whose dispatch goroutine waits here is not
//     reading its events channel, exactly as if it were mid-turn.
//     Peers sending to it still do not block: their deliveries queue,
//     and past `sendQAllowance` the subscription is disconnected on a
//     goroutine of its own. That teardown takes the command loop for
//     the QUIT, which this goroutine does not hold, and it releases
//     the model-client without joining the goroutine waiting here.
//   - The wait ends early on anything that makes the command
//     pointless to run: the caller's context, the subscription being
//     reaped, or the command loop stopping. A throttled client
//     therefore never holds up its own teardown or the session's
//     shutdown.
func (s *Session) throttleCommand(ctx context.Context, c protocol.Client) time.Duration {
	if !s.flood.enforced() {
		return 0
	}

	sc := s.lookupClientHandle(c.Identity())
	if sc == nil {
		return 0
	}

	delay, opened := sc.penalty.charge(s.flood, time.Now())
	if delay <= 0 {
		return 0
	}

	if opened {
		s.noticeThrottled(ctx, sc)
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-ctx.Done():
	case <-sc.done:
	case <-s.writerStopped:
	}

	return delay
}

// throttleNoticeText is what a client is told as its commands start
// lagging.
const throttleNoticeText = "You are sending commands too fast; the server is holding them back. Slow down."

// noticeThrottled tells a client that its commands have started
// lagging and why. The notice goes to that client alone. Fanning it
// out to the channel would add traffic to a channel that is already
// carrying too much, which is the problem the throttle exists to
// bound.
//
// It names the status window, because a throttle belongs to the
// connection and not to any one channel.
//
// Nothing files it anywhere. The instance reply log holds an
// issuer's own lookup results, which a model replays as things it
// once asked for and still knows; a throttle is a condition that was
// true for a few seconds, and a model reading it back on every later
// turn would be told to slow down long after it already had. What
// the client carries forward is the delay itself, which it
// experienced.
func (s *Session) noticeThrottled(ctx context.Context, sc *serverClient) {
	s.deliverToClient(ctx, domain.InstanceID(sc.id), domain.SystemNotice{
		Target: domain.StatusChannelName,
		Text:   throttleNoticeText,
		At:     s.now(),
	})
}

// channelFloodWindow is the period a `+f` limit counts messages
// over. The mode parameter is a message count, so the window has to
// be a server constant for the two to mean anything together.
//
// The window is fixed, not sliding: it opens at the first message
// counted and closes a whole `channelFloodWindow` later, whatever
// arrived in between. A channel can therefore relay twice its limit
// across a boundary, by spending the end of one window and the start
// of the next. A sliding window would not, at the cost of keeping
// every message's timestamp; per-connection pacing already bounds
// the rate that burst can arrive at, so the cheaper shape is enough.
const channelFloodWindow = time.Minute

// channelFlood counts the messages each channel has relayed in the
// current flood window, for the channels that set `+f`. A channel
// without the mode is never counted.
//
// Windows are measured on the monotonic clock, for the reason
// [penaltyTimer] gives: `Session.now` stamps domain events and is
// commonly frozen at a fixed instant, and a window on a frozen clock
// would never reopen.
//
// The send gates are the only thing that reads or writes these
// counts, and they run on the command loop, so the counts move in
// command order. The mutex guards the map against a second reader
// arriving from somewhere else later: the map is session state, and
// its safety should not rest on where today's one caller happens to
// run.
type channelFlood struct {
	mu     sync.Mutex
	counts map[domain.ChannelName]floodWindow
}

// floodWindow is one channel's count for the window that opened at
// `openedAt`.
type floodWindow struct {
	openedAt time.Time
	messages int
}

func newChannelFlood() *channelFlood {
	return &channelFlood{counts: make(map[domain.ChannelName]floodWindow)}
}

// countMessage reports whether `ch` has room for one more message
// under a limit of `limit` messages per window, and counts the
// message when it has. A refused message is not counted: the channel
// never relayed it, so it does not spend the window's budget.
func (f *channelFlood) countMessage(ch domain.ChannelName, limit int) bool {
	now := time.Now()

	f.mu.Lock()
	defer f.mu.Unlock()

	w, ok := f.counts[ch]
	if !ok || now.Sub(w.openedAt) >= channelFloodWindow {
		w = floodWindow{openedAt: now}
	}

	if w.messages >= limit {
		f.counts[ch] = w

		return false
	}

	w.messages++
	f.counts[ch] = w

	return true
}

// forget drops a channel's count. A channel ends when its last
// occupant leaves (RFC 2811 §2), and a channel created again under
// the same name is a new channel, which starts with a fresh window.
func (f *channelFlood) forget(ch domain.ChannelName) {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.counts, ch)
}

// checkChannelFlood is the `+f` send gate. It counts one message
// against the channel's flood window and refuses with the
// ERR_CANNOTSENDTOCHAN shape (RFC 2812 numeric 404) when the window
// is full. That is the numeric an ircd returns for a message its
// flood mode will not relay, and it is already how `+m`, `+n` and
// `+q` refuse here.
//
// Deciding and counting are one step for the same reason
// [Session.checkJoinGates] consumes an invitation on a successful
// `+i` join: this gate is what decides the message is relayed, so
// this gate is where the channel's budget is spent.
//
// The refusal is stamped with `Session.now`, the clock every other
// domain event is stamped with. Only the window measurement reads
// the monotonic clock.
func (s *Session) checkChannelFlood(ch domain.ChannelName, modes domain.ChannelModes) error {
	if modes.FloodLimit <= 0 {
		return nil
	}

	if s.channelFlood.countMessage(ch, modes.FloodLimit) {
		return nil
	}

	return domain.CannotSendToChannelError{Channel: ch, Reason: domain.SendBlockFlood, At: s.now()}
}
