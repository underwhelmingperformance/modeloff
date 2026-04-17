package session

import (
	"context"
	"fmt"
	"strings"
	"time"

	orderedmap "github.com/wk8/go-ordered-map/v2"
	"go.opentelemetry.io/otel/attribute"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
)

// JoinAs joins the given actor to a channel.
func (s *Session) JoinAs(ctx context.Context, actor domain.Nick, ch domain.ChannelName) (retErr error) {
	ch = domain.NormaliseChannelName(ch)

	ctx, span := startSpan(
		ctx,
		"session.join",
		attribute.String(observability.AttrOperation, "session.join"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actor)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	now := s.now()
	isUser := actor == s.userSnapshot().Nick

	var actorID domain.InstanceID
	if !isUser {
		if inst, err := s.store.GetInstance(ctx, actor); err == nil {
			actorID = inst.InstanceID
		}
	}

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actorID)))

	channel, err := s.store.GetChannel(ctx, ch)
	created := false

	if err == nil && channel.Members.Len() == 0 {
		channel.Members = domain.NewMemberList()
	}

	if err != nil {
		members := domain.NewMemberList()
		members.Add(actorID, actor)

		channel = domain.Channel{
			Name:    ch,
			Kind:    domain.KindChannel,
			Members: members,
			Created: now,
		}

		if saveErr := s.store.SaveChannel(ctx, channel); saveErr != nil {
			return fmt.Errorf("save channel: %w", saveErr)
		}

		created = true
	}

	alreadyMember := !created && channel.Members.HasID(actorID)

	// The created branch already added the actor to the fresh member
	// list when constructing the channel, so we only add (and save)
	// here when joining an existing channel the actor isn't already a
	// member of.
	if !created && !alreadyMember {
		channel.Members.Add(actorID, actor)

		if err := s.store.SaveChannel(ctx, channel); err != nil {
			return fmt.Errorf("save channel: %w", err)
		}
	}

	if isUser {
		if err := s.store.SetLastChannel(ctx, ch); err != nil {
			return fmt.Errorf("set last channel: %w", err)
		}

		if err := s.MarkRead(ctx, ch); err != nil {
			return fmt.Errorf("mark read: %w", err)
		}

		if !alreadyMember {
			s.mutateUser(func(u *domain.Instance) {
				u.Channels.Set(ch, now)
			})
		}
	} else {
		inst, err := s.store.GetInstance(ctx, actor)
		if err == nil {
			if inst.Channels == nil {
				inst.Channels = orderedmap.New[domain.ChannelName, time.Time]()
			}

			if _, ok := inst.Channels.Get(ch); !ok {
				inst.Channels.Set(ch, now)
			}

			if err := s.store.SaveInstance(ctx, inst); err != nil {
				return fmt.Errorf("save instance: %w", err)
			}
		}
	}

	if !alreadyMember && channel.Kind != domain.KindDM {
		s.appendEvent(ctx, ch, domain.ChannelJoin{Channel: ch, Nick: actor, Created: created, At: now})
		s.emit(ctx, domain.JoinEvent{Channel: ch, InstanceID: actorID, Nick: actor, Created: created, At: now})
	}

	if !alreadyMember && channel.Kind != domain.KindDM {
		channel, _ = s.store.GetChannel(ctx, ch)

		if isUser {
			if err := s.emitJoinProtocol(ctx, ch, channel, now); err != nil {
				return err
			}
		} else {
			if err := s.grantVoice(ctx, ch, channel, actorID, actor, now); err != nil {
				return err
			}
		}
	}

	return nil
}

// emitJoinProtocol sets the user's mode to +o, emits topic info if
// the channel has a topic, and saves the autojoin list.
func (s *Session) emitJoinProtocol(ctx context.Context, ch domain.ChannelName, channel domain.Channel, now time.Time) error {
	userNick := s.userSnapshot().Nick
	channel.Members.SetMode("", domain.ModeOp)

	if err := s.store.SaveChannel(ctx, channel); err != nil {
		return fmt.Errorf("save channel after mode: %w", err)
	}

	s.appendEvent(ctx, ch, domain.ChannelModeChange{
		Channel: ch, Nick: userNick, Mode: domain.ModeOp, By: "ChanServ", At: now,
	})
	s.emitUIOnly(domain.ModeChangeEvent{
		Channel: ch, Nick: userNick, InstanceID: "", Mode: domain.ModeOp, Actor: "ChanServ", At: now,
	})

	if channel.Topic != "" {
		s.emitUIOnly(domain.TopicInfoEvent{
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
func (s *Session) grantVoice(ctx context.Context, ch domain.ChannelName, channel domain.Channel, instanceID domain.InstanceID, nick domain.Nick, now time.Time) error {
	channel.Members.SetMode(instanceID, domain.ModeVoice)

	if err := s.store.SaveChannel(ctx, channel); err != nil {
		return fmt.Errorf("save channel after voice: %w", err)
	}

	s.appendEvent(ctx, ch, domain.ChannelModeChange{
		Channel: ch, Nick: nick, Mode: domain.ModeVoice, By: "ChanServ", At: now,
	})
	s.emitUIOnly(domain.ModeChangeEvent{
		Channel: ch, InstanceID: instanceID, Nick: nick, Mode: domain.ModeVoice, Actor: "ChanServ", At: now,
	})

	return nil
}

// PartAs parts the given actor from a channel.
func (s *Session) PartAs(ctx context.Context, actor domain.Nick, ch domain.ChannelName, message string) (retErr error) {
	ctx, span := startSpan(
		ctx,
		"session.part",
		attribute.String(observability.AttrOperation, "session.part"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actor)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	channel, err := s.store.GetChannel(ctx, ch)
	if err != nil {
		return fmt.Errorf("channel not found: %w", err)
	}

	if channel.Kind == domain.KindStatus {
		return errWithKind(fmt.Errorf("cannot part status channel"), observability.ErrorKindValidation)
	}

	isUser := actor == s.userSnapshot().Nick

	var (
		actorID      domain.InstanceID
		actorIsKnown bool
	)

	if isUser {
		actorIsKnown = true
	} else if inst, err := s.store.GetInstance(ctx, actor); err == nil {
		actorID = inst.InstanceID
		actorIsKnown = true
	}

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actorID)))

	// Refuse to act when a non-user actor cannot be resolved. Mirrors
	// the same guard in `KickAs`: defaulting `actorID` to the
	// empty-id human sentinel would make the human vanish from the
	// channel via the subsequent `GetByID("")` lookup, so an unknown
	// actor must be a no-op.
	if !actorIsKnown {
		return nil
	}

	if m, ok := channel.Members.GetByID(actorID); ok {
		channel.Members.Remove(m)
	}

	if err := s.store.SaveChannel(ctx, channel); err != nil {
		return fmt.Errorf("save channel: %w", err)
	}

	if isUser {
		s.mutateUser(func(u *domain.Instance) {
			u.Channels.Delete(ch)
		})

		if err := s.saveAutojoinList(ctx); err != nil {
			return fmt.Errorf("save autojoin: %w", err)
		}
	} else if inst, err := s.store.GetInstance(ctx, actor); err == nil {
		inst.Channels.Delete(ch)

		if err := s.store.SaveInstance(ctx, inst); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}
	}

	now := s.now()
	s.appendEvent(ctx, ch, domain.ChannelPart{Channel: ch, Nick: actor, Message: message, At: now})
	s.emit(ctx, domain.PartEvent{Channel: ch, InstanceID: actorID, Nick: actor, Message: message, At: now})

	return nil
}

// QuitAs quits the given actor from every joined channel.
func (s *Session) QuitAs(ctx context.Context, actor domain.Nick, message string) (retErr error) {
	if actor == s.userSnapshot().Nick {
		return s.Quit(ctx, message)
	}

	ctx, span := startSpan(
		ctx,
		"session.quit",
		attribute.String(observability.AttrOperation, "session.quit"),
		attribute.String(observability.AttrNick, string(actor)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	inst, err := s.store.GetInstance(ctx, actor)
	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(inst.InstanceID)))

	now := s.now()
	for _, ch := range s.instanceChannelNames(inst) {
		s.removeInstanceFromChannel(ctx, inst.InstanceID, actor, ch)
		s.appendEvent(ctx, ch, domain.ChannelQuit{Channel: ch, Nick: actor, Message: message, At: now})
	}

	if err := s.store.DeleteInstance(ctx, actor); err != nil {
		return fmt.Errorf("delete instance: %w", err)
	}

	s.emitUIOnly(domain.QuitEvent{InstanceID: inst.InstanceID, Nick: actor, Message: message, At: now})

	return nil
}

// ChangeNickAs changes the given actor's nickname.
func (s *Session) ChangeNickAs(ctx context.Context, actor domain.Nick, newNick domain.Nick) (retErr error) {
	ctx, span := startSpan(
		ctx,
		"session.change_nick",
		attribute.String(observability.AttrOperation, "session.change_nick"),
		attribute.String(observability.AttrNick, string(actor)),
		attribute.String("nick.new", string(newNick)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	isUser := actor == s.userSnapshot().Nick

	var (
		actorID      domain.InstanceID
		channelNames []domain.ChannelName
	)

	if isUser {
		s.mutateUser(func(u *domain.Instance) {
			u.Nick = newNick
		})

		channels, _ := s.store.ListChannels(ctx)
		for _, ch := range channels {
			if ch.Members.HasID("") {
				channelNames = append(channelNames, ch.Name)
			}
		}
	} else {
		inst, err := s.store.GetInstance(ctx, actor)
		if err != nil {
			return fmt.Errorf("get instance: %w", err)
		}

		actorID = inst.InstanceID

		if err := s.store.DeleteInstance(ctx, actor); err != nil {
			return fmt.Errorf("delete old instance: %w", err)
		}

		inst.Nick = newNick

		if err := s.store.SaveInstance(ctx, inst); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}

		channelNames = s.instanceChannelNames(inst)
	}

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actorID)))

	now := s.now()
	for _, chName := range channelNames {
		channel, err := s.store.GetChannel(ctx, chName)
		if err != nil {
			continue
		}

		channel.Members.RenameTo(actorID, newNick)

		if err := s.store.SaveChannel(ctx, channel); err != nil {
			return fmt.Errorf("save channel: %w", err)
		}

		s.appendEvent(ctx, chName, domain.ChannelNickChange{
			Channel: chName, OldNick: actor, NewNick: newNick, At: now,
		})
		s.emit(ctx, domain.NickChangeEvent{
			Channel: chName, InstanceID: actorID, OldNick: actor, NewNick: newNick, At: now,
		})
	}

	return nil
}

// SendMessageAs records a message from the given actor.
func (s *Session) SendMessageAs(ctx context.Context, actor domain.Nick, ch domain.ChannelName, body string) (retErr error) {
	ctx, span := startSpan(
		ctx,
		"session.send_message",
		attribute.String(observability.AttrOperation, "session.send_message"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actor)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	if ch == domain.StatusChannelName {
		return errWithKind(domain.StatusChannelGuardError{
			Command: "send",
			Hint:    "the status channel doesn't take messages — try /msg <nick> for a model or /join <channel> for a channel",
		}, observability.ErrorKindValidation)
	}

	var instanceID domain.InstanceID
	if inst, err := s.store.GetInstance(ctx, actor); err == nil {
		instanceID = inst.InstanceID
	}

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(instanceID)))

	cm := domain.ChannelMessage{
		Channel:    ch,
		From:       actor,
		InstanceID: instanceID,
		Body:       body,
		At:         s.now(),
	}

	s.appendEvent(ctx, ch, cm)
	s.emit(ctx, domain.MessageEvent{Event: cm})

	return nil
}

// SendActionAs records an action message from the given actor.
func (s *Session) SendActionAs(ctx context.Context, actor domain.Nick, ch domain.ChannelName, body string) (retErr error) {
	ctx, span := startSpan(
		ctx,
		"session.send_action",
		attribute.String(observability.AttrOperation, "session.send_action"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actor)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	if ch == domain.StatusChannelName {
		return errWithKind(domain.StatusChannelGuardError{
			Command: "me",
			Hint:    "the status channel doesn't take messages — try /msg <nick> for a model or /join <channel> for a channel",
		}, observability.ErrorKindValidation)
	}

	var instanceID domain.InstanceID
	if inst, err := s.store.GetInstance(ctx, actor); err == nil {
		instanceID = inst.InstanceID
	}

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(instanceID)))

	cm := domain.ChannelMessage{
		Channel:    ch,
		From:       actor,
		InstanceID: instanceID,
		Body:       body,
		Action:     true,
		At:         s.now(),
	}

	s.appendEvent(ctx, ch, cm)
	s.emit(ctx, domain.MessageEvent{Event: cm})

	return nil
}

// SetTopicAs sets the topic for a channel.
func (s *Session) SetTopicAs(ctx context.Context, actor domain.Nick, ch domain.ChannelName, topic string) (retErr error) {
	ctx, span := startSpan(
		ctx,
		"session.set_topic",
		attribute.String(observability.AttrOperation, "session.set_topic"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actor)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	var actorID domain.InstanceID
	if actor != s.userSnapshot().Nick {
		if inst, err := s.store.GetInstance(ctx, actor); err == nil {
			actorID = inst.InstanceID
		}
	}

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actorID)))

	now := s.now()

	channel, err := s.store.GetChannel(ctx, ch)
	if err != nil {
		return fmt.Errorf("get channel: %w", err)
	}

	if channel.Kind == domain.KindDM {
		return errWithKind(fmt.Errorf("cannot set topic on a direct message"), observability.ErrorKindValidation)
	}

	channel.Topic = topic
	channel.TopicSetBy = actor
	channel.TopicSetAt = now

	if err := s.store.SaveChannel(ctx, channel); err != nil {
		return fmt.Errorf("save channel: %w", err)
	}

	s.appendEvent(ctx, ch, domain.ChannelTopicChange{Channel: ch, Topic: topic, By: actor, At: now})
	s.emit(ctx, domain.TopicChangeEvent{Channel: ch, Topic: topic, By: actor, At: now})

	return nil
}

// KickAs removes a nick from a channel on behalf of the actor.
func (s *Session) KickAs(ctx context.Context, actor domain.Nick, target domain.Nick, ch domain.ChannelName) (retErr error) {
	ctx, span := startSpan(
		ctx,
		"session.kick",
		attribute.String(observability.AttrOperation, "session.kick"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(target)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	channel, err := s.store.GetChannel(ctx, ch)
	if err != nil {
		return fmt.Errorf("get channel: %w", err)
	}

	if channel.Kind == domain.KindDM {
		return errWithKind(fmt.Errorf("cannot kick from a direct message"), observability.ErrorKindValidation)
	}

	isUser := target == s.userSnapshot().Nick

	var (
		targetID      domain.InstanceID
		targetIsKnown bool
	)

	if isUser {
		targetIsKnown = true
	} else if inst, err := s.store.GetInstance(ctx, target); err == nil {
		targetID = inst.InstanceID
		targetIsKnown = true
	}

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(targetID)))

	// Refuse to act when the target nick cannot be resolved to a
	// known identity. Defaulting `targetID` to the empty-id
	// human-user sentinel would cause membership removals (and the
	// ModelKickedEvent that follows) to point at the user, so a
	// `/kick <typo>` must be a no-op — no stored mutation, no event
	// emission.
	if !targetIsKnown {
		return nil
	}

	if m, ok := channel.Members.GetByID(targetID); ok {
		channel.Members.Remove(m)
	}

	if err := s.store.SaveChannel(ctx, channel); err != nil {
		return fmt.Errorf("save channel: %w", err)
	}

	if isUser {
		s.mutateUser(func(u *domain.Instance) {
			u.Channels.Delete(ch)
		})
	} else if inst, err := s.store.GetInstance(ctx, target); err == nil {
		inst.Channels.Delete(ch)

		if err := s.store.SaveInstance(ctx, inst); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}
	}

	now := s.now()
	s.appendEvent(ctx, ch, domain.ChannelModelKicked{Channel: ch, Nick: target, By: actor, At: now})
	s.emit(ctx, domain.ModelKickedEvent{Channel: ch, InstanceID: targetID, Nick: target, By: actor, At: now})

	return nil
}

// OpenDMAs opens or creates a DM for the acting actor and target.
func (s *Session) OpenDMAs(ctx context.Context, actor domain.Nick, target domain.Nick) (_ domain.Channel, _ bool, retErr error) {
	if actor == s.userSnapshot().Nick {
		return s.OpenDM(ctx, target)
	}

	ctx, span := startSpan(
		ctx,
		"session.open_dm",
		attribute.String(observability.AttrOperation, "session.open_dm"),
		attribute.String(observability.AttrNick, string(actor)),
		attribute.String("nick.target", string(target)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	if domain.ChannelName(target) == domain.StatusChannelName {
		return domain.Channel{}, false, errWithKind(domain.StatusChannelGuardError{
			Command: "msg",
			Hint:    "to message a model, use /msg <nick> with the model's name; &modeloff is a server channel.",
		}, observability.ErrorKindValidation)
	}

	userNick := s.userSnapshot().Nick

	// Resolve the actor's identity up-front so the span carries it;
	// the human is always keyed with the empty instance id.
	var actorID domain.InstanceID
	if actor != userNick {
		if inst, err := s.store.GetInstance(ctx, actor); err == nil {
			actorID = inst.InstanceID
		}
	}

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actorID)))

	name := domain.ChannelName(target)
	ch, err := s.store.GetChannel(ctx, name)
	created := false

	if err != nil {
		members := domain.NewMemberList()

		var targetID domain.InstanceID

		if target != userNick {
			if inst, err := s.store.GetInstance(ctx, target); err == nil {
				targetID = inst.InstanceID
			}
		}

		if target == userNick {
			members.Add("", userNick)
			members.Add(actorID, actor)
		} else {
			members.Add(actorID, actor)
			members.Add(targetID, target)
		}

		ch = domain.Channel{
			Name:    name,
			Kind:    domain.KindDM,
			Members: members,
			Created: s.now(),
		}

		if err := s.store.SaveChannel(ctx, ch); err != nil {
			return domain.Channel{}, false, fmt.Errorf("save dm channel: %w", err)
		}

		created = true
	}

	now := s.now()

	if inst, err := s.store.GetInstance(ctx, actor); err == nil {
		if inst.Channels == nil {
			inst.Channels = orderedmap.New[domain.ChannelName, time.Time]()
		}

		if _, ok := inst.Channels.Get(name); !ok {
			inst.Channels.Set(name, now)

			if err := s.store.SaveInstance(ctx, inst); err != nil {
				return domain.Channel{}, false, fmt.Errorf("save actor instance: %w", err)
			}
		}
	}

	if target != s.userSnapshot().Nick {
		if inst, err := s.store.GetInstance(ctx, target); err == nil {
			if inst.Channels == nil {
				inst.Channels = orderedmap.New[domain.ChannelName, time.Time]()
			}

			if _, ok := inst.Channels.Get(name); !ok {
				inst.Channels.Set(name, now)

				if err := s.store.SaveInstance(ctx, inst); err != nil {
					return domain.Channel{}, false, fmt.Errorf("save target instance: %w", err)
				}
			}
		}
	}

	return ch, created, nil
}

// InviteAs sends a real IRC-style invite.
func (s *Session) InviteAs(ctx context.Context, actor domain.Nick, target domain.Nick, ch domain.ChannelName) error {
	return s.inviteActor(ctx, actor, target, ch)
}

func (s *Session) inviteActor(ctx context.Context, actor domain.Nick, target domain.Nick, ch domain.ChannelName) (retErr error) {
	ctx, span := startSpan(
		ctx,
		"session.invite",
		attribute.String(observability.AttrOperation, "session.invite"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actor)),
		attribute.String("nick.target", string(target)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	target = domain.Nick(strings.TrimSpace(string(target)))
	if target == "" {
		return fmt.Errorf("target nick is required")
	}

	if actor == s.userSnapshot().Nick {
		if inst, err := s.store.GetInstance(ctx, target); err == nil {
			span.SetAttributes(attribute.String(observability.AttrInstanceID, string(inst.InstanceID)))
			return s.attachInstanceToChannel(ctx, ch, inst, actor)
		}
	}

	now := s.now()
	notice := fmt.Sprintf("%s invited %s to %s", actor, target, ch)
	s.appendEvent(ctx, ch, domain.ChannelSystemNotice{Channel: ch, Text: notice, At: now})

	return nil
}
