package screens

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/components"
)

func (s *ChatScreen) channelMembers(ch domain.ChannelName) []domain.Member {
	if ch == "" {
		return nil
	}

	channel, err := s.sess.GetChannel(s.ctx, ch)
	if err != nil {
		return nil
	}

	return s.sortedMembers(channel.Members)
}

func noChannelMsg() tea.Msg {
	return systemEventMsg{events: []components.ChatLine{components.NoChannel{}}}
}

func errorEvent(err error) systemEventMsg {
	return systemEventMsg{events: []components.ChatLine{
		components.CommandError{Err: err},
	}}
}

func (s *ChatScreen) handleCommand(msg components.CommandSubmitMsg) tea.Cmd {
	cmd, err := command.Execute(s.CommandScope(), msg.Raw)
	if err != nil {
		return func() tea.Msg { return errorEvent(err) }
	}

	return cmd
}

func (s *ChatScreen) configure(cmd command.ConfigCommand) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		switch cmd.Key {
		case "":
			return systemEventMsg{events: []components.ChatLine{
				components.UsageHint{Command: "config"},
			}}

		case "api-key":
			value := strings.TrimSpace(strings.Join(cmd.Value, " "))
			if value == "" {
				return systemEventMsg{events: []components.ChatLine{
					components.UsageHint{Command: "config api-key"},
				}}
			}

			if _, err := s.sess.SetAPIKey(ctx, value); err != nil {
				return errorEvent(err)
			}

			return apiKeyActivatedMsg{}

		case "poke-interval":
			value := strings.TrimSpace(strings.Join(cmd.Value, " "))
			if value == "" {
				return systemEventMsg{events: []components.ChatLine{
					components.UsageHint{Command: "config poke-interval"},
				}}
			}

			interval, err := time.ParseDuration(value)
			if err != nil {
				return errorEvent(domain.InvalidDurationError{
					Input: value,
					Err:   err,
				})
			}

			if _, err := s.sess.SetPokeInterval(ctx, interval); err != nil {
				return errorEvent(err)
			}

			return systemEventMsg{events: []components.ChatLine{
				components.PokeIntervalSet{Interval: interval},
			}}

		case "nick-model":
			value := strings.TrimSpace(strings.Join(cmd.Value, " "))
			if value == "" {
				return systemEventMsg{events: []components.ChatLine{
					components.UsageHint{Command: "config nick-model"},
				}}
			}

			modelID := domain.ModelID(value)

			if _, err := s.sess.SetNickModel(ctx, modelID); err != nil {
				return errorEvent(err)
			}

			return systemEventMsg{events: []components.ChatLine{
				components.NickModelSet{ModelID: modelID},
			}}

		default:
			return errorEvent(domain.UnknownConfigKeyError{Key: cmd.Key})
		}
	}
}

func (s *ChatScreen) directMessage(nick domain.Nick, body string) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		ch, created, err := s.sess.OpenDM(ctx, nick)
		if err != nil {
			return errorEvent(domain.UnknownNickError{Nick: nick})
		}

		if strings.TrimSpace(body) != "" {
			if _, err := s.sess.SendMessage(ctx, ch.Name, body); err != nil {
				return errorEvent(err)
			}
		}

		channels, _ := s.sess.ListChannels(ctx)
		instances, _ := s.sess.ListInstances(ctx)
		messages, _ := s.sess.Messages(ctx, ch.Name)

		var events []components.ChatLine
		if created {
			events = []components.ChatLine{components.DMOpened{Nick: nick}}
		}

		return commandResultMsg{
			channels:  channels,
			instances: instances,
			active:    ch.Name,
			topic:     "",
			messages:  messages,
			unread:    s.unreadCounts(ctx, channels),
			members:   s.channelMembers(ch.Name),
			events:    events,
		}
	}
}

func (s *ChatScreen) handlePoke() tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		if err := s.sess.Poke(ctx); err != nil {
			return errorEvent(err)
		}

		channels, _ := s.sess.ListChannels(ctx)
		instances, _ := s.sess.ListInstances(ctx)
		messages, _ := s.sess.Messages(ctx, s.active)

		return commandResultMsg{
			channels:  channels,
			instances: instances,
			active:    s.active,
			topic:     s.topic,
			messages:  messages,
			unread:    s.unreadCounts(ctx, channels),
			members:   s.channelMembers(s.active),
		}
	}
}

func (s *ChatScreen) joinChannel(name string) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		evt, err := s.sess.Join(ctx, name)
		if err != nil {
			return errorEvent(err)
		}

		channels, _ := s.sess.ListChannels(ctx)
		instances, _ := s.sess.ListInstances(ctx)
		active := domain.ChannelName(name)
		messages, _ := s.sess.Messages(ctx, active)

		var topic string
		if ch, err := s.sess.GetChannel(ctx, active); err == nil {
			topic = ch.Topic
		}

		return commandResultMsg{
			channels:  channels,
			instances: instances,
			active:    active,
			topic:     topic,
			messages:  messages,
			unread:    s.unreadCounts(ctx, channels),
			members:   s.channelMembers(active),
			events:    []components.ChatLine{components.Join{JoinEvent: evt}},
		}
	}
}

func (s *ChatScreen) leaveChannel() tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		evt, _ := s.sess.Leave(ctx, s.active)

		channels, _ := s.sess.ListChannels(ctx)
		instances, _ := s.sess.ListInstances(ctx)

		var active domain.ChannelName
		var topic string
		var messages []domain.Message

		if len(channels) > 0 {
			active = channels[0].Name
			topic = channels[0].Topic
			messages, _ = s.sess.Messages(ctx, active)
		}

		return commandResultMsg{
			channels:  channels,
			instances: instances,
			active:    active,
			topic:     topic,
			messages:  messages,
			unread:    s.unreadCounts(ctx, channels),
			members:   s.channelMembers(active),
			events:    []components.ChatLine{components.Part{PartEvent: evt}},
		}
	}
}

func (s *ChatScreen) changeNick(nick domain.Nick) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		evt, err := s.sess.ChangeNick(ctx, nick)
		if err != nil {
			return errorEvent(err)
		}

		channels, _ := s.sess.ListChannels(ctx)
		instances, _ := s.sess.ListInstances(ctx)
		messages, _ := s.sess.Messages(ctx, s.active)

		return commandResultMsg{
			channels:  channels,
			instances: instances,
			active:    s.active,
			topic:     s.topic,
			messages:  messages,
			unread:    s.unreadCounts(ctx, channels),
			members:   s.channelMembers(s.active),
			events:    []components.ChatLine{components.NickChange{NickChangeEvent: evt}},
		}
	}
}

func (s *ChatScreen) setTopic(topic string) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		evt, err := s.sess.SetTopic(ctx, s.active, topic)
		if err != nil {
			return errorEvent(err)
		}

		channels, _ := s.sess.ListChannels(ctx)
		instances, _ := s.sess.ListInstances(ctx)
		messages, _ := s.sess.Messages(ctx, s.active)

		return commandResultMsg{
			channels:  channels,
			instances: instances,
			active:    s.active,
			topic:     topic,
			messages:  messages,
			unread:    s.unreadCounts(ctx, channels),
			members:   s.channelMembers(s.active),
			events:    []components.ChatLine{components.TopicChange{TopicChangeEvent: evt}},
		}
	}
}

func (s *ChatScreen) whois(nick domain.Nick) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		inst, err := s.sess.Whois(ctx, nick)
		if err != nil {
			return errorEvent(domain.UnknownNickError{Nick: nick})
		}

		return systemEventMsg{events: []components.ChatLine{
			components.Whois{ModelInstance: inst},
		}}
	}
}

func (s *ChatScreen) inviteModel(modelID domain.ModelID, persona string) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		evt, err := s.sess.Invite(ctx, s.active, modelID, persona)
		if err != nil {
			return errorEvent(err)
		}

		channels, _ := s.sess.ListChannels(ctx)
		instances, _ := s.sess.ListInstances(ctx)
		messages, _ := s.sess.Messages(ctx, s.active)

		return commandResultMsg{
			channels:  channels,
			instances: instances,
			active:    s.active,
			topic:     s.topic,
			messages:  messages,
			unread:    s.unreadCounts(ctx, channels),
			members:   s.channelMembers(s.active),
			events:    []components.ChatLine{components.ModelInvited{ModelInvitedEvent: evt}},
		}
	}
}

func (s *ChatScreen) kickModel(nick domain.Nick) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		evt, err := s.sess.Kick(ctx, s.active, nick)
		if err != nil {
			return errorEvent(err)
		}

		channels, _ := s.sess.ListChannels(ctx)
		instances, _ := s.sess.ListInstances(ctx)
		messages, _ := s.sess.Messages(ctx, s.active)

		return commandResultMsg{
			channels:  channels,
			instances: instances,
			active:    s.active,
			topic:     s.topic,
			messages:  messages,
			unread:    s.unreadCounts(ctx, channels),
			members:   s.channelMembers(s.active),
			events:    []components.ChatLine{components.ModelKicked{ModelKickedEvent: evt}},
		}
	}
}

func (s *ChatScreen) showHelp() tea.Cmd {
	return func() tea.Msg {
		return systemEventMsg{events: []components.ChatLine{components.Help{}}}
	}
}

func (s *ChatScreen) listChannels() tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		channels, err := s.sess.ListChannels(ctx)
		if err != nil {
			return errorEvent(err)
		}

		return systemEventMsg{events: []components.ChatLine{
			components.ChannelList{Channels: channels},
		}}
	}
}
