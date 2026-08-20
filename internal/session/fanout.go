package session

import (
	"context"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// subscriberSnapshot returns a stable copy of the subscriber set
// under the read lock so callers iterating it cannot race with a
// concurrent registration or deregistration. The returned slice's
// `*serverClient` pointers are shared with the registry.
func (s *Session) subscriberSnapshot() []*serverClient {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()

	snap := make([]*serverClient, 0, len(s.subscribers))
	for _, sub := range s.subscribers {
		snap = append(snap, sub)
	}

	// Go map iteration is randomised per process, which leaks into
	// per-fan-out delivery order: model-client dispatch goroutines
	// wake in different orders across runs, and the lifecycle
	// events they emit then interleave with the main goroutine's
	// emits in a non-deterministic order on the user-client's
	// buffered events channel. Sort by ClientID so fan-out
	// iteration is stable; the user-client (sentinel empty id)
	// always sorts first, then model-clients lexicographically by
	// instance id.
	slices.SortFunc(snap, func(a, b *serverClient) int {
		return strings.Compare(string(a.id), string(b.id))
	})

	return snap
}

// fanOutProtocol delivers a protocol event to every active
// subscription that should receive it. Each delivery is handed to
// the subscription and the call returns: a consumer that has
// stopped reading grows its own backlog and holds up nobody else.
// This is what lets the command loop fan out safely — a model that
// is mid-turn, and therefore not draining its events channel, can
// still be sent to while it waits on the loop for a command of its
// own.
//
// Chat traffic (PRIVMSG/Action) is delivered to every member of the
// target window except the sender during membership fan-out (RFC
// 2812 §3.3.1); a sender holding IRCv3 echo-message then receives a
// direct echo of its own line via [Session.echoToOriginator]. Other
// event types — JOIN, PART, MODE, TOPIC, NICK, etc. — are delivered
// to every member-subscriber including the originator, matching IRC's
// behaviour for those signals.
//
// Membership filtering keeps every client — the user-client
// included — from receiving events for windows it is not in. The
// user-client is a member of whatever it has joined, so the
// chat-screen renders exactly those windows.
func (s *Session) fanOutProtocol(ctx context.Context, pe domain.ProtocolEvent) {
	s.noteChatActivity(pe)

	suppressOriginator, sender := chatTrafficSender(pe)
	spanCtx := trace.SpanContextFromContext(ctx)

	// `+a` rewrites the visible nick on chat-traffic events to
	// [domain.AnonymousNick] (RFC 2811 §4.2.1) before delivery, so
	// even the channel's own members can't see who sent what. The
	// stored event retains the real From for audit. The actor-scoped
	// events are masked further down, per recipient, by
	// [maskActorEvent].
	pe = anonymiseIfNeeded(ctx, s, pe)

	// Actor-scoped events ([domain.Quit] and [domain.NickChange])
	// carry no target on the wire; the per-recipient channel list
	// is computed at fan-out time as the intersection of the
	// actor's live membership and each recipient's. Snapshot the
	// actor's channels once so the per-sub loop does not re-walk
	// the ordered map.
	actorChannels := actorChannelSnapshot(pe)
	anonymous := s.anonymousChannels(ctx, actorChannels)

	for _, sub := range s.subscriberSnapshot() {
		if suppressOriginator && sub.Identity() == sender {
			continue
		}

		targets := intersectActorTargets(sub, actorChannels)

		event, targets := maskActorEvent(pe, targets, anonymous)
		if event == nil {
			continue
		}

		if !sub.canReceive(event, targets) {
			continue
		}

		sub.enqueue(protocol.Delivery{
			Event:   event,
			Targets: targets,
			SpanCtx: spanCtx,
		})
	}

	if quit, ok := pe.(domain.Quit); ok {
		s.partAnonymousChannels(quit, anonymous, spanCtx)
	}

	if suppressOriginator {
		s.echoToOriginator(ctx, pe, sender, spanCtx)
	}
}

// anonymousChannels returns the subset of `channels` carrying `+a`.
// It is read once per fan-out so the per-subscriber loop compares
// against a small slice; an empty or nil argument reads nothing.
func (s *Session) anonymousChannels(ctx context.Context, channels []domain.ChannelName) []domain.ChannelName {
	var anonymous []domain.ChannelName

	for _, ch := range channels {
		if modes, ok := s.channelModes(ctx, ch); ok && modes.Anonymous {
			anonymous = append(anonymous, ch)
		}
	}

	return anonymous
}

// maskActorEvent applies `+a` to an actor-scoped event on its way to
// one recipient, and returns what that recipient should be sent
// along with the targets to send it under. A nil event means send
// nothing.
//
// A recipient that shares only anonymous channels with the actor may
// not learn who the actor is (RFC 2811 §4.2.1), so:
//
//   - a `QUIT` is withheld and the anonymous channels receive a
//     `PART` in its place, per [Session.partAnonymousChannels];
//   - a dispatch-lifecycle event loses its instance handle, which is
//     the only thing in it that names anybody, so the recipient
//     learns that something is happening and not who is doing it.
//
// A recipient that also shares a non-anonymous channel already knows
// the actor from there, and receives the event unmasked but scoped
// to those channels alone.
func maskActorEvent(pe domain.ProtocolEvent, targets, anonymous []domain.ChannelName) (domain.ProtocolEvent, []domain.ChannelName) {
	if len(anonymous) == 0 {
		return pe, targets
	}

	named := namedChannels(targets, anonymous)

	switch e := pe.(type) {
	case domain.Quit:
		if len(named) == 0 {
			return nil, nil
		}

		return e, named

	case domain.ModelDispatchStarted:
		if len(named) == 0 {
			e.Instance = nil
		}

		return e, targets

	case domain.ModelDispatchDone:
		if len(named) == 0 {
			e.Instance = nil
		}

		return e, targets
	}

	return pe, targets
}

// namedChannels returns the members of `channels` that do not carry
// `+a`. An actor-scoped event may name its actor only on these:
// everywhere else RFC 2811 §4.2.1 has the mask stand in. An empty
// result means there is nowhere the actor can be named, which is
// what [Session.quitAs] asks before falling back to a direct
// delivery.
func namedChannels(channels, anonymous []domain.ChannelName) []domain.ChannelName {
	var named []domain.ChannelName

	for _, ch := range channels {
		if !slices.Contains(anonymous, ch) {
			named = append(named, ch)
		}
	}

	return named
}

// partAnonymousChannels delivers a `PART` to each anonymous channel
// the quitting actor was in, which is what RFC 2811 §4.2.1 puts on
// an anonymous channel in place of a `QUIT`: a member sees somebody
// leave the channel and cannot tell that they left the server. The
// departing nick is the `+a` mask, matching what every message in
// the channel was already attributed to.
//
// The actor's own subscription is among the recipients, so the
// quitting client sees the same masked departure its peers do.
func (s *Session) partAnonymousChannels(quit domain.Quit, anonymous []domain.ChannelName, spanCtx trace.SpanContext) {
	if len(anonymous) == 0 {
		return
	}

	subs := s.subscriberSnapshot()

	for _, ch := range anonymous {
		part := domain.Part{
			Target:  ch,
			Nick:    domain.AnonymousNick,
			Message: quit.Message,
			At:      quit.At,
		}

		for _, sub := range subs {
			if !sub.canReceive(part, nil) {
				continue
			}

			sub.enqueue(protocol.Delivery{Event: part, SpanCtx: spanCtx})
		}
	}
}

// echoToOriginator delivers chat traffic back to its sender when the
// sender holds echo-message (IRCv3). The echo is a direct send to the
// originating subscription: a message is keyed for its recipient
// window — a DM's target is the counterpart's id — so the sender's
// own copy cannot ride the membership filter. A sender without the
// capability (every model) gets nothing, keeping RFC 2812 §3.3.1
// no-self-echo.
func (s *Session) echoToOriginator(_ context.Context, pe domain.ProtocolEvent, sender protocol.ClientID, spanCtx trace.SpanContext) {
	origin := s.lookupClientHandle(sender)
	if origin == nil || !origin.echo {
		return
	}

	origin.enqueue(protocol.Delivery{Event: pe, SpanCtx: spanCtx})
}

// anonymiseIfNeeded rewrites a chat-traffic event's `From` field
// to `"anonymous"` when the target channel carries `+a`. Returns
// the event unchanged when the channel is not anonymous or when
// the event is not chat traffic. The mode set comes from the
// session's live channel state, which is what makes this
// affordable on every message the server fans out.
func anonymiseIfNeeded(ctx context.Context, s *Session, pe domain.ProtocolEvent) domain.ProtocolEvent {
	msg, ok := pe.(domain.Message)
	if !ok {
		return pe
	}

	if domain.InferChannelKind(msg.Target) != domain.KindChannel {
		return pe
	}

	modes, ok := s.channelModes(ctx, msg.Target)
	if !ok || !modes.Anonymous {
		return pe
	}

	msg.From = domain.AnonymousNick
	return msg
}

// actorChannelSnapshot returns the actor's channel set if `pe` is
// an actor-scoped event, or nil otherwise. The snapshot is read
// once per fan-out under the assumption that
// [Session.propagateActorEvent] has not yet run its post-emit
// `MutateChannels`; per-sub callers iterate the slice instead of
// re-walking the ordered map.
func actorChannelSnapshot(pe domain.ProtocolEvent) []domain.ChannelName {
	var actor *domain.Instance

	switch e := pe.(type) {
	case domain.Quit:
		actor = e.Instance
	case domain.NickChange:
		actor = e.Instance
	case domain.ModelDispatchStarted:
		actor = e.Instance
	case domain.ModelDispatchDone:
		actor = e.Instance
	default:
		return nil
	}

	if actor == nil {
		return nil
	}

	channels := actor.Channels()
	if channels == nil {
		return nil
	}

	names := make([]domain.ChannelName, 0, channels.Len())
	for pair := channels.Oldest(); pair != nil; pair = pair.Next() {
		names = append(names, pair.Key)
	}

	return names
}

// intersectActorTargets returns the recipient-visible channel
// list for an actor-scoped event: those channels in
// `actorChannels` that `sub` is also a member of. The user-client
// uses the same intersection as a model-client — it sees the actor
// move only in the windows the two share. Window-scoped events pass
// `actorChannels == nil` and receive a nil result.
func intersectActorTargets(sub *serverClient, actorChannels []domain.ChannelName) []domain.ChannelName {
	if len(actorChannels) == 0 {
		return nil
	}

	var out []domain.ChannelName
	for _, ch := range actorChannels {
		if sub.instance.InChannel(ch) {
			out = append(out, ch)
		}
	}

	return out
}

// chatTrafficSender reports whether `ev` carries the
// originator-suppression rule (PRIVMSG/Action), and returns the
// sender's [protocol.ClientID] when it does. The empty client id
// returned alongside `false` is unused and never compared.
//
// Today only [domain.Message] (covering both PRIVMSG and `/me`
// via [domain.Message.Action]) qualifies. Future event types
// needing the same rule add a switch arm here.
func chatTrafficSender(ev domain.ProtocolEvent) (suppress bool, sender protocol.ClientID) {
	if msg, ok := ev.(domain.Message); ok {
		return true, protocol.ClientID(msg.InstanceID)
	}

	return false, ""
}
