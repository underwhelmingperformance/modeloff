package session

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"time"

	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// modelHistorySize caps the per-(model-client, channel) rolling
// history buffer at 500 events. The LLM's context window dictates
// this bound regardless of where the events come from.
const modelHistorySize = 500

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
// pointer is set at construction for model-clients (nil for the
// user-client) and is used by [Session.subscriberCanReceive] to
// consult channel membership without a per-event store lookup.
type serverClient struct {
	sess     *Session
	id       protocol.ClientID
	instance *domain.Instance
	events   chan protocol.Delivery

	modesMu sync.RWMutex
	modes   map[domain.Mode]struct{}

	// history holds the per-channel rolling buffer this model
	// uses to construct each dispatch turn's prompt. Channels are
	// eager-seeded at registration ([Session.ensureModelClient]);
	// DM targets are lazy-seeded on first event arrival, both
	// under `historyMu` so no concurrent appender can interleave
	// with a seed. The user-client allocates a nil map — it never
	// dispatches, so it never reads or writes here.
	historyMu sync.Mutex
	history   map[domain.ChannelName][]domain.StoredEvent
}

// newServerClient constructs a subscription with the given identity.
// `inst` may be nil for the user-client; model-clients hold the
// canonical handle so membership lookups stay in-process. Modes
// start empty — the user-client is promoted via [Session.New]'s
// bootstrap call to `setUserModeAs`; future model elevation flows
// through [protocol.Oper] via the dispatcher.
func newServerClient(sess *Session, id protocol.ClientID, inst *domain.Instance) *serverClient {
	c := &serverClient{
		sess:     sess,
		id:       id,
		instance: inst,
		events:   make(chan protocol.Delivery, eventBufSize),
		modes:    make(map[domain.Mode]struct{}),
	}

	// Only model-clients run a dispatch loop, so only model-clients
	// need a history buffer. The user-client has no instance and
	// never reads from `history`.
	if inst != nil {
		c.history = make(map[domain.ChannelName][]domain.StoredEvent)
	}

	return c
}

func (c *serverClient) Identity() protocol.ClientID { return c.id }

func (c *serverClient) Send(ctx context.Context, cmd protocol.Command) (protocol.Response, error) {
	return c.sess.Handle(ctx, c, cmd)
}

func (c *serverClient) Events() <-chan protocol.Delivery { return c.events }

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
func (c *serverClient) Has(cap command.Capability) bool {
	switch cap {
	case protocol.CapOperator:
		return c.HasMode(domain.ModeOperator)
	default:
		return false
	}
}

// canReceive reports whether this subscription should receive
// `ev`. The user-client (no backing instance) sees every event so
// the chat-screen renders the full session view. A model-client
// receives only events whose target window it is a member of, or
// actor-scoped events (Quit, NickChange) where the recipient
// shares any channel with the actor — RFC 2812 §3.3.1's
// intersection rule. `actorTargets` is the per-recipient
// intersection that [Session.fanOutProtocol] computed for this
// fan-out; it is non-empty exactly when the actor and `c` share
// at least one channel, so the test for actor-scoped delivery is
// just a length check.
func (c *serverClient) canReceive(ev domain.ProtocolEvent, actorTargets []domain.ChannelName) bool {
	if c.instance == nil {
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
	case domain.ModelInvited:
		// The freshly-invited model needs to see its own invite to
		// trigger its first dispatch turn; other models in the
		// channel see it for member-list refresh purposes.
		return e.InstanceID == c.id || channelsContains(channels, e.Target)
	case domain.ModelKicked:
		return channelsContains(channels, e.Target)
	case domain.Quit, domain.NickChange, domain.ModelDispatchStarted, domain.ModelDispatchDone:
		_ = e
		return len(actorTargets) > 0
	case domain.PokeEvent:
		return channelsContains(channels, e.Channel)
	case domain.NamesReplyEvent:
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

// appendHistory records `ev` against `target` in the model-client's
// rolling buffer. Events the LLM never sees in its prompt
// (`!ModelVisible`) are skipped so the buffer's trim cap reflects
// turns of conversation rather than wire chatter.
//
// On first sight of a DM target — `target` is a counterpart
// `InstanceID` and the buffer has no entry for it yet — the
// method lazy-seeds from the store under the same lock the live
// append takes, so no concurrent appender can interleave between
// seed and append. Channel targets are eager-seeded at
// registration time; the lazy-seed branch is DM-only.
//
// Skips a duplicate if the incoming event matches the buffer's
// most-recent entry by concrete type and timestamp; protects
// against the seed-then-live-emit race where a producer persists
// and is mid-fan-out while a concurrent registration's seed reads
// the event from the store and then receives the same event again
// via fan-out.
//
// The buffer trims to [modelHistorySize] from the older end on
// every append so a chatty target cannot grow it without bound.
func (c *serverClient) appendHistory(ctx context.Context, ev domain.StoredEvent, target domain.ChannelName) {
	if c.history == nil {
		return
	}

	if !ev.Event.ModelVisible() {
		return
	}

	c.historyMu.Lock()
	defer c.historyMu.Unlock()

	if _, ok := c.history[target]; !ok && domain.InferChannelKind(target) == domain.KindDM {
		seed, err := c.sess.store.DMEventsBefore(ctx, c.id, domain.InstanceID(target), nil, modelHistorySize)
		if err != nil {
			slog.Default().ErrorContext(ctx, "lazy-seed DM history",
				"component", "session",
				"instance_id", c.id,
				"peer", target,
				"error", err,
			)
			c.history[target] = nil
		} else {
			c.history[target] = seed
		}
	}

	if buf := c.history[target]; len(buf) > 0 && sameStoredEvent(buf[len(buf)-1], ev) {
		return
	}

	c.history[target] = append(c.history[target], ev)
	if len(c.history[target]) > modelHistorySize {
		c.history[target] = c.history[target][len(c.history[target])-modelHistorySize:]
	}
}

// sameStoredEvent reports whether `a` and `b` represent the same
// persisted event. The store-loaded form (from `EventsBefore` /
// `DMEventsBefore`) carries the row's ID; the fan-out form is
// constructed without the ID since the wire layer does not
// propagate it. Compare on the (type, timestamp) tuple instead:
// two events of the same concrete type at the same nanosecond
// timestamp are not realistically distinct, and a storeload-then-
// fanout duplicate has both attributes identical by construction.
func sameStoredEvent(a, b domain.StoredEvent) bool {
	if a.ID != 0 && b.ID != 0 {
		return a.ID == b.ID
	}

	if a.Event == nil || b.Event == nil {
		return false
	}

	if reflect.TypeOf(a.Event) != reflect.TypeOf(b.Event) {
		return false
	}

	return domain.EventTime(a.Event).Equal(domain.EventTime(b.Event))
}

// snapshotHistory returns a defensive copy of the buffer for
// `target`. The dispatch turn iterates the slice without holding
// the lock, so the snapshot must not alias the live backing array.
func (c *serverClient) snapshotHistory(target domain.ChannelName) []domain.StoredEvent {
	if c.history == nil {
		return nil
	}

	c.historyMu.Lock()
	defer c.historyMu.Unlock()

	src := c.history[target]
	if len(src) == 0 {
		return nil
	}

	dst := make([]domain.StoredEvent, len(src))
	copy(dst, src)
	return dst
}
