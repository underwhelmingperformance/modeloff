package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	orderedmap "github.com/wk8/go-ordered-map/v2"
	"go.opentelemetry.io/otel/attribute"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/store"
)

// JoinAs joins the given actor to a channel.
func (s *Session) JoinAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName) (retErr error) {
	ch = domain.NormaliseChannelName(ch)

	actorNick := actor.Nick()

	ctx, span := s.startSpan(
		ctx,
		"session.join",
		attribute.String(observability.AttrOperation, "session.join"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actor.ID())))

	now := s.now()
	isUser := actor == s.user

	window, created, err := s.ensureChannelWindowWithActor(ctx, ch, actor, now)
	if err != nil {
		return err
	}

	alreadyMember := !created && window.Members.HasInstance(actor)

	if !created && !alreadyMember {
		window.Members.Add(actor)

		if err := s.persistChannelWindow(ctx, window); err != nil {
			return fmt.Errorf("save channel: %w", err)
		}
	}

	if isUser {
		// Stamp the user's mark-as-read cursor at the current head so
		// the join itself does not leave the channel showing as unread.
		// `last_channel` persistence is the UI's concern and lands when
		// the chat screen receives a `ChannelActiveMsg`.
		if err := s.MarkRead(ctx, ch); err != nil {
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

	if isUser {
		// Send the joiner the channel's current member list (IRC's
		// RPL_NAMREPLY). The chat-screen handler uses this to
		// populate its local member-list cache with pre-existing
		// members; without it, the cache would see only the joiner.
		s.emitUIOnly(domain.NamesReplyEvent{
			Channel: ch,
			Members: window.Members,
			At:      now,
		})

		return s.emitJoinProtocol(ctx, ch, window, now)
	}

	return s.grantVoice(ctx, ch, window, actor, actorNick, now)
}

// ensureChannelWindowWithActor loads the channel-window or creates
// a fresh one that already contains the actor. Returns the
// (possibly freshly-saved) `*ChannelWindow`, whether it was newly
// created, and any persistence error encountered along the way.
// JoinAs is the only caller and is gated on `#`-prefixed names by
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

// emitJoinProtocol sets the user's mode to +o, emits topic info if
// the channel has a topic, and saves the autojoin list.
func (s *Session) emitJoinProtocol(ctx context.Context, ch domain.ChannelName, window *domain.ChannelWindow, now time.Time) error {
	s.setUserMode(ctx, ch, domain.ModeOp)
	window.Members.SetMode(s.user, domain.ModeOp)

	if err := s.persistChannelWindow(ctx, window); err != nil {
		return fmt.Errorf("save channel after mode: %w", err)
	}

	s.persistAndEmitUIOnly(ctx, ch, domain.ModeChange{
		Target:     ch,
		Nick:       s.user.Nick(),
		InstanceID: s.user.ID(),
		Mode:       domain.ModeOp,
		By:         "ChanServ",
		At:         now,
		Instance:   s.user,
		Actor:      "ChanServ",
	})

	if window.Topic != "" {
		s.emitUIOnly(domain.TopicInfo{
			Target:     ch,
			Topic:      window.Topic,
			TopicSetBy: window.TopicSetBy,
			TopicSetAt: window.TopicSetAt,
			At:         now,
		})
	}

	return s.saveAutojoinList(ctx)
}

// grantVoice gives a non-user joiner (a model instance) +v via
// ChanServ. This mirrors the +o granted to the user by
// emitJoinProtocol so the nick list distinguishes models (+nick) from
// the user (@nick) on every join, not only when the model was added
// through the invite path.
//
// The caller passes its own nick snapshot so the `Join` and
// `ModeChange` events emitted back-to-back for the same join
// record the same nick, even if the actor renames between them.
func (s *Session) grantVoice(ctx context.Context, ch domain.ChannelName, window *domain.ChannelWindow, inst *domain.Instance, nick domain.Nick, now time.Time) error {
	window.Members.SetMode(inst, domain.ModeVoice)

	if err := s.persistChannelWindow(ctx, window); err != nil {
		return fmt.Errorf("save channel after voice: %w", err)
	}

	s.persistAndEmitUIOnly(ctx, ch, domain.ModeChange{
		Target:     ch,
		Nick:       nick,
		InstanceID: inst.ID(),
		Mode:       domain.ModeVoice,
		By:         "ChanServ",
		At:         now,
		Instance:   inst,
		Actor:      "ChanServ",
	})

	return nil
}

// PartAs parts the given actor from a channel.
func (s *Session) PartAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, message string) (retErr error) {
	actorNick := actor.Nick()

	ctx, span := s.startSpan(
		ctx,
		"session.part",
		attribute.String(observability.AttrOperation, "session.part"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	if domain.InferChannelKind(ch) != domain.KindChannel {
		return errWithKind(fmt.Errorf("cannot part %s", ch), observability.ErrorKindValidation)
	}

	window, err := s.loadChannelWindow(ctx, ch)
	if err != nil {
		return fmt.Errorf("channel not found: %w", err)
	}

	isUser := actor == s.user

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actor.ID())))

	m, ok := window.Members.GetByInstance(actor)
	if !ok {
		return nil
	}

	window.Members.Remove(m)

	if err := s.persistChannelWindow(ctx, window); err != nil {
		return fmt.Errorf("save channel: %w", err)
	}

	actor.MutateChannels(func(m *orderedmap.OrderedMap[domain.ChannelName, time.Time]) {
		m.Delete(ch)
	})

	if isUser {
		s.forgetUserMode(ctx, ch)
		if err := s.saveAutojoinList(ctx); err != nil {
			return fmt.Errorf("save autojoin: %w", err)
		}
	} else if err := s.store.SaveInstance(ctx, actor); err != nil {
		return fmt.Errorf("save instance: %w", err)
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
}

// QuitAs quits the given actor from every joined channel.
func (s *Session) QuitAs(ctx context.Context, actor *domain.Instance, message string) (retErr error) {
	if actor == s.user {
		return s.Quit(ctx, message)
	}

	actorID := actor.ID()
	actorNick := actor.Nick()

	ctx, span := s.startSpan(
		ctx,
		"session.quit",
		attribute.String(observability.AttrOperation, "session.quit"),
		attribute.String(observability.AttrNick, string(actorNick)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actorID)))

	now := s.now()
	channels := s.instanceChannelNames(actor)

	s.propagateActorEvent(ctx, actor, actorEventConfig{
		mutate: func(window *domain.ChannelWindow) {
			if m, ok := window.Members.GetByInstance(actor); ok {
				window.Members.Remove(m)
			}
		},
		build: func(ch domain.ChannelName) domain.PersistableEvent {
			return domain.Quit{
				Target:     ch,
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
}

// ChangeNickAs changes the given actor's nickname.
func (s *Session) ChangeNickAs(ctx context.Context, actor *domain.Instance, newNick domain.Nick) (retErr error) {
	oldNick := actor.Nick()

	ctx, span := s.startSpan(
		ctx,
		"session.change_nick",
		attribute.String(observability.AttrOperation, "session.change_nick"),
		attribute.String(observability.AttrNick, string(oldNick)),
		attribute.String("nick.new", string(newNick)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	if newNick == oldNick {
		return nil
	}

	if existing, err := s.ResolveNick(ctx, newNick); err == nil && existing != actor {
		return errWithKind(domain.NickInUseError{Nick: newNick}, observability.ErrorKindValidation)
	}

	isUser := actor == s.user

	actor.SetNick(newNick)

	if !isUser {
		// The instances table is keyed by InstanceID, so a rename is
		// an in-place update of the `nick` column — no delete-then-
		// reinsert needed as it was under the old nick-keyed schema.
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
		build: func(ch domain.ChannelName) domain.PersistableEvent {
			return domain.NickChange{
				Target:     ch,
				OldNick:    oldNick,
				NewNick:    newNick,
				InstanceID: actorID,
				At:         now,
				Instance:   actor,
			}
		},
	})

	return nil
}

// SendMessageAs records a message from the given actor and
// returns the persisted [domain.Message]. The session does not
// echo the user's own outgoing messages on its events channel —
// per RFC 2812 §3.3.1 a server forwards PRIVMSG to other
// clients on the channel and to the addressed nick, not back
// to the sender. Standard IRC clients render their own
// outgoing line locally; the chat screen does the same by
// consuming the returned [domain.Message] from the command-
// result tea.Msg path. Messages from model actors continue to
// emit so the user's chat screen can render replies.
func (s *Session) SendMessageAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, body string) (msg domain.Message, retErr error) {
	actorNick := actor.Nick()

	ctx, span := s.startSpan(
		ctx,
		"session.send_message",
		attribute.String(observability.AttrOperation, "session.send_message"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	if ch == domain.StatusChannelName {
		return domain.Message{}, errWithKind(domain.StatusChannelGuardError{
			Command: "send",
			Hint:    "the status channel doesn't take messages — try /msg <nick-or-#channel> instead",
		}, observability.ErrorKindValidation)
	}

	instanceID := actor.ID()
	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(instanceID)))

	cm := domain.Message{
		Target:     ch,
		From:       actorNick,
		InstanceID: instanceID,
		Body:       body,
		At:         s.now(),
	}

	s.appendEvent(ctx, ch, cm)

	if actor == s.user {
		// Don't echo on the events channel: the chat screen
		// renders its own outgoing line locally from the
		// command-result path. Still trigger model dispatch so
		// channel members react to the user's message.
		s.maybeDispatch(ctx, cm)
	} else {
		s.emit(ctx, cm)
	}

	return cm, nil
}

// SendActionAs records an action message from the given actor.
// See [Session.SendMessageAs] for echo semantics.
func (s *Session) SendActionAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, body string) (msg domain.Message, retErr error) {
	actorNick := actor.Nick()

	ctx, span := s.startSpan(
		ctx,
		"session.send_action",
		attribute.String(observability.AttrOperation, "session.send_action"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	if ch == domain.StatusChannelName {
		return domain.Message{}, errWithKind(domain.StatusChannelGuardError{
			Command: "me",
			Hint:    "the status channel doesn't take messages — try /msg <nick-or-#channel> instead",
		}, observability.ErrorKindValidation)
	}

	instanceID := actor.ID()
	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(instanceID)))

	cm := domain.Message{
		Target:     ch,
		From:       actorNick,
		InstanceID: instanceID,
		Body:       body,
		Action:     true,
		At:         s.now(),
	}

	s.appendEvent(ctx, ch, cm)

	if actor == s.user {
		s.maybeDispatch(ctx, cm)
	} else {
		s.emit(ctx, cm)
	}

	return cm, nil
}

// SetTopicAs sets the topic for a channel.
func (s *Session) SetTopicAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, topic string) (retErr error) {
	actorNick := actor.Nick()

	ctx, span := s.startSpan(
		ctx,
		"session.set_topic",
		attribute.String(observability.AttrOperation, "session.set_topic"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actor.ID())))

	if domain.InferChannelKind(ch) != domain.KindChannel {
		return errWithKind(fmt.Errorf("cannot set topic on a direct message"), observability.ErrorKindValidation)
	}

	now := s.now()

	window, err := s.loadChannelWindow(ctx, ch)
	if err != nil {
		return fmt.Errorf("get channel: %w", err)
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
		At:         now,
		ByInstance: actor,
	})

	return nil
}

// KickAs removes a target from a channel on behalf of the actor.
func (s *Session) KickAs(ctx context.Context, actor, target *domain.Instance, ch domain.ChannelName) (retErr error) {
	targetNick := target.Nick()

	ctx, span := s.startSpan(
		ctx,
		"session.kick",
		attribute.String(observability.AttrOperation, "session.kick"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(targetNick)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	if domain.InferChannelKind(ch) != domain.KindChannel {
		return errWithKind(fmt.Errorf("cannot kick from a direct message"), observability.ErrorKindValidation)
	}

	window, err := s.loadChannelWindow(ctx, ch)
	if err != nil {
		return fmt.Errorf("get channel: %w", err)
	}

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(target.ID())))

	if !window.Members.HasInstance(target) {
		return nil
	}

	if m, ok := window.Members.GetByInstance(target); ok {
		window.Members.Remove(m)
	}

	if err := s.persistChannelWindow(ctx, window); err != nil {
		return fmt.Errorf("save channel: %w", err)
	}

	target.MutateChannels(func(m *orderedmap.OrderedMap[domain.ChannelName, time.Time]) {
		m.Delete(ch)
	})

	if target != s.user {
		if err := s.store.SaveInstance(ctx, target); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}
	} else {
		s.forgetUserMode(ctx, ch)
	}

	actorNick := actor.Nick()

	now := s.now()
	s.persistAndEmit(ctx, ch, domain.ModelKicked{
		Target:     ch,
		Nick:       targetNick,
		InstanceID: target.ID(),
		By:         actorNick,
		At:         now,
		Instance:   target,
	})

	return nil
}

// InviteAs sends a real IRC-style invite.
func (s *Session) InviteAs(ctx context.Context, actor *domain.Instance, target domain.Nick, ch domain.ChannelName) (retErr error) {
	actorNick := actor.Nick()

	ctx, span := s.startSpan(
		ctx,
		"session.invite",
		attribute.String(observability.AttrOperation, "session.invite"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
		attribute.String("nick.target", string(target)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	target = domain.Nick(strings.TrimSpace(string(target)))
	if target == "" {
		return fmt.Errorf("target nick is required")
	}

	if actor == s.user {
		inst, err := s.store.ResolveNick(ctx, target)
		if err == nil {
			span.SetAttributes(attribute.String(observability.AttrInstanceID, string(inst.ID())))
			return s.attachInstanceToChannel(ctx, ch, inst, actor)
		}

		if !errors.Is(err, store.ErrNoSuchNick) {
			return fmt.Errorf("resolve nick: %w", err)
		}
	}

	now := s.now()
	notice := fmt.Sprintf("%s invited %s to %s", actorNick, target, ch)
	s.appendEvent(ctx, ch, domain.SystemNotice{Target: ch, Text: notice, At: now})

	return nil
}
