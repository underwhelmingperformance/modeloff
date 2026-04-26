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

	channel, created, err := s.ensureChannelWithActor(ctx, ch, actor, now)
	if err != nil {
		return err
	}

	alreadyMember := !created && channel.Members.HasInstance(actor)

	if !created && !alreadyMember {
		channel.Members.Add(actor)

		if err := s.persistChannel(ctx, channel); err != nil {
			return fmt.Errorf("save channel: %w", err)
		}
	}

	if isUser {
		if err := s.rememberActiveChannel(ctx, ch); err != nil {
			return err
		}
	}

	if !alreadyMember {
		if err := s.recordActorMembership(ctx, actor, ch, now, isUser); err != nil {
			return err
		}
	}

	if alreadyMember || channel.Kind == domain.KindDM {
		return nil
	}

	s.persistAndEmit(ctx, ch, domain.ChannelJoin{
		Channel:    ch,
		Nick:       actorNick,
		InstanceID: actor.ID(),
		Created:    created,
		At:         now,
		Instance:   actor,
	})

	channel, _ = s.loadChannel(ctx, ch)

	if isUser {
		// Send the joiner the channel's current member list (IRC's
		// RPL_NAMREPLY). The chat-screen handler uses this to
		// populate its local member-list cache with pre-existing
		// members; without it, the cache would see only the joiner.
		s.emitUIOnly(domain.NamesReplyEvent{
			Channel: ch,
			Members: channel.Members,
			At:      now,
		})

		return s.emitJoinProtocol(ctx, ch, channel, now)
	}

	return s.grantVoice(ctx, ch, channel, actor, actorNick, now)
}

// ensureChannelWithActor loads the channel or creates a fresh one
// that already contains the actor. Returns the (possibly freshly-
// saved) channel, whether it was newly created, and any persistence
// error encountered along the way.
func (s *Session) ensureChannelWithActor(ctx context.Context, ch domain.ChannelName, actor *domain.Instance, now time.Time) (domain.Channel, bool, error) {
	channel, err := s.loadChannel(ctx, ch)
	if err == nil {
		if channel.Members.Len() == 0 {
			channel.Members = domain.NewMemberList()
		}

		return channel, false, nil
	}

	members := domain.NewMemberList()
	members.Add(actor)

	channel = domain.Channel{
		Name:    ch,
		Kind:    domain.KindChannel,
		Members: members,
		Created: now,
	}

	if saveErr := s.persistChannel(ctx, channel); saveErr != nil {
		return domain.Channel{}, false, fmt.Errorf("save channel: %w", saveErr)
	}

	return channel, true, nil
}

// rememberActiveChannel records that the user just joined `ch`:
// their last-focused channel in the store (so a crash restores
// them to the same spot) and the mark-as-read cursor.
func (s *Session) rememberActiveChannel(ctx context.Context, ch domain.ChannelName) error {
	if err := s.store.SetLastChannel(ctx, ch); err != nil {
		return fmt.Errorf("set last channel: %w", err)
	}

	if err := s.MarkRead(ctx, ch); err != nil {
		return fmt.Errorf("mark read: %w", err)
	}

	return nil
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
func (s *Session) emitJoinProtocol(ctx context.Context, ch domain.ChannelName, channel domain.Channel, now time.Time) error {
	s.setUserMode(ctx, ch, domain.ModeOp)
	channel.Members.SetMode(s.user, domain.ModeOp)

	if err := s.persistChannel(ctx, channel); err != nil {
		return fmt.Errorf("save channel after mode: %w", err)
	}

	s.persistAndEmitUIOnly(ctx, ch, domain.ChannelModeChange{
		Channel:    ch,
		Nick:       s.user.Nick(),
		InstanceID: s.user.ID(),
		Mode:       domain.ModeOp,
		By:         "ChanServ",
		At:         now,
		Instance:   s.user,
		Actor:      "ChanServ",
	})

	if channel.Topic != "" {
		s.emitUIOnly(domain.ChannelTopicInfo{
			Channel:    ch,
			Topic:      channel.Topic,
			TopicSetBy: channel.TopicSetBy,
			TopicSetAt: channel.TopicSetAt,
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
// The caller passes its own nick snapshot so the `ChannelJoin` and
// `ChannelModeChange` events emitted back-to-back for the same join
// record the same nick, even if the actor renames between them.
func (s *Session) grantVoice(ctx context.Context, ch domain.ChannelName, channel domain.Channel, inst *domain.Instance, nick domain.Nick, now time.Time) error {
	channel.Members.SetMode(inst, domain.ModeVoice)

	if err := s.persistChannel(ctx, channel); err != nil {
		return fmt.Errorf("save channel after voice: %w", err)
	}

	s.persistAndEmitUIOnly(ctx, ch, domain.ChannelModeChange{
		Channel:    ch,
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

	channel, err := s.loadChannel(ctx, ch)
	if err != nil {
		return fmt.Errorf("channel not found: %w", err)
	}

	if channel.Kind == domain.KindStatus {
		return errWithKind(fmt.Errorf("cannot part status channel"), observability.ErrorKindValidation)
	}

	isUser := actor == s.user

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actor.ID())))

	m, ok := channel.Members.GetByInstance(actor)
	if !ok {
		return nil
	}

	channel.Members.Remove(m)

	if err := s.persistChannel(ctx, channel); err != nil {
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
	s.persistAndEmit(ctx, ch, domain.ChannelPart{
		Channel:    ch,
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

	for _, ch := range channels {
		s.removeInstanceFromChannel(ctx, actor, ch)
		s.persistAndEmit(ctx, ch, domain.ChannelQuit{
			Channel:    ch,
			Nick:       actorNick,
			InstanceID: actorID,
			Message:    message,
			At:         now,
			Instance:   actor,
		})
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

	var channelNames []domain.ChannelName

	if isUser {
		channels, _ := s.loadChannels(ctx)
		for _, ch := range channels {
			if ch.Members.HasInstance(actor) {
				channelNames = append(channelNames, ch.Name)
			}
		}
	} else {
		// The instances table is keyed by InstanceID, so a rename is
		// an in-place update of the `nick` column — no delete-then-
		// reinsert needed as it was under the old nick-keyed schema.
		if err := s.store.SaveInstance(ctx, actor); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}

		channelNames = s.instanceChannelNames(actor)
	}

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actor.ID())))

	now := s.now()
	for _, chName := range channelNames {
		channel, err := s.loadChannel(ctx, chName)
		if err != nil {
			continue
		}

		channel.Members.RenameTo(actor, newNick)

		if err := s.persistChannel(ctx, channel); err != nil {
			return fmt.Errorf("save channel: %w", err)
		}

		s.persistAndEmit(ctx, chName, domain.ChannelNickChange{
			Channel:    chName,
			OldNick:    oldNick,
			NewNick:    newNick,
			InstanceID: actor.ID(),
			At:         now,
			Instance:   actor,
		})
	}

	return nil
}

// SendMessageAs records a message from the given actor.
func (s *Session) SendMessageAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, body string) (retErr error) {
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
		return errWithKind(domain.StatusChannelGuardError{
			Command: "send",
			Hint:    "the status channel doesn't take messages — try /msg <nick> for a model or /join <channel> for a channel",
		}, observability.ErrorKindValidation)
	}

	instanceID := actor.ID()
	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(instanceID)))

	cm := domain.ChannelMessage{
		Channel:    ch,
		From:       actorNick,
		InstanceID: instanceID,
		Body:       body,
		At:         s.now(),
	}

	s.persistAndEmit(ctx, ch, cm)

	return nil
}

// SendActionAs records an action message from the given actor.
func (s *Session) SendActionAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, body string) (retErr error) {
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
		return errWithKind(domain.StatusChannelGuardError{
			Command: "me",
			Hint:    "the status channel doesn't take messages — try /msg <nick> for a model or /join <channel> for a channel",
		}, observability.ErrorKindValidation)
	}

	instanceID := actor.ID()
	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(instanceID)))

	cm := domain.ChannelMessage{
		Channel:    ch,
		From:       actorNick,
		InstanceID: instanceID,
		Body:       body,
		Action:     true,
		At:         s.now(),
	}

	s.persistAndEmit(ctx, ch, cm)

	return nil
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

	now := s.now()

	channel, err := s.loadChannel(ctx, ch)
	if err != nil {
		return fmt.Errorf("get channel: %w", err)
	}

	if channel.Kind == domain.KindDM {
		return errWithKind(fmt.Errorf("cannot set topic on a direct message"), observability.ErrorKindValidation)
	}

	channel.Topic = topic
	channel.TopicSetBy = actorNick
	channel.TopicSetAt = now

	if err := s.persistChannel(ctx, channel); err != nil {
		return fmt.Errorf("save channel: %w", err)
	}

	s.persistAndEmit(ctx, ch, domain.ChannelTopicChange{
		Channel:    ch,
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

	channel, err := s.loadChannel(ctx, ch)
	if err != nil {
		return fmt.Errorf("get channel: %w", err)
	}

	if channel.Kind == domain.KindDM {
		return errWithKind(fmt.Errorf("cannot kick from a direct message"), observability.ErrorKindValidation)
	}

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(target.ID())))

	if !channel.Members.HasInstance(target) {
		return nil
	}

	if m, ok := channel.Members.GetByInstance(target); ok {
		channel.Members.Remove(m)
	}

	if err := s.persistChannel(ctx, channel); err != nil {
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
	s.persistAndEmit(ctx, ch, domain.ChannelModelKicked{
		Channel:    ch,
		Nick:       targetNick,
		InstanceID: target.ID(),
		By:         actorNick,
		At:         now,
		Instance:   target,
	})

	return nil
}

// OpenDMAs opens or creates a DM for the acting actor and target.
func (s *Session) OpenDMAs(ctx context.Context, actor, target *domain.Instance) (_ domain.Channel, _ bool, retErr error) {
	if actor == s.user {
		return s.OpenDM(ctx, target)
	}

	actorNick := actor.Nick()
	targetNick := target.Nick()

	ctx, span := s.startSpan(
		ctx,
		"session.open_dm",
		attribute.String(observability.AttrOperation, "session.open_dm"),
		attribute.String(observability.AttrNick, string(actorNick)),
		attribute.String("nick.target", string(targetNick)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	if domain.ChannelName(targetNick) == domain.StatusChannelName {
		return domain.Channel{}, false, errWithKind(domain.StatusChannelGuardError{
			Command: "msg",
			Hint:    "to message a model, use /msg <nick> with the model's name; &modeloff is a server channel.",
		}, observability.ErrorKindValidation)
	}

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actor.ID())))

	name := domain.ChannelName(targetNick)
	ch, err := s.loadChannel(ctx, name)
	created := false

	if err != nil {
		members := domain.NewMemberList()
		members.Add(actor)
		members.Add(target)

		ch = domain.Channel{
			Name:    name,
			Kind:    domain.KindDM,
			Members: members,
			Created: s.now(),
		}

		if err := s.persistChannel(ctx, ch); err != nil {
			return domain.Channel{}, false, fmt.Errorf("save dm channel: %w", err)
		}

		created = true
	}

	now := s.now()

	actor.MutateChannels(func(m *orderedmap.OrderedMap[domain.ChannelName, time.Time]) {
		if _, ok := m.Get(name); !ok {
			m.Set(name, now)
		}
	})

	if actor != s.user {
		if err := s.store.SaveInstance(ctx, actor); err != nil {
			return domain.Channel{}, false, fmt.Errorf("save actor instance: %w", err)
		}
	}

	target.MutateChannels(func(m *orderedmap.OrderedMap[domain.ChannelName, time.Time]) {
		if _, ok := m.Get(name); !ok {
			m.Set(name, now)
		}
	})

	if target != s.user {
		if err := s.store.SaveInstance(ctx, target); err != nil {
			return domain.Channel{}, false, fmt.Errorf("save target instance: %w", err)
		}
	}

	return ch, created, nil
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
	s.appendEvent(ctx, ch, domain.ChannelSystemNotice{Channel: ch, Text: notice, At: now})

	return nil
}
