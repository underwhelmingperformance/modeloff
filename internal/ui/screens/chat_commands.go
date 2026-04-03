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
	raw := "/" + msg.Name
	if msg.Args != "" {
		raw += " " + msg.Args
	}

	parsed, err := command.Parse(raw)
	if err != nil {
		return func() tea.Msg { return errorEvent(err) }
	}

	switch cmd := parsed.(type) {
	case command.JoinCommand:
		return s.joinChannel(cmd.Channel)

	case command.LeaveCommand:
		if s.active == "" {
			return noChannelMsg
		}

		return s.leaveChannel()

	case command.NickCommand:
		return s.changeNick(domain.Nick(cmd.Nick))

	case command.TitleCommand:
		if s.active == "" {
			return noChannelMsg
		}

		return s.setTitle(cmd.Title)

	case command.WhoisCommand:
		return s.whois(domain.Nick(cmd.Nick))

	case command.ListCommand:
		return s.listChannels()

	case command.InviteCommand:
		if s.active == "" {
			return noChannelMsg
		}

		if cmd.Model == "" {
			return func() tea.Msg {
				return systemEventMsg{events: []components.ChatLine{
					components.UsageHint{Command: "invite"},
				}}
			}
		}

		return s.inviteModel(domain.ModelID(cmd.Model), cmd.Persona)

	case command.KickCommand:
		if s.active == "" {
			return noChannelMsg
		}

		return s.kickModel(domain.Nick(cmd.Nick))

	case command.ConfigCommand:
		return s.configure(cmd)

	case command.MsgCommand:
		return s.directMessage(domain.Nick(cmd.Nick), cmd.Body)

	case command.HelpCommand:
		return s.showHelp()

	case command.QuitCommand:
		return tea.Quit

	default:
		return func() tea.Msg {
			return errorEvent(domain.UnknownCommandError{Name: msg.Name})
		}
	}
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
			if strings.TrimSpace(cmd.Value) == "" {
				return systemEventMsg{events: []components.ChatLine{
					components.UsageHint{Command: "config api-key"},
				}}
			}

			if _, err := s.sess.SetAPIKey(ctx, strings.TrimSpace(cmd.Value)); err != nil {
				return errorEvent(err)
			}

			return systemEventMsg{events: []components.ChatLine{
				components.APIKeySaved{},
			}}

		case "poke-interval":
			if strings.TrimSpace(cmd.Value) == "" {
				return systemEventMsg{events: []components.ChatLine{
					components.UsageHint{Command: "config poke-interval"},
				}}
			}

			interval, err := time.ParseDuration(strings.TrimSpace(cmd.Value))
			if err != nil {
				return errorEvent(domain.InvalidDurationError{
					Input: cmd.Value,
					Err:   err,
				})
			}

			if _, err := s.sess.SetPokeInterval(ctx, interval); err != nil {
				return errorEvent(err)
			}

			return systemEventMsg{events: []components.ChatLine{
				components.PokeIntervalSet{Interval: interval},
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
		messages, _ := s.sess.Messages(ctx, ch.Name)

		var events []components.ChatLine
		if created {
			events = []components.ChatLine{components.DMOpened{Nick: nick}}
		}

		return commandResultMsg{
			channels: channels,
			active:   ch.Name,
			title:    "",
			messages: messages,
			unread:   s.unreadCounts(ctx, channels),
			members:  s.channelMembers(ch.Name),
			events:   events,
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
		messages, _ := s.sess.Messages(ctx, s.active)

		return commandResultMsg{
			channels: channels,
			active:   s.active,
			title:    s.title,
			messages: messages,
			unread:   s.unreadCounts(ctx, channels),
			members:  s.channelMembers(s.active),
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
		active := domain.ChannelName(name)
		messages, _ := s.sess.Messages(ctx, active)

		var title string
		if ch, err := s.sess.GetChannel(ctx, active); err == nil {
			title = ch.Title
		}

		return commandResultMsg{
			channels: channels,
			active:   active,
			title:    title,
			messages: messages,
			unread:   s.unreadCounts(ctx, channels),
			members:  s.channelMembers(active),
			events:   []components.ChatLine{components.Join{JoinEvent: evt}},
		}
	}
}

func (s *ChatScreen) leaveChannel() tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		evt, _ := s.sess.Leave(ctx, s.active)

		channels, _ := s.sess.ListChannels(ctx)

		var active domain.ChannelName
		var title string
		var messages []domain.Message

		if len(channels) > 0 {
			active = channels[0].Name
			title = channels[0].Title
			messages, _ = s.sess.Messages(ctx, active)
		}

		return commandResultMsg{
			channels: channels,
			active:   active,
			title:    title,
			messages: messages,
			unread:   s.unreadCounts(ctx, channels),
			members:  s.channelMembers(active),
			events:   []components.ChatLine{components.Part{PartEvent: evt}},
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
		messages, _ := s.sess.Messages(ctx, s.active)

		return commandResultMsg{
			channels: channels,
			active:   s.active,
			title:    s.title,
			messages: messages,
			unread:   s.unreadCounts(ctx, channels),
			members:  s.channelMembers(s.active),
			events:   []components.ChatLine{components.NickChange{NickChangeEvent: evt}},
		}
	}
}

func (s *ChatScreen) setTitle(title string) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		evt, err := s.sess.SetTitle(ctx, s.active, title)
		if err != nil {
			return errorEvent(err)
		}

		channels, _ := s.sess.ListChannels(ctx)
		messages, _ := s.sess.Messages(ctx, s.active)

		return commandResultMsg{
			channels: channels,
			active:   s.active,
			title:    title,
			messages: messages,
			unread:   s.unreadCounts(ctx, channels),
			members:  s.channelMembers(s.active),
			events:   []components.ChatLine{components.TopicChange{TopicChangeEvent: evt}},
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
		messages, _ := s.sess.Messages(ctx, s.active)

		return commandResultMsg{
			channels: channels,
			active:   s.active,
			title:    s.title,
			messages: messages,
			unread:   s.unreadCounts(ctx, channels),
			members:  s.channelMembers(s.active),
			events:   []components.ChatLine{components.ModelInvited{ModelInvitedEvent: evt}},
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
		messages, _ := s.sess.Messages(ctx, s.active)

		return commandResultMsg{
			channels: channels,
			active:   s.active,
			title:    s.title,
			messages: messages,
			unread:   s.unreadCounts(ctx, channels),
			members:  s.channelMembers(s.active),
			events:   []components.ChatLine{components.ModelKicked{ModelKickedEvent: evt}},
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
