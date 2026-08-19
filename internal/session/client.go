package session

import (
	"context"
	"sync"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// serverClient is the session-side concrete implementation of
// [protocol.Client]. One instance per subscription: the user-client
// is created at session bootstrap. The struct keeps a back-reference
// to its owning session so `Send` can route through [Session.Handle].
//
// The mode set is guarded by `modesMu`: `HasMode` and `Has` take
// the read lock, `setMode` takes the write lock. The `instance`
// pointer is set at construction and is the canonical actor handle
// the dispatcher reads via [Session.resolveClientActor] — no
// per-command store lookup.
//
// No producer ever waits on a consumer. Producers — the command
// loop, the poke scheduler, a model-client's own emissions — hand a
// delivery to `enqueue` and return; when the client is behind, the
// delivery lands in `outbox` and `pump` is what waits on `events`.
// That is what keeps a client that has stopped reading from
// stalling the server.
type serverClient struct {
	sess     *Session
	id       protocol.ClientID
	instance *domain.Instance
	events   chan protocol.Delivery

	// done closes exactly once when the subscription is reaped,
	// from any source: client-initiated via [serverClient.Unsubscribe],
	// session-initiated via QUIT / KILL through [Session.reapClient],
	// or shutdown. `unsubOnce` serialises the close so the channel
	// is never closed twice and never written to. Consumers that
	// long-poll on `Events` select on `done` to exit cleanly.
	done      chan struct{}
	unsubOnce sync.Once

	// outbox is this subscription's send queue, guarded by `outMu`.
	// It is unbounded, so a backlog survives a consumer that has
	// fallen behind for as long as the subscription does: what the
	// queue still holds is released when the subscription is reaped
	// or the session shuts down, and only then. The bound on its
	// growth arrives with flood control, which disconnects a client
	// whose queue exceeds its allowance (RFC 1459 §8.10) — a
	// visible KILL-shaped ending in place of a silent gap in the
	// transcript.
	//
	// `outWake` carries a single coalesced wake-up: producers offer
	// one after appending and the pump drains the queue dry on each
	// wake, so a burst of appends costs at most one signal.
	outMu     sync.Mutex
	outbox    []protocol.Delivery
	outWake   chan struct{}
	outClosed bool

	// pumpDone closes when this subscription's pump goroutine
	// exits, so [Session.reapClient] and [Session.Shutdown] can join
	// it before returning.
	pumpDone chan struct{}

	modesMu sync.RWMutex
	modes   map[domain.Mode]struct{}

	// echo grants IRCv3 echo-message: the client's own chat traffic
	// is delivered back to it (see [Session.fanOutProtocol]). Set once
	// at subscribe time from [protocol.SubscribeOptions.EchoMessage].
	echo bool
}

// newServerClient constructs a subscription with the given identity
// and actor instance, and starts its outbound pump. Modes start
// empty — the user-client is promoted via [Session.New]'s bootstrap
// call to `setUserModeAs`; future model elevation flows through
// [protocol.Oper] via the dispatcher.
//
// `stop` ends the pump for a subscription that is never reaped: the
// user-client lives for the session, so its pump exits on the
// session's shutdown gate.
func newServerClient(sess *Session, id protocol.ClientID, inst *domain.Instance, stop <-chan struct{}) *serverClient {
	c := &serverClient{
		sess:     sess,
		id:       id,
		instance: inst,
		events:   make(chan protocol.Delivery, eventBufSize),
		done:     make(chan struct{}),
		outWake:  make(chan struct{}, 1),
		pumpDone: make(chan struct{}),
		modes:    make(map[domain.Mode]struct{}),
	}

	go c.pump(stop)

	return c
}

// enqueue accepts `delivery` for this subscription and returns. It
// never blocks: the queue is unbounded, so a producer's progress
// does not depend on the consumer's.
//
// While the client is keeping up — nothing queued and room in its
// channel — the delivery goes straight across, so a client that
// reads promptly sees an event as soon as the command that raised
// it returns. The queue takes over the moment either is untrue, and
// from then on the pump owns delivery until it drains. Ordering
// holds across the switch because the direct path is taken only
// when the queue is empty, and the pump keeps a delivery queued
// until its send completes.
//
// A reaped subscription accepts nothing and drops what it holds:
// the client it addressed has gone, so there is nobody left to read
// either. The same goes for whatever is still queued when the
// session shuts down.
func (c *serverClient) enqueue(delivery protocol.Delivery) {
	c.outMu.Lock()
	defer c.outMu.Unlock()

	if c.outClosed {
		return
	}

	if len(c.outbox) == 0 {
		select {
		case c.events <- delivery:
			return
		default:
		}
	}

	c.outbox = append(c.outbox, delivery)

	select {
	case c.outWake <- struct{}{}:
	default:
	}
}

// pump is the subscription's outbound goroutine. It moves queued
// deliveries into `events` in order, blocking there for as long as
// the consumer needs, and exits when the subscription is reaped or
// the session shuts down.
//
// The head stays on the queue until the send completes, so a
// producer appending mid-send always lands behind it and the
// consumer sees the server's order.
func (c *serverClient) pump(stop <-chan struct{}) {
	defer close(c.pumpDone)

	for {
		delivery, ok := c.peek()
		if !ok {
			select {
			case <-c.outWake:
				continue
			case <-c.done:
				return
			case <-stop:
				return
			}
		}

		select {
		case c.events <- delivery:
			c.advance()
		case <-c.done:
			return
		case <-stop:
			return
		}
	}
}

// peek returns the delivery at the head of the queue, or false when
// the queue is empty.
func (c *serverClient) peek() (protocol.Delivery, bool) {
	c.outMu.Lock()
	defer c.outMu.Unlock()

	if len(c.outbox) == 0 {
		return protocol.Delivery{}, false
	}

	return c.outbox[0], true
}

// advance drops the delivered head. Emptying the queue releases the
// backing array so a burst does not pin its peak size for the life
// of the subscription.
//
// The queue can be empty here: a send completing at the moment the
// subscription is reaped leaves both arms of the pump's select
// ready, and `closeOutbound` may already have released the queue.
// The delivery landed either way, so there is nothing left to drop.
func (c *serverClient) advance() {
	c.outMu.Lock()
	defer c.outMu.Unlock()

	if len(c.outbox) == 0 {
		return
	}

	c.outbox = c.outbox[1:]
	if len(c.outbox) == 0 {
		c.outbox = nil
	}
}

// closeOutbound refuses further deliveries and releases whatever is
// still queued. Called as the subscription is reaped, once the
// client it addressed has gone.
func (c *serverClient) closeOutbound() {
	c.outMu.Lock()
	defer c.outMu.Unlock()

	c.outClosed = true
	c.outbox = nil
}

func (c *serverClient) Identity() protocol.ClientID { return c.id }

func (c *serverClient) Send(ctx context.Context, cmd protocol.Command) (protocol.Response, error) {
	return c.sess.Handle(ctx, c, cmd)
}

func (c *serverClient) Events() <-chan protocol.Delivery { return c.events }

// Done returns a channel closed when the subscription is reaped.
// The user-client's `done` channel is allocated but never closed —
// the user-client lives for the session's lifetime.
func (c *serverClient) Done() <-chan struct{} { return c.done }

// Unsubscribe removes the client from the session's subscriber
// registry and closes [Done]. The user-client never reaps — its
// lifetime equals the session. Idempotent across concurrent
// callers via `unsubOnce`.
func (c *serverClient) Unsubscribe() { c.sess.reapClient(c.id) }

func (c *serverClient) HasMode(m domain.Mode) bool {
	c.modesMu.RLock()
	defer c.modesMu.RUnlock()

	_, ok := c.modes[m]
	return ok
}

// setMode adds or clears a single mode flag under the write lock.
// Idempotent: a grant for an already-held mode (or a clear for an
// unheld mode) is a no-op. Returns true if the call mutated state —
// actor methods use it to decide whether to emit a
// [domain.UserModeChange].
func (c *serverClient) setMode(m domain.Mode, add bool) bool {
	c.modesMu.Lock()
	defer c.modesMu.Unlock()

	_, present := c.modes[m]

	if add {
		if present {
			return false
		}
		c.modes[m] = struct{}{}
		return true
	}

	if !present {
		return false
	}
	delete(c.modes, m)
	return true
}

// Caps returns the client as a [command.CapabilityHolder] bound to
// live state. Each call to [command.CapabilityHolder.Has] re-reads
// the current mode set, so a mode mutation is reflected on the
// next consultation by the suggestion filter or the tool registry.
func (c *serverClient) Caps() command.CapabilityHolder { return c }

// Has implements [command.CapabilityHolder]. Adding a new capability
// that maps to a mode requires both a [protocol] constant and a
// new case here.
func (c *serverClient) Has(capability command.Capability) bool {
	switch capability {
	case protocol.CapOperator:
		return c.HasMode(domain.ModeOperator)
	default:
		return false
	}
}

// canReceive reports whether this subscription should receive
// `ev`. Both kinds of client ride the same filter: a subscription
// receives only events whose target window it is a member of, or
// actor-scoped events (Quit, NickChange) where the recipient shares
// any channel with the actor — RFC 2812 §3.3.1's intersection rule.
// The user-client is a member of whatever it has joined, so the
// chat-screen renders exactly those windows; server handshake
// numerics and command replies reach it point-to-point (via
// [Session.deliverToClient] or the issuing command's
// `Response.Events`), not through this filter. `actorTargets` is the
// per-recipient intersection that [Session.fanOutProtocol] computed
// for this fan-out; it is non-empty exactly when the actor and `c`
// share at least one channel, so the test for actor-scoped delivery
// is just a length check.
func (c *serverClient) canReceive(ev domain.ProtocolEvent, actorTargets []domain.ChannelName) bool {
	switch e := ev.(type) {
	case domain.Message:
		return modelTargetsThis(c, e.Target)
	case domain.Join:
		return c.instance.InChannel(e.Target)
	case domain.Part:
		return c.instance.InChannel(e.Target)
	case domain.TopicChange:
		return c.instance.InChannel(e.Target)
	case domain.TopicInfo:
		return c.instance.InChannel(e.Target)
	case domain.ChannelModeChange:
		return c.instance.InChannel(e.Target)
	case domain.Kicked:
		return c.instance.InChannel(e.Target)
	case domain.Quit, domain.NickChange, domain.ModelDispatchStarted, domain.ModelDispatchDone:
		_ = e
		return len(actorTargets) > 0
	case domain.PokeEvent:
		return c.instance.InChannel(e.Channel)
	case domain.NamesReplyEvent:
		return c.instance.InChannel(e.Channel)
	case domain.NamesEnd:
		return c.instance.InChannel(e.Channel)
	case domain.ModelUnavailableError:
		_ = e
		// Dispatch failures are operator diagnostics, rendered in the
		// operator's status window; an operator subscription receives
		// them across every window, channel and DM alike.
		return c.HasMode(domain.ModeOperator)
	}

	// Server handshake numerics (Welcome, Reconnected) and the
	// point-to-point command replies the session emits (Whois,
	// ListReply, ListEnd, the invite-failure SystemNotice) reach the
	// issuing client through [Session.deliverToClient] or the
	// command's `Response.Events`. Help, UsageHint, PersonasList and
	// CommandError are chat-screen-local control signals the session
	// never puts on this bus.
	return false
}

// modelTargetsThis reports whether a [domain.Message] target
// addresses this model — either its own DM (target equals the
// instance id) or a channel it is in. The sender side is gated by
// the echo helper, not here.
func modelTargetsThis(c *serverClient, target domain.ChannelName) bool {
	if domain.InferChannelKind(target) == domain.KindDM {
		return target == domain.ChannelName(c.id)
	}

	return c.instance.InChannel(target)
}
