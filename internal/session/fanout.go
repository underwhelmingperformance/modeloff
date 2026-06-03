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
// subscription that should receive it. Sends are blocking,
// matching the back-pressure discipline of `s.events`: a stuck
// consumer surfaces as a wedged producer rather than silent data
// loss. Each subscription's events channel is buffered to
// [eventBufSize]; callers should attach a consumer before
// bootstrap-time emission exceeds that capacity.
//
// PRIVMSG/Action events do not echo back to their originator
// (RFC 2812 §3.3.1: chat traffic is delivered to every member of
// the target window except the sender). Other event types — JOIN,
// PART, MODE, TOPIC, NICK, etc. — are delivered to every
// member-subscriber including the originator, matching IRC's
// behaviour for those signals.
//
// Membership filtering keeps every client — the user-client
// included — from receiving events for windows it is not in. The
// user-client is a member of whatever it has joined, so the
// chat-screen renders exactly those windows.
//
// The send-side select gates only on `ctx.Done()`: cancelling the
// supplier ctx propagates to every in-flight handler's ctx, so a
// blocked send aborts when shutdown begins, even if its target
// dispatch goroutine has already exited.
func (s *Session) fanOutProtocol(ctx context.Context, pe domain.ProtocolEvent) {
	s.noteChatActivity(pe)

	suppressOriginator, sender := chatTrafficSender(pe)
	spanCtx := trace.SpanContextFromContext(ctx)

	// `+a` rewrites the visible nick on chat-traffic events to the
	// `"anonymous"` sentinel (RFC 2811 §4.2.1) before delivery, so
	// even the channel's own members can't see who sent what. The
	// stored event retains the real From for audit.
	pe = anonymiseIfNeeded(ctx, s, pe)

	// Actor-scoped events ([domain.Quit] and [domain.NickChange])
	// carry no target on the wire; the per-recipient channel list
	// is computed at fan-out time as the intersection of the
	// actor's live membership and each recipient's. Snapshot the
	// actor's channels once so the per-sub loop does not re-walk
	// the ordered map.
	actorChannels := actorChannelSnapshot(pe)

	for _, sub := range s.subscriberSnapshot() {
		if suppressOriginator && sub.Identity() == sender {
			continue
		}

		targets := intersectActorTargets(sub, actorChannels)
		if !sub.canReceive(pe, targets) {
			continue
		}

		select {
		case sub.events <- protocol.Delivery{
			Event:   pe,
			Targets: targets,
			SpanCtx: spanCtx,
		}:
		case <-sub.done:
			// Subscription was reaped between the snapshot and the
			// send. The recipient is gone; drop the delivery.
		case <-ctx.Done():
			return
		}
	}
}

// anonymiseIfNeeded rewrites a chat-traffic event's `From` field
// to `"anonymous"` when the target channel carries `+a`. Returns
// the event unchanged when the channel is not anonymous or when
// the event is not chat traffic.
func anonymiseIfNeeded(ctx context.Context, s *Session, pe domain.ProtocolEvent) domain.ProtocolEvent {
	msg, ok := pe.(domain.Message)
	if !ok {
		return pe
	}

	if domain.InferChannelKind(msg.Target) != domain.KindChannel {
		return pe
	}

	window, err := s.loadChannelWindow(ctx, msg.Target)
	if err != nil || !window.Modes.Anonymous {
		return pe
	}

	msg.From = "anonymous"
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

	subChannels := sub.instance.Channels()
	if subChannels == nil {
		return nil
	}

	var out []domain.ChannelName
	for _, ch := range actorChannels {
		if _, ok := subChannels.Get(ch); ok {
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
