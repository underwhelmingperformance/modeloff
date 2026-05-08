package session

import (
	"context"
	"time"

	orderedmap "github.com/wk8/go-ordered-map/v2"

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
// The mode set is constructed once and never mutated; reads need no
// synchronisation. The `instance` pointer is set at construction
// for model-clients (nil for the user-client) and is used by
// [Session.subscriberCanReceive] to consult channel membership
// without a per-event store lookup.
type serverClient struct {
	sess     *Session
	id       protocol.ClientID
	instance *domain.Instance
	events   chan protocol.Delivery
	modes    map[protocol.UserMode]struct{}
}

// newServerClient constructs a subscription with the given identity
// and modes. `inst` may be nil for the user-client; model-clients
// hold the canonical handle so membership lookups stay in-process.
func newServerClient(sess *Session, id protocol.ClientID, inst *domain.Instance, modes ...protocol.UserMode) *serverClient {
	modeSet := make(map[protocol.UserMode]struct{}, len(modes))
	for _, m := range modes {
		modeSet[m] = struct{}{}
	}

	return &serverClient{
		sess:     sess,
		id:       id,
		instance: inst,
		events:   make(chan protocol.Delivery, eventBufSize),
		modes:    modeSet,
	}
}

func (c *serverClient) Identity() protocol.ClientID { return c.id }

func (c *serverClient) Send(ctx context.Context, cmd protocol.Command) (protocol.Response, error) {
	return c.sess.Handle(ctx, c, cmd)
}

func (c *serverClient) Events() <-chan protocol.Delivery { return c.events }

func (c *serverClient) HasMode(m protocol.UserMode) bool {
	_, ok := c.modes[m]
	return ok
}

// canReceive reports whether this subscription should receive
// `ev`. The user-client (no backing instance) sees every event so
// the chat-screen renders the full session view. A model-client
// receives only events whose target window it is a member of, or
// actor-scoped events (Quit, NickChange) that touch a channel it
// shares with the actor.
func (c *serverClient) canReceive(ev domain.ProtocolEvent) bool {
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
	case domain.Quit:
		return anyChannelInCommon(channels, e.Channels)
	case domain.NickChange:
		return anyChannelInCommon(channels, e.Channels)
	case domain.PokeEvent:
		return channelsContains(channels, e.Channel)
	case domain.NamesReplyEvent:
		return channelsContains(channels, e.Channel)
	}

	// Server-narrated and lifecycle events (DispatchStartedEvent,
	// DispatchDoneEvent, FocusChannelEvent, StatusOpenedEvent, Help,
	// Whois, ListReply, ListEnd, SystemNotice, CommandError,
	// UsageHint, PersonasList, Killed) have no model-side rendering;
	// they belong to the chat-screen.
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

func anyChannelInCommon(membership channelMembership, candidates []domain.ChannelName) bool {
	for _, ch := range candidates {
		if _, ok := membership.Get(ch); ok {
			return true
		}
	}

	return false
}
