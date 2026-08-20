package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	orderedmap "github.com/wk8/go-ordered-map/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

// joinAs joins the given actor to a channel. `key` carries the
// channel password for keyed (`+k`) channels — empty for unkeyed
// joins. `+i`, `+l`, and `+k` gate the add against an existing
// channel; a fresh channel (this call creates it) has no modes
// and so no gate applies.
//
// `kind` says on whose authority the join is happening, which is
// what decides whether `+i` admits it. See [joinKind].
//
// The first return is the channel's canonical name: the spelling
// under which the channel actually exists, which may differ in case
// from the one the client asked for. Callers that report the join
// back to the client use it, so a `/join #Dev` that reached `#dev`
// says so.
//
// Runs on the session's command loop, so the load-mutate-commit of
// the channel record below is atomic against every other command.
//
//nolint:gocognit // sequenced join steps (create-or-load, gate, op-grant, persist, broadcast, replies) read clearer inline than as further-extracted helpers.
func (s *Session) joinAs(ctx context.Context, actor *domain.Instance, kind joinKind, ch domain.ChannelName, key string) (domain.ChannelName, error) {
	ch = domain.NormaliseChannelName(ch)

	if reason := domain.ValidateChannelName(ch); reason != domain.ChannelNameAccepted {
		return ch, domain.ErroneousChannelNameError{Channel: ch, Reason: reason, At: s.now()}
	}

	actorNick := actor.Nick()

	err := s.inSpan(ctx, "session.join", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
		attribute.String(observability.AttrInstanceID, string(actor.ID())),
	}, func(ctx context.Context, _ trace.Span) error {
		now := s.now()
		isUser := actor.ID() == ""

		window, created, err := s.ensureChannelWindowWithActor(ctx, ch, actor, now)
		if err != nil {
			return err
		}

		// The channel record is the authority for how the name is
		// spelled: under the casemapping a JOIN for `#Dev` reaches an
		// existing `#dev`. Every key derived from here on uses the
		// spelling the channel was created with, so the event log,
		// the actor's channel set and the wire events all agree.
		ch = window.Name()

		alreadyMember := !created && window.Members.HasInstance(actor)

		if !created && !alreadyMember {
			if err := s.checkJoinGates(window, actor, kind, key); err != nil {
				return err
			}

			window.Members.Add(actor)

			if err := s.persistChannelWindow(ctx, window); err != nil {
				return fmt.Errorf("save channel: %w", err)
			}
		}

		// RFC 2811 §4.3: the JOIN that creates the channel auto-grants
		// the joiner `+o`. That is the only automatic `+o` grant the
		// server ever performs — the original creator parting and
		// rejoining gets nothing back; subsequent ops are granted only
		// by an existing op via wire `MODE +o`. The grant happens
		// here, before any wire event, so the Join echo and the
		// `RPL_NAMREPLY` that follow see the `+o` in the member list
		// (the `@` prefix in NAMES is how RFC 2812 §3.2.1 conveys the
		// new op's rank — there is no separate MODE message).
		if created {
			window.Members.ApplyMode(actor, domain.ModeOperator, true)
			if isUser {
				s.setUserModes(ctx, ch, domain.MemberModes{Operator: true})
			}

			if err := s.persistChannelWindow(ctx, window); err != nil {
				return fmt.Errorf("save channel after mode: %w", err)
			}
		}

		if !alreadyMember {
			if err := s.recordActorMembership(ctx, actor, ch, now, isUser); err != nil {
				return err
			}
		}

		if alreadyMember {
			return nil
		}

		s.persistAndEmit(ctx, ch, domain.Join{
			Target:     ch,
			Nick:       actorNick,
			InstanceID: actor.ID(),
			Created:    created,
			At:         now,
			Instance:   actor,
		})

		window, err = s.loadChannelWindow(ctx, ch)
		if err != nil {
			return fmt.Errorf("reload channel after join: %w", err)
		}

		// RFC 2812 §3.2.1 / §3.2.4: RPL_NAMREPLY and RPL_TOPIC are
		// sent only to the joiner. They are server-to-client
		// responses, not channel broadcasts, so they go directly to
		// the joiner's subscription via [Session.deliverToClient]. The
		// chat-screen consumes NamesReplyEvent to populate its
		// member-list cache when the user joins; the model-client's
		// dispatch loop files TopicInfo into history when a model
		// joins so the prompt knows who set the topic and when.
		//
		// On a `+a` channel the reply carries the mask alone (RFC
		// 2811 §4.2.1). Anyone may join an anonymous channel, so a
		// reply naming its members would be the way to ask who is on
		// one. The topic names no member and needs no masking.
		members := window.Members
		if window.Modes.Anonymous {
			members = domain.AnonymousMembers()
		}

		s.deliverToClient(ctx, actor.ID(), domain.NamesReplyEvent{
			Channel: ch,
			Members: members,
			At:      now,
		})

		s.deliverToClient(ctx, actor.ID(), domain.NamesEnd{
			Channel: ch,
			At:      now,
		})

		if window.Topic != "" {
			s.deliverToClient(ctx, actor.ID(), domain.TopicInfo{
				Target:     ch,
				Topic:      window.Topic,
				TopicSetBy: window.TopicSetBy,
				TopicSetAt: window.TopicSetAt,
				At:         now,
			})
		}

		return nil
	})

	return ch, err
}

// ensureChannelWindowWithActor loads the channel-window or creates
// a fresh one that already contains the actor. Returns the
// (possibly freshly-saved) `*ChannelWindow`, whether it was newly
// created, and any persistence error encountered along the way.
//
// A channel is created only when the load says the channel does not
// exist. Any other load failure is returned as-is: creating a fresh
// window on, say, a transient store failure would overwrite the
// live channel's topic, modes and invitation list with an empty
// record. joinAs is the only caller and is gated on `#`-prefixed
// names by `NormaliseChannelName`, so a load that returns a
// non-channel row indicates a programming error in the upstream
// guard; that too is returned.
//
// A freshly created channel starts with the session's default mode
// set (see [DefaultChannelModes]); a channel that already exists
// keeps the modes it has.
func (s *Session) ensureChannelWindowWithActor(ctx context.Context, ch domain.ChannelName, actor *domain.Instance, now time.Time) (*domain.ChannelWindow, bool, error) {
	window, err := s.loadChannelWindow(ctx, ch)
	if err == nil {
		return window, false, nil
	}

	if !errors.Is(err, store.ErrNoSuchChannel) {
		return nil, false, fmt.Errorf("load channel: %w", err)
	}

	window = domain.NewChannelWindow(ch, now)
	window.Modes = s.newChannelModes(ctx)
	window.Members.Add(actor)

	if saveErr := s.persistChannelWindow(ctx, window); saveErr != nil {
		return nil, false, fmt.Errorf("save channel: %w", saveErr)
	}

	return window, true, nil
}

// recordActorMembership stamps the channel onto the actor's joined-
// channels map and — for model actors — persists the updated
// instance row.
func (s *Session) recordActorMembership(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, now time.Time, isUser bool) error {
	actor.JoinChannel(ch, now)

	if isUser {
		return nil
	}

	if err := s.store.SaveInstance(ctx, actor); err != nil {
		return fmt.Errorf("save instance: %w", err)
	}

	return nil
}

// partAs parts the given actor from a channel.
func (s *Session) partAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, message string) error {
	actorNick := actor.Nick()

	return s.inSpan(ctx, "session.part", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
	}, func(ctx context.Context, span trace.Span) error {
		if domain.InferChannelKind(ch) != domain.KindChannel {
			return observability.ErrWithKind(fmt.Errorf("cannot part %s", ch), observability.ErrorKindValidation)
		}

		window, err := s.loadChannelWindow(ctx, ch)
		if err != nil {
			return fmt.Errorf("channel not found: %w", err)
		}

		ch = window.Name()

		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actor.ID())))

		if !window.Members.HasInstance(actor) {
			return domain.NotOnChannelError{Channel: ch, Command: "PART", At: s.now()}
		}

		// The PART is broadcast to the channel while `actor` is still a
		// member, then membership is dropped — RFC 2812 §3.2.2 order.
		// Emitting first is what lets the departing member receive its
		// own PART through the membership filter.
		now := s.now()
		s.persistAndEmit(ctx, ch, domain.Part{
			Target:     ch,
			Nick:       actorNick,
			InstanceID: actor.ID(),
			Message:    message,
			At:         now,
			Instance:   actor,
		})

		return s.removeMember(ctx, window, actor)
	})
}

// quitAs disconnects the given actor from every joined channel.
// For the user-client the QUIT lines are persisted but not
// broadcast, because the only consumer of broadcast events (the
// chat-screen) is about to tear down. For a model-client the call
// broadcasts the `domain.Quit` event to common-channel peers and
// deletes the instance row. The dispatcher reaps the subscription
// separately, via [Session.reapClient].
func (s *Session) quitAs(ctx context.Context, actor *domain.Instance, message string) error {
	if actor.ID() == "" {
		return s.userQuit(ctx, message)
	}

	return s.modelQuit(ctx, actor, message)
}

func (s *Session) modelQuit(ctx context.Context, actor *domain.Instance, message string) error {
	actorID := actor.ID()
	actorNick := actor.Nick()

	return s.inSpan(ctx, "session.quit", []attribute.KeyValue{
		attribute.String(observability.AttrNick, string(actorNick)),
	}, func(ctx context.Context, span trace.Span) error {
		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actorID)))

		now := s.now()
		channels := s.instanceChannelNames(actor)

		s.propagateActorEvent(ctx, actor, actorEventConfig{
			mutate: func(window *domain.ChannelWindow) {
				if m, ok := window.Members.GetByInstance(actor); ok {
					window.Members.Remove(m)
				}
			},
			build: func() broadcastEvent {
				return domain.Quit{
					Nick:       actorNick,
					InstanceID: actorID,
					Message:    message,
					At:         now,
					Instance:   actor,
				}
			},
		})

		actor.LeaveChannels(channels...)

		if err := s.store.SaveInstance(ctx, actor); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}

		if err := s.store.DeleteInstanceByID(ctx, actorID); err != nil {
			return fmt.Errorf("delete instance: %w", err)
		}

		return nil
	})
}

// changeNickAs changes the given actor's nickname. The grammar and
// collision checks are [Session.requireNickAvailable]'s, the same
// pair `ADDMODEL` runs before it claims a nick for a new instance.
//
// Runs on the session's command loop, which is what makes the
// collision check decisive: no other command can claim `newNick`
// between the check and the rename that takes it.
func (s *Session) changeNickAs(ctx context.Context, actor *domain.Instance, newNick domain.Nick) error {
	oldNick := actor.Nick()

	return s.inSpan(ctx, "session.change_nick", []attribute.KeyValue{
		attribute.String(observability.AttrNick, string(oldNick)),
		attribute.String("nick.new", string(newNick)),
	}, func(ctx context.Context, span trace.Span) error {
		if newNick == oldNick {
			return nil
		}

		if err := s.requireNickAvailable(ctx, newNick, actor); err != nil {
			return err
		}

		isUser := actor.ID() == ""

		actor.SetNick(newNick)

		if !isUser {
			// The instances table is keyed by InstanceID, so a rename is
			// an in-place update of the `nick` column.
			if err := s.store.SaveInstance(ctx, actor); err != nil {
				return fmt.Errorf("save instance: %w", err)
			}
		}

		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actor.ID())))

		now := s.now()
		actorID := actor.ID()

		change := domain.NickChange{
			OldNick:    oldNick,
			NewNick:    newNick,
			InstanceID: actorID,
			At:         now,
			Instance:   actor,
		}

		s.propagateActorEvent(ctx, actor, actorEventConfig{
			mutate: func(window *domain.ChannelWindow) {
				window.Members.RenameTo(actor, newNick)
			},
			build: func() broadcastEvent { return change },
		})

		// RFC 2812 §3.1.2: a client is always told its own NICK
		// succeeded. The broadcast above carries it back through the
		// membership filter for a client that is on a channel, but a
		// client on none has no channel to reach it through, and
		// without this its rename would land silently.
		if len(s.instanceChannelNames(actor)) == 0 {
			s.deliverToClient(ctx, actorID, change)
		}

		return nil
	})
}

// sendMessageAs records a message from the given actor and
// returns the persisted [domain.Message]. The message is emitted
// via [Session.emit]; membership fan-out suppresses the originator
// (RFC 2812 §3.3.1), and a sender holding echo-message additionally
// receives a direct echo via [Session.echoToOriginator]. A model,
// holding no echo capability, sees no copy of its own line.
func (s *Session) sendMessageAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, body string) (domain.Message, error) {
	actorNick := actor.Nick()

	var msg domain.Message

	err := s.inSpan(ctx, "session.send_message", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
	}, func(ctx context.Context, span trace.Span) error {
		instanceID := actor.ID()
		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(instanceID)))

		target, err := s.checkSendGates(ctx, actor, ch)
		if err != nil {
			return err
		}

		msg = domain.Message{
			Target:     target,
			From:       actorNick,
			InstanceID: instanceID,
			Body:       body,
			At:         s.now(),
		}

		ch = target

		s.appendEvent(ctx, ch, msg)
		s.emit(ctx, msg)

		return nil
	})

	return msg, err
}

// sendActionAs records an action message from the given actor.
// See [Session.sendMessageAs] for echo semantics.
func (s *Session) sendActionAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, body string) (domain.Message, error) {
	actorNick := actor.Nick()

	var msg domain.Message

	err := s.inSpan(ctx, "session.send_action", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
	}, func(ctx context.Context, span trace.Span) error {
		instanceID := actor.ID()
		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(instanceID)))

		target, err := s.checkSendGates(ctx, actor, ch)
		if err != nil {
			return err
		}

		msg = domain.Message{
			Target:     target,
			From:       actorNick,
			InstanceID: instanceID,
			Body:       body,
			Action:     true,
			At:         s.now(),
		}

		ch = target

		s.appendEvent(ctx, ch, msg)
		s.emit(ctx, msg)

		return nil
	})

	return msg, err
}

// setTopicAs sets the topic for a channel. A topic longer than
// [domain.TopicMaxLen] (this server's TOPICLEN) is refused with
// [domain.ErroneousTopicError] before anything else runs: the topic
// is repeated into every dispatch turn's prompt for the channel, so
// an oversized value never enters channel state; it is refused, not
// truncated.
func (s *Session) setTopicAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, topic string) error {
	if reason := domain.ValidateTopic(topic); reason != domain.TopicAccepted {
		return domain.ErroneousTopicError{Channel: ch, Reason: reason, At: s.now()}
	}

	actorNick := actor.Nick()

	return s.inSpan(ctx, "session.set_topic", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
	}, func(ctx context.Context, span trace.Span) error {
		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actor.ID())))

		if domain.InferChannelKind(ch) != domain.KindChannel {
			return observability.ErrWithKind(fmt.Errorf("cannot set topic on a direct message"), observability.ErrorKindValidation)
		}

		now := s.now()

		window, err := s.loadChannelWindow(ctx, ch)
		if err != nil {
			return fmt.Errorf("get channel: %w", err)
		}

		ch = window.Name()

		// RFC 2812 §3.2.4: setting a topic requires being on the
		// channel, whatever the channel's modes say, and a server
		// operator is no exception. The `+o` override waives channel-op
		// status, which is a privilege among the members; it does not
		// make a non-member a member, and a topic set by someone who
		// is not there has no author anyone in the channel can address.
		if !window.Members.HasInstance(actor) {
			return domain.NotOnChannelError{Channel: ch, Command: "TOPIC", At: s.now()}
		}

		// `+t` restricts TOPIC to ops (RFC 2811 §4.2.7). When the
		// channel doesn't carry `+t`, any member can change topic.
		if window.Modes.TopicLock {
			if err := s.requireChannelOp(actor, window, "TOPIC", ch); err != nil {
				return err
			}
		}

		// A TOPIC command that leaves the topic unchanged is a no-op:
		// IRC servers conventionally suppress the wire event, and
		// without this guard a chatty model can re-set the same
		// string on every turn and the channel sees a stream of
		// duplicate TopicChange events.
		if window.Topic == topic {
			return nil
		}

		window.Topic = topic
		window.TopicSetBy = actorNick
		window.TopicSetAt = now

		if err := s.persistChannelWindow(ctx, window); err != nil {
			return fmt.Errorf("save channel: %w", err)
		}

		s.persistAndEmit(ctx, ch, domain.TopicChange{
			Target:     ch,
			Topic:      topic,
			By:         actorNick,
			InstanceID: actor.ID(),
			At:         now,
			ByInstance: actor,
		})

		return nil
	})
}

// kickAs removes a target from a channel on behalf of the actor.
func (s *Session) kickAs(ctx context.Context, actor, target *domain.Instance, ch domain.ChannelName) error {
	targetNick := target.Nick()

	return s.inSpan(ctx, "session.kick", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(targetNick)),
	}, func(ctx context.Context, span trace.Span) error {
		if domain.InferChannelKind(ch) != domain.KindChannel {
			return observability.ErrWithKind(fmt.Errorf("cannot kick from a direct message"), observability.ErrorKindValidation)
		}

		window, err := s.loadChannelWindow(ctx, ch)
		if err != nil {
			return fmt.Errorf("get channel: %w", err)
		}

		ch = window.Name()

		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(target.ID())))

		if err := s.requireChannelOp(actor, window, "KICK", ch); err != nil {
			return err
		}

		if !window.Members.HasInstance(target) {
			return domain.UserNotInChannelError{Nick: targetNick, Channel: ch, Command: "KICK", At: s.now()}
		}

		// The KICK is broadcast to the channel while `target` is still
		// a member, then membership is dropped, which is the order
		// PART follows and the one RFC 2812 §3.2.8 requires: the
		// kicked client is told it was kicked, and the membership
		// filter is what carries the event to it.
		now := s.now()
		s.persistAndEmit(ctx, ch, domain.Kicked{
			Target:       ch,
			Nick:         targetNick,
			InstanceID:   target.ID(),
			By:           actor.Nick(),
			ByInstanceID: actor.ID(),
			At:           now,
			Instance:     target,
		})

		return s.removeMember(ctx, window, target)
	})
}

// inviteAs implements RFC 2812 §3.2.7's INVITE command. The invitee
// is recorded against the channel's [domain.Invitations] set so a
// follow-up JOIN can pass `+i`. Delivery is scoped to the inviter
// and invitee: the returned [domain.Invited] envelope is the
// inviter's `RPL_INVITING`-equivalent, which [Session.handleInvite]
// wraps in `Response.Events` for the synchronous client reply, and
// the same envelope is written directly to the invitee's
// subscription as their wire `INVITE` message. The channel event log
// is not touched and no broadcast happens; other channel members are
// not told.
//
// The gates run in RFC order, and nothing is recorded until every
// one of them has passed:
//
//   - the inviter must be on the channel, whatever its modes, or it
//     is [domain.NotOnChannelError] (numeric 442 ERR_NOTONCHANNEL).
//     An invitation is a member vouching for someone, so a client
//     that is not there has nothing to vouch with.
//   - on a `+i` channel the inviter must additionally hold `@`
//     (§3.2.7). On `-i` channels any member may invite.
//   - a target already on the channel is [domain.UserOnChannelError]
//     (numeric 443 ERR_USERONCHANNEL).
//
// The target is resolved, via [Session.resolveConnectedNick], before
// the invitation is written, so an unknown nick leaves the channel's
// invitation set untouched rather than holding an entry for a client
// that does not exist. Resolving against the registry of connected
// clients is what makes the user an invitable target: it holds no
// instances row, so a store lookup would answer "no such nick" for
// it. The inviter gets a [domain.SystemNotice] in place of the
// envelope, so the chat-screen surfaces the missing-nick condition.
func (s *Session) inviteAs(ctx context.Context, actor *domain.Instance, target domain.Nick, ch domain.ChannelName) (domain.ProtocolEvent, error) {
	actorNick := actor.Nick()

	var event domain.ProtocolEvent

	err := s.inSpan(ctx, "session.invite", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
		attribute.String("nick.target", string(target)),
	}, func(ctx context.Context, span trace.Span) error {
		target = domain.Nick(strings.TrimSpace(string(target)))
		if target == "" {
			return fmt.Errorf("target nick is required")
		}

		window, err := s.loadChannelWindow(ctx, ch)
		if err != nil {
			return fmt.Errorf("get channel: %w", err)
		}

		ch = window.Name()

		if !window.Members.HasInstance(actor) {
			return domain.NotOnChannelError{Channel: ch, Command: "INVITE", At: s.now()}
		}

		if window.Modes.InviteOnly {
			if err := s.requireChannelOp(actor, window, "INVITE", ch); err != nil {
				return err
			}
		}

		if _, alreadyMember := window.Members.GetByNick(target); alreadyMember {
			return domain.UserOnChannelError{Nick: target, Channel: ch, At: s.now()}
		}

		now := s.now()

		inst, err := s.resolveConnectedNick(target)
		if err != nil {
			var unknown domain.UnknownNickError
			if errors.As(err, &unknown) {
				event = domain.SystemNotice{
					Target: ch,
					Text:   fmt.Sprintf("no such nick: %s", target),
					At:     now,
				}

				return nil
			}

			return err
		}

		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(inst.ID())))

		window.Invitations.Add(inst.ID())
		if err := s.persistChannelWindow(ctx, window); err != nil {
			return fmt.Errorf("save channel: %w", err)
		}

		invited := domain.Invited{
			Target:       ch,
			Nick:         inst.Nick(),
			InstanceID:   inst.ID(),
			By:           actorNick,
			ByInstanceID: actor.ID(),
			At:           now,
			Instance:     inst,
		}

		s.deliverToClient(ctx, inst.ID(), invited)

		event = invited

		return nil
	})

	return event, err
}

// deliverToClient writes a single event directly to the
// subscription registered under `id`, bypassing
// [Session.fanOutProtocol]. Used by commands whose RFC scope
// names a specific recipient (INVITE, user-mode replies) rather
// than the channel-wide audience.
func (s *Session) deliverToClient(ctx context.Context, id domain.InstanceID, evt domain.ProtocolEvent) {
	target := s.lookupClientHandle(protocol.ClientID(id))
	if target == nil {
		return
	}

	target.enqueue(protocol.Delivery{
		Event:   evt,
		SpanCtx: trace.SpanContextFromContext(ctx),
	})
}

// setUserModeAs mutates a single user-mode flag on `target` and
// announces the change via a [domain.UserModeChange]. Delivered only
// to the affected client — RFC 2812 §3.1.5 scopes user-mode replies
// to the requester — so this bypasses [Session.fanOutProtocol] and
// writes directly to the target's events channel.
//
// Empty `by` signals a server-originated change (the canonical OPER
// MODE response shape per RFC §3.1.4). The affected client consumes
// the event to refresh its capability-gated command palette; it
// raises no scrollback line.
//
// Idempotent: a grant for an already-held mode (or a clear for an
// unheld mode) is a no-op and emits nothing.
func (s *Session) setUserModeAs(ctx context.Context, by domain.Nick, target *serverClient, mode domain.Mode, add bool) {
	if !target.setMode(mode, add) {
		return
	}

	targetInst := target.instance

	_ = s.inSpan(ctx, "session.set_user_mode", []attribute.KeyValue{
		attribute.String(observability.AttrNick, string(targetInst.Nick())),
		attribute.String(observability.AttrInstanceID, string(targetInst.ID())),
		attribute.String("mode.flag", string(mode)),
		attribute.Bool("mode.add", add),
	}, func(ctx context.Context, _ trace.Span) error {
		target.enqueue(protocol.Delivery{
			Event: domain.UserModeChange{
				Nick:       targetInst.Nick(),
				InstanceID: targetInst.ID(),
				Flag:       mode,
				Add:        add,
				By:         by,
				At:         s.now(),
				Instance:   targetInst,
			},
			SpanCtx: trace.SpanContextFromContext(ctx),
		})

		return nil
	})
}

// registerModelAs claims `nick` for a fresh model instance and
// records it. This is the registration half of `ADDMODEL`: the
// instance exists and holds its nick, but has no subscription and is
// in no channel yet. The dispatcher's [protocol.AddModel] handler is
// the only caller and has already run the operator gate.
//
// Runs on the session's command loop. `nick` was chosen against a
// snapshot of the nick space taken before the loop was reached, so
// the claim is checked here: this is the point at which the nick is
// actually taken, and the only point where the check cannot be
// overtaken by a concurrent rename.
func (s *Session) registerModelAs(
	ctx context.Context,
	ch domain.ChannelName,
	modelID domain.ModelID,
	nick domain.Nick,
	persona string,
) (*domain.Instance, error) {
	var inst *domain.Instance

	err := s.inSpan(ctx, "session.register_model", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(nick)),
		attribute.String(observability.AttrModelID, string(modelID)),
	}, func(ctx context.Context, _ trace.Span) error {
		if err := s.requireNickAvailable(ctx, nick, nil); err != nil {
			return err
		}

		if _, err := s.loadChannelWindow(ctx, ch); err != nil {
			return fmt.Errorf("get channel: %w", err)
		}

		channels := orderedmap.New[domain.ChannelName, time.Time]()
		channels.Set(ch, s.now())

		inst = domain.NewModelInstance(
			domain.GenerateInstanceID(),
			nick,
			modelID,
			persona,
			channels,
		)

		if err := s.store.SaveInstance(ctx, inst); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return inst, nil
}

// requireNickAvailable is the one place a nick is checked before it
// is claimed, whether the claim comes from `NICK` or from the
// registration half of `ADDMODEL`. It answers the two refusals IRC
// distinguishes, in the order a server does: a nick outside the RFC
// 2812 §2.3.1 grammar is [domain.ErroneousNicknameError] (numeric
// 432) and no client will ever hold it; a well-formed nick another
// client holds is [domain.NickInUseError] (numeric 433) and the
// answer changes when that client renames or quits.
//
// `holder` is the instance allowed to already hold the nick, which
// is the renaming client under `NICK`, so re-taking one's own nick
// in a different case is not a collision. Pass nil when no client
// may hold it.
//
// Only a clean "no such nick" from the store counts as free: any
// other resolve failure is returned, so a store that is briefly
// unavailable refuses the claim and no duplicate gets through.
func (s *Session) requireNickAvailable(ctx context.Context, nick domain.Nick, holder *domain.Instance) error {
	if reason := domain.ValidateNick(nick); reason != domain.NickAccepted {
		return observability.ErrWithKind(
			domain.ErroneousNicknameError{Nick: nick, Reason: reason, At: s.now()},
			observability.ErrorKindValidation,
		)
	}

	existing, err := s.ResolveNick(ctx, nick)

	switch {
	case err == nil:
		if existing == holder {
			return nil
		}

		return observability.ErrWithKind(domain.NickInUseError{Nick: nick, At: s.now()}, observability.ErrorKindValidation)
	case errors.Is(err, store.ErrNoSuchNick):
		return nil
	default:
		return fmt.Errorf("resolve nick: %w", err)
	}
}

// admitModelAs joins a registered model instance to `ch`, the
// closing half of `ADDMODEL`. The bus carries a `Join` event with
// the same wire shape any `/join` would produce. `actor` is the
// operator who issued the command; it is the join's authority, so a
// `+i` channel admits the new instance on the operator's privileges
// (see [Session.checkJoinGates]).
//
// Runs on the session's command loop, after the instance's
// model-client has attached, so the JOIN reaches its subscription.
func (s *Session) admitModelAs(ctx context.Context, actor, inst *domain.Instance, ch domain.ChannelName) error {
	return s.inSpan(ctx, "session.add_model", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actor.Nick())),
		attribute.String(observability.AttrInstanceID, string(inst.ID())),
	}, func(ctx context.Context, _ trace.Span) error {
		_, err := s.joinAs(ctx, inst, operatorJoin, ch, "")

		return err
	})
}

// killAs is the operator-issued forced disconnect of `target` per
// RFC 2812 §3.7.1. The kill is announced exactly as IRC frames it: a
// killed client is seen to QUIT, so `quitAs`'s model-actor branch
// broadcasts a wire `QUIT` to peers in shared channels with the
// conventional `"Killed by <oper> (<reason>)"` body.
//
// The dispatcher's `handleKill` is the only caller and runs the
// operator gate, so this method assumes `oper` has the
// authority. The reap of the target's subscription happens in
// the dispatcher too, after this returns.
func (s *Session) killAs(ctx context.Context, oper, target *domain.Instance, reason string) error {
	if target.ID() == "" {
		return fmt.Errorf("KILL cannot target the user-client")
	}

	body := fmt.Sprintf("Killed by %s (%s)", oper.Nick(), reason)

	return s.quitAs(ctx, target, body)
}
