package session

import (
	"context"
	"sync"
	"time"

	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// channelMembership is the ordered channel-set carried by a model
// instance. It mirrors [domain.Instance.Channels]'s return type and
// is aliased here so the membership helpers below have a tight
// type to work against.
type channelMembership = *orderedmap.OrderedMap[domain.ChannelName, time.Time]

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

	modesMu sync.RWMutex
	modes   map[domain.Mode]struct{}
}

// newServerClient constructs a subscription with the given identity
// and actor instance. Modes start empty — the user-client is promoted
// via [Session.New]'s bootstrap call to `setUserModeAs`; future model
// elevation flows through [protocol.Oper] via the dispatcher.
func newServerClient(sess *Session, id protocol.ClientID, inst *domain.Instance) *serverClient {
	return &serverClient{
		sess:     sess,
		id:       id,
		instance: inst,
		events:   make(chan protocol.Delivery, eventBufSize),
		done:     make(chan struct{}),
		modes:    make(map[domain.Mode]struct{}),
	}
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
// [domain.ModeChange].
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
// `ev`. The user-client sees every event so the chat-screen renders
// the full session view. A model-client receives only events whose
// target window it is a member of, or actor-scoped events (Quit,
// NickChange) where the recipient shares any channel with the
// actor — RFC 2812 §3.3.1's intersection rule. `actorTargets` is
// the per-recipient intersection that [Session.fanOutProtocol]
// computed for this fan-out; it is non-empty exactly when the actor
// and `c` share at least one channel, so the test for actor-scoped
// delivery is just a length check.
func (c *serverClient) canReceive(ev domain.ProtocolEvent, actorTargets []domain.ChannelName) bool {
	if c.id == protocol.UserClientID {
		return true
	}

	channels := c.instance.Channels()
	if channels == nil {
		return false
	}

	switch e := ev.(type) {
	case domain.Message:
		return modelTargetsThis(c, e.Target)
	case domain.Join:
		return channelsContains(channels, e.Target)
	case domain.Part:
		return channelsContains(channels, e.Target)
	case domain.TopicChange:
		return channelsContains(channels, e.Target)
	case domain.TopicInfo:
		return channelsContains(channels, e.Target)
	case domain.ModeChange:
		return channelsContains(channels, e.Target)
	case domain.ModelKicked:
		return channelsContains(channels, e.Target)
	case domain.Quit, domain.NickChange, domain.ModelDispatchStarted, domain.ModelDispatchDone:
		_ = e
		return len(actorTargets) > 0
	case domain.PokeEvent:
		return channelsContains(channels, e.Channel)
	case domain.NamesReplyEvent:
		return channelsContains(channels, e.Channel)
	case domain.NamesEnd:
		return channelsContains(channels, e.Channel)
	}

	// Server-narrated and lifecycle events (FocusChannelEvent,
	// Help, Whois, ListReply, ListEnd, SystemNotice, CommandError,
	// UsageHint, PersonasList, Killed) have no model-side
	// rendering; they belong to the chat-screen.
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

	channels := c.instance.Channels()
	if channels == nil {
		return false
	}

	return channelsContains(channels, target)
}

func channelsContains(channels channelMembership, target domain.ChannelName) bool {
	if channels == nil {
		return false
	}

	_, ok := channels.Get(target)
	return ok
}
