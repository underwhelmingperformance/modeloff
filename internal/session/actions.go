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
	defer endSpan(span, &retErr)

	now := s.now()
	isUser := actor == s.user.Nick

	channel, err := s.store.GetChannel(ctx, ch)
	created := false

	if err != nil {
		members := domain.NewMemberList()
		members.Add(actor)

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

	alreadyMember := !created && channel.Members.Has(actor)

	if !alreadyMember {
		channel.Members.Add(actor)

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

		s.user.Channels.Set(ch, now)
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
		s.emit(ctx, domain.JoinEvent{Channel: ch, Nick: actor, Created: created, At: now})
	}

	if !alreadyMember && created && isUser && channel.Kind != domain.KindDM {
		channel, _ = s.store.GetChannel(ctx, ch)
		channel.Members.SetMode(actor, domain.ModeOp)

		if err := s.store.SaveChannel(ctx, channel); err != nil {
			return fmt.Errorf("save channel after mode: %w", err)
		}

		s.appendEvent(ctx, ch, domain.ChannelModeChange{
			Channel: ch, Nick: actor, Mode: domain.ModeOp, At: now,
		})
		s.emit(ctx, domain.ModeChangeEvent{
			Channel: ch, Nick: actor, Mode: domain.ModeOp, Actor: "ChanServ", At: now,
		})
	}

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
	defer endSpan(span, &retErr)

	channel, err := s.store.GetChannel(ctx, ch)
	if err != nil {
		return fmt.Errorf("channel not found: %w", err)
	}

	if m, ok := channel.Members.Get(actor); ok {
		channel.Members.Remove(m)
	}

	if err := s.store.SaveChannel(ctx, channel); err != nil {
		return fmt.Errorf("save channel: %w", err)
	}

	if actor == s.user.Nick {
		s.user.Channels.Delete(ch)
	} else if inst, err := s.store.GetInstance(ctx, actor); err == nil {
		inst.Channels.Delete(ch)

		if err := s.store.SaveInstance(ctx, inst); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}
	}

	now := s.now()
	s.appendEvent(ctx, ch, domain.ChannelPart{Channel: ch, Nick: actor, Message: message, At: now})
	s.emit(ctx, domain.PartEvent{Channel: ch, Nick: actor, Message: message, At: now})

	return nil
}

// QuitAs quits the given actor from every joined channel.
func (s *Session) QuitAs(ctx context.Context, actor domain.Nick, message string) (retErr error) {
	if actor == s.user.Nick {
		return s.Quit(ctx, message)
	}

	ctx, span := startSpan(
		ctx,
		"session.quit",
		attribute.String(observability.AttrOperation, "session.quit"),
		attribute.String(observability.AttrNick, string(actor)),
	)
	defer endSpan(span, &retErr)

	inst, err := s.store.GetInstance(ctx, actor)
	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}

	now := s.now()
	for _, ch := range s.instanceChannelNames(inst) {
		s.removeInstanceFromChannel(ctx, actor, ch)
		s.appendEvent(ctx, ch, domain.ChannelQuit{Channel: ch, Nick: actor, Message: message, At: now})
	}

	if err := s.store.DeleteInstance(ctx, actor); err != nil {
		return fmt.Errorf("delete instance: %w", err)
	}

	s.emitUIOnly(domain.QuitEvent{Nick: actor, Message: message, At: now})

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
	defer endSpan(span, &retErr)

	isUser := actor == s.user.Nick

	var channelNames []domain.ChannelName

	if isUser {
		s.user.Nick = newNick

		channels, _ := s.store.ListChannels(ctx)
		for _, ch := range channels {
			if ch.Members.Has(actor) {
				channelNames = append(channelNames, ch.Name)
			}
		}
	} else {
		inst, err := s.store.GetInstance(ctx, actor)
		if err != nil {
			return fmt.Errorf("get instance: %w", err)
		}

		if err := s.store.DeleteInstance(ctx, actor); err != nil {
			return fmt.Errorf("delete old instance: %w", err)
		}

		inst.Nick = newNick

		if err := s.store.SaveInstance(ctx, inst); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}

		channelNames = s.instanceChannelNames(inst)
	}

	now := s.now()
	for _, chName := range channelNames {
		channel, err := s.store.GetChannel(ctx, chName)
		if err != nil {
			continue
		}

		if m, ok := channel.Members.Get(actor); ok {
			channel.Members.Remove(m)
			channel.Members.Add(newNick)
			channel.Members.SetMode(newNick, m.Mode)
		}

		if err := s.store.SaveChannel(ctx, channel); err != nil {
			return fmt.Errorf("save channel: %w", err)
		}

		s.appendEvent(ctx, chName, domain.ChannelNickChange{
			Channel: chName, OldNick: actor, NewNick: newNick, At: now,
		})
		s.emit(ctx, domain.NickChangeEvent{
			Channel: chName, OldNick: actor, NewNick: newNick, At: now,
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
	defer endSpan(span, &retErr)

	var instanceID string
	if inst, err := s.store.GetInstance(ctx, actor); err == nil {
		instanceID = inst.InstanceID
	}

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
	defer endSpan(span, &retErr)

	var instanceID string
	if inst, err := s.store.GetInstance(ctx, actor); err == nil {
		instanceID = inst.InstanceID
	}

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
	defer endSpan(span, &retErr)

	now := s.now()

	channel, err := s.store.GetChannel(ctx, ch)
	if err != nil {
		return fmt.Errorf("get channel: %w", err)
	}

	if channel.Kind == domain.KindDM {
		return fmt.Errorf("cannot set topic on a direct message")
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

// KickAs removes a nick from a channel.
func (s *Session) KickAs(ctx context.Context, target domain.Nick, ch domain.ChannelName) (retErr error) {
	ctx, span := startSpan(
		ctx,
		"session.kick",
		attribute.String(observability.AttrOperation, "session.kick"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(target)),
	)
	defer endSpan(span, &retErr)

	channel, err := s.store.GetChannel(ctx, ch)
	if err != nil {
		return fmt.Errorf("get channel: %w", err)
	}

	if channel.Kind == domain.KindDM {
		return fmt.Errorf("cannot kick from a direct message")
	}

	if m, ok := channel.Members.Get(target); ok {
		channel.Members.Remove(m)
	}

	if err := s.store.SaveChannel(ctx, channel); err != nil {
		return fmt.Errorf("save channel: %w", err)
	}

	if target == s.user.Nick {
		s.user.Channels.Delete(ch)
	} else if inst, err := s.store.GetInstance(ctx, target); err == nil {
		inst.Channels.Delete(ch)

		if err := s.store.SaveInstance(ctx, inst); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}
	}

	now := s.now()
	s.appendEvent(ctx, ch, domain.ChannelModelKicked{Channel: ch, Nick: target, At: now})
	s.emit(ctx, domain.ModelKickedEvent{Channel: ch, Nick: target, At: now})

	return nil
}

// OpenDMAs opens or creates a DM for the acting actor and target.
func (s *Session) OpenDMAs(ctx context.Context, actor domain.Nick, target domain.Nick) (_ domain.Channel, _ bool, retErr error) {
	if actor == s.user.Nick {
		return s.OpenDM(ctx, target)
	}

	ctx, span := startSpan(
		ctx,
		"session.open_dm",
		attribute.String(observability.AttrOperation, "session.open_dm"),
		attribute.String(observability.AttrNick, string(actor)),
		attribute.String("nick.target", string(target)),
	)
	defer endSpan(span, &retErr)

	name := domain.ChannelName(target)
	ch, err := s.store.GetChannel(ctx, name)
	created := false

	if err != nil {
		members := domain.NewMemberList()

		if target == s.user.Nick {
			members.Add(s.user.Nick)
			members.Add(actor)
		} else {
			members.Add(actor)
			members.Add(target)
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

	if target != s.user.Nick {
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
	defer endSpan(span, &retErr)

	target = domain.Nick(strings.TrimSpace(string(target)))
	if target == "" {
		return fmt.Errorf("target nick is required")
	}

	if actor == s.user.Nick {
		if inst, err := s.store.GetInstance(ctx, target); err == nil {
			return s.attachInstanceToChannel(ctx, ch, inst)
		}
	}

	now := s.now()
	notice := fmt.Sprintf("%s invited %s to %s", actor, target, ch)
	s.appendEvent(ctx, ch, domain.ChannelSystemNotice{Channel: ch, Text: notice, At: now})

	return nil
}
