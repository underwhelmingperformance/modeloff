package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
//nolint:gocognit // sequenced join steps (create-or-load, gate, op-grant, mark-read, persist, broadcast, replies) read clearer inline than as further-extracted helpers.
func (s *Session) joinAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, key string) error {
	ch = domain.NormaliseChannelName(ch)

	actorNick := actor.Nick()

	return s.inSpan(ctx, "session.join", []attribute.KeyValue{
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

		alreadyMember := !created && window.Members.HasInstance(actor)

		if !created && !alreadyMember {
			if err := s.checkJoinGates(window, actorNick, key); err != nil {
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
			window.Members.SetMode(actor, domain.ModeOp)
			if isUser {
				s.setUserMode(ctx, ch, domain.ModeOp)
			}

			if err := s.persistChannelWindow(ctx, window); err != nil {
				return fmt.Errorf("save channel after mode: %w", err)
			}
		}

		if isUser {
			// Stamp the user's mark-as-read cursor at the current head so
			// the join itself does not leave the channel showing as unread.
			// `last_channel` persistence is the UI's concern and lands when
			// the chat screen receives a `ChannelActiveMsg`.
			if err := s.markRead(ctx, ch); err != nil {
				return fmt.Errorf("mark read: %w", err)
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
		// sent only to the joiner — they are server-to-client
		// responses, not channel broadcasts. Deliver directly to the
		// joiner's subscription via [Session.deliverToClient]. The
		// chat-screen consumes NamesReplyEvent to populate its
		// member-list cache when the user joins; the model-client's
		// dispatch loop files TopicInfo into history when a model
		// joins so the prompt knows who set the topic and when.
		s.deliverToClient(ctx, actor.ID(), domain.NamesReplyEvent{
			Channel: ch,
			Members: window.Members,
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

		if isUser {
			return s.saveAutojoinList(ctx)
		}

		return nil
	})
}

// ensureChannelWindowWithActor loads the channel-window or creates
// a fresh one that already contains the actor. Returns the
// (possibly freshly-saved) `*ChannelWindow`, whether it was newly
// created, and any persistence error encountered along the way.
// joinAs is the only caller and is gated on `#`-prefixed names by
// `NormaliseChannelName`, so a load that returns a non-channel
// row indicates a programming error in the upstream guard rather
// than a user-reachable state.
func (s *Session) ensureChannelWindowWithActor(ctx context.Context, ch domain.ChannelName, actor *domain.Instance, now time.Time) (*domain.ChannelWindow, bool, error) {
	window, err := s.loadChannelWindow(ctx, ch)
	if err == nil {
		return window, false, nil
	}

	window = domain.NewChannelWindow(ch, now)
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
	actor.MutateChannels(func(m *orderedmap.OrderedMap[domain.ChannelName, time.Time]) {
		if _, ok := m.Get(ch); !ok {
			m.Set(ch, now)
		}
	})

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
			return errWithKind(fmt.Errorf("cannot part %s", ch), observability.ErrorKindValidation)
		}

		window, err := s.loadChannelWindow(ctx, ch)
		if err != nil {
			return fmt.Errorf("channel not found: %w", err)
		}

		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actor.ID())))

		if !window.Members.HasInstance(actor) {
			return domain.NotOnChannelError{Channel: ch, Command: "PART", At: s.now()}
		}

		if err := s.removeMember(ctx, window, actor); err != nil {
			return err
		}

		if actor.ID() == "" {
			if err := s.saveAutojoinList(ctx); err != nil {
				return fmt.Errorf("save autojoin: %w", err)
			}
		}

		now := s.now()
		s.persistAndEmit(ctx, ch, domain.Part{
			Target:     ch,
			Nick:       actorNick,
			InstanceID: actor.ID(),
			Message:    message,
			At:         now,
			Instance:   actor,
		})

		return nil
	})
}

// quitAs disconnects the given actor from every joined channel.
// For the user-client the call saves the autojoin list and clears
// the session-active marker so the next startup is classified as
// clean; the QUIT lines are persisted but not broadcast, because
// the only consumer of broadcast events (the chat-screen) is
// about to tear down. For a model-client the call broadcasts the
// `domain.Quit` event to common-channel peers and deletes the
// instance row — the dispatcher reaps the subscription separately
// via [Session.reapClient].
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

		actor.MutateChannels(func(m *orderedmap.OrderedMap[domain.ChannelName, time.Time]) {
			for _, ch := range channels {
				m.Delete(ch)
			}
		})

		if err := s.store.SaveInstance(ctx, actor); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}

		if err := s.store.DeleteInstanceByID(ctx, actorID); err != nil {
			return fmt.Errorf("delete instance: %w", err)
		}

		return nil
	})
}

// changeNickAs changes the given actor's nickname.
func (s *Session) changeNickAs(ctx context.Context, actor *domain.Instance, newNick domain.Nick) error {
	oldNick := actor.Nick()

	return s.inSpan(ctx, "session.change_nick", []attribute.KeyValue{
		attribute.String(observability.AttrNick, string(oldNick)),
		attribute.String("nick.new", string(newNick)),
	}, func(ctx context.Context, span trace.Span) error {
		if newNick == oldNick {
			return nil
		}

		if existing, err := s.ResolveNick(ctx, newNick); err == nil && existing != actor {
			return errWithKind(domain.NickInUseError{Nick: newNick, At: s.now()}, observability.ErrorKindValidation)
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

		s.propagateActorEvent(ctx, actor, actorEventConfig{
			mutate: func(window *domain.ChannelWindow) {
				window.Members.RenameTo(actor, newNick)
			},
			build: func() broadcastEvent {
				return domain.NickChange{
					OldNick:    oldNick,
					NewNick:    newNick,
					InstanceID: actorID,
					At:         now,
					Instance:   actor,
				}
			},
		})

		return nil
	})
}

// sendMessageAs records a message from the given actor and
// returns the persisted [domain.Message]. The message is emitted
// via [Session.emit]; the broadcast helper applies the
// originator-suppression rule (RFC 2812 §3.3.1) so the sender
// does not see their own line on its [protocol.Client.Events]
// channel.
func (s *Session) sendMessageAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, body string) (domain.Message, error) {
	actorNick := actor.Nick()

	var msg domain.Message

	err := s.inSpan(ctx, "session.send_message", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
	}, func(ctx context.Context, span trace.Span) error {
		instanceID := actor.ID()
		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(instanceID)))

		if err := s.checkSendGates(ctx, actor, ch); err != nil {
			return err
		}

		msg = domain.Message{
			Target:     ch,
			From:       actorNick,
			InstanceID: instanceID,
			Body:       body,
			At:         s.now(),
		}

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

		if err := s.checkSendGates(ctx, actor, ch); err != nil {
			return err
		}

		msg = domain.Message{
			Target:     ch,
			From:       actorNick,
			InstanceID: instanceID,
			Body:       body,
			Action:     true,
			At:         s.now(),
		}

		s.appendEvent(ctx, ch, msg)
		s.emit(ctx, msg)

		return nil
	})

	return msg, err
}

// setTopicAs sets the topic for a channel.
func (s *Session) setTopicAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, topic string) error {
	actorNick := actor.Nick()

	return s.inSpan(ctx, "session.set_topic", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
	}, func(ctx context.Context, span trace.Span) error {
		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actor.ID())))

		if domain.InferChannelKind(ch) != domain.KindChannel {
			return errWithKind(fmt.Errorf("cannot set topic on a direct message"), observability.ErrorKindValidation)
		}

		now := s.now()

		window, err := s.loadChannelWindow(ctx, ch)
		if err != nil {
			return fmt.Errorf("get channel: %w", err)
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
			return errWithKind(fmt.Errorf("cannot kick from a direct message"), observability.ErrorKindValidation)
		}

		window, err := s.loadChannelWindow(ctx, ch)
		if err != nil {
			return fmt.Errorf("get channel: %w", err)
		}

		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(target.ID())))

		if err := s.requireChannelOp(actor, window, "KICK", ch); err != nil {
			return err
		}

		if !window.Members.HasInstance(target) {
			return domain.UserNotInChannelError{Nick: targetNick, Channel: ch, Command: "KICK", At: s.now()}
		}

		if err := s.removeMember(ctx, window, target); err != nil {
			return err
		}

		actorNick := actor.Nick()

		now := s.now()
		s.persistAndEmit(ctx, ch, domain.ModelKicked{
			Target:       ch,
			Nick:         targetNick,
			InstanceID:   target.ID(),
			By:           actorNick,
			ByInstanceID: actor.ID(),
			At:           now,
			Instance:     target,
		})

		return nil
	})
}

// inviteAs implements RFC 2812 §3.2.7's INVITE command. The
// invited nick is recorded against the channel's `InvitedNicks`
// set so a follow-up JOIN can clear `+i`. Delivery is scoped to
// the inviter and invitee: the returned [domain.ModelInvited]
// envelope is the inviter's `RPL_INVITING`-equivalent (the
// caller — [Session.handleInvite] — wraps it in `Response.Events`
// for the synchronous client reply), and the same envelope is
// written directly to the invitee's subscription as their wire
// `INVITE` message. The channel event log is not touched and no
// broadcast happens; other channel members are not told.
//
// A target nick already on the channel is refused with
// [domain.UserOnChannelError] (RFC 2812 numeric 443
// ERR_USERONCHANNEL) and nothing is recorded.
//
// An unknown target nick has no subscription to receive the
// invite. The inviter gets a [domain.SystemNotice] in its place
// so the chat-screen surfaces the missing-nick condition; the
// channel still records nothing.
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

		// INVITE is op-gated only when the target channel carries
		// `+i` (RFC 2812 §3.2.7). On `-i` channels any member can
		// invite.
		if window.Modes.InviteOnly {
			if err := s.requireChannelOp(actor, window, "INVITE", ch); err != nil {
				return err
			}
		}

		if _, alreadyMember := window.Members.GetByNick(target); alreadyMember {
			return domain.UserOnChannelError{Nick: target, Channel: ch, At: s.now()}
		}

		window.InvitedNicks.Add(target)
		if err := s.persistChannelWindow(ctx, window); err != nil {
			return fmt.Errorf("save channel: %w", err)
		}

		now := s.now()

		inst, err := s.store.ResolveNick(ctx, target)
		switch {
		case err == nil:
			span.SetAttributes(attribute.String(observability.AttrInstanceID, string(inst.ID())))

			invited := domain.ModelInvited{
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

		case errors.Is(err, store.ErrNoSuchNick):
			event = domain.SystemNotice{
				Target: ch,
				Text:   fmt.Sprintf("no such nick: %s", target),
				At:     now,
			}

			return nil

		default:
			return fmt.Errorf("resolve nick: %w", err)
		}
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

	select {
	case target.events <- protocol.Delivery{
		Event:   evt,
		SpanCtx: trace.SpanContextFromContext(ctx),
	}:
	case <-target.done:
	case <-ctx.Done():
	}
}

// setUserModeAs mutates a single user-mode flag on `target` and
// announces the change via a [domain.ModeChange] with empty
// `Target` (the user-mode form). Delivered only to the affected
// client — RFC 2812 §3.1.5 scopes user-mode replies to the
// requester — so this bypasses [Session.fanOutProtocol] and
// writes directly to the target's events channel.
//
// Empty `by` signals server-originated (the canonical OPER MODE
// response shape per RFC §3.1.4): the chat-screen renderer prints
// `*** server sets mode +o nick` rather than attributing to a
// peer nick.
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
		evt := domain.ModeChange{
			Nick:       targetInst.Nick(),
			InstanceID: targetInst.ID(),
			Flag:       mode,
			Add:        add,
			By:         by,
			At:         s.now(),
			Instance:   targetInst,
		}

		select {
		case target.events <- protocol.Delivery{
			Event:   evt,
			SpanCtx: trace.SpanContextFromContext(ctx),
		}:
		case <-target.done:
			// Target was reaped between resolution and delivery; drop.
		case <-ctx.Done():
		}

		return nil
	})
}

// addModelAs creates a fresh model instance, generates a unique
// nick for it via the small-model API, attaches the model-client,
// and joins it to the named channel via `joinAs`. The bus carries
// a `Join` event with the same wire shape any `/join` would
// produce. The dispatcher's [protocol.AddModel] handler is the
// only caller — the operator gate lives there, so this method
// assumes the caller has already verified `actor`'s authority.
//
// Persona resolution: a non-empty `persona` is used verbatim; an
// empty value triggers a draw from the personas pool (lazily
// generated if missing). API failures during persona generation
// are logged but never block the add; the instance gets an empty
// persona instead.
func (s *Session) addModelAs(
	ctx context.Context,
	actor *domain.Instance,
	ch domain.ChannelName,
	modelID domain.ModelID,
	persona string,
) (*domain.Instance, error) {
	var inst *domain.Instance

	err := s.inSpan(ctx, "session.add_model", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actor.Nick())),
	}, func(ctx context.Context, _ trace.Span) error {
		nick, assignedPersona, err := s.modelClientFactory.PrepareInstance(ctx, s, modelID, persona)
		if err != nil {
			return errWithKind(err, observability.ErrorKindDispatch)
		}

		channels := orderedmap.New[domain.ChannelName, time.Time]()
		channels.Set(ch, s.now())

		inst = domain.NewModelInstance(
			domain.GenerateInstanceID(),
			nick,
			modelID,
			assignedPersona,
			channels,
		)

		if _, err := s.loadChannelWindow(ctx, ch); err != nil {
			return fmt.Errorf("get channel: %w", err)
		}

		if err := s.store.SaveInstance(ctx, inst); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}

		if _, err := s.modelClientFactory.Attach(ctx, s, inst); err != nil {
			slog.Default().WarnContext(ctx, "attach model client",
				"component", "session",
				"instance_id", inst.ID(),
				"channel", ch,
				"error", err,
			)
		}

		// Pre-seed `InvitedNicks` so `joinAs` clears `+i`.
		if window, err := s.loadChannelWindow(ctx, ch); err == nil {
			window.InvitedNicks.Add(nick)
			if err := s.persistChannelWindow(ctx, window); err != nil {
				return fmt.Errorf("save channel: %w", err)
			}
		}

		if err := s.joinAs(ctx, inst, ch, ""); err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return inst, nil
}

// killAs is the operator-issued forced disconnect of `target` per
// RFC 2812 §3.7.1. The target receives a single [domain.Killed]
// event on its own Events channel before its subscription is
// reaped — that line is the renderer's last word, the
// notification "you were KILLed by X". Peers in shared channels
// separately receive a wire `QUIT` with the conventional
// `"Killed by <oper> (<reason>)"` body, emitted by `quitAs`'s
// model-actor branch.
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

	sc := s.lookupClientHandle(protocol.ClientID(target.ID()))
	if sc != nil {
		select {
		case sc.events <- protocol.Delivery{
			Event: domain.Killed{
				By:     oper.Nick(),
				Reason: reason,
				At:     s.now(),
			},
			SpanCtx: trace.SpanContextFromContext(ctx),
		}:
		case <-sc.done:
			// Target was concurrently reaped; the Killed line
			// belongs to a subscription that no longer exists.
		case <-ctx.Done():
		}
	}

	return s.quitAs(ctx, target, body)
}
