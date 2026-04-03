package screens

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/components"
)

func (s *ChatScreen) handleCommand(msg components.CommandSubmitMsg) tea.Cmd {
	raw := "/" + msg.Name
	if msg.Args != "" {
		raw += " " + msg.Args
	}

	parsed, err := command.Parse(raw)
	if err != nil {
		return func() tea.Msg {
			return systemEventMsg{kind: components.EventError, lines: []string{err.Error()}}
		}
	}

	switch cmd := parsed.(type) {
	case command.JoinCommand:
		return s.joinChannel(cmd.Channel)

	case command.LeaveCommand:
		return s.leaveChannel()

	case command.NickCommand:
		return s.changeNick(domain.Nick(cmd.Nick))

	case command.TitleCommand:
		return s.setTitle(cmd.Title)

	case command.WhoisCommand:
		return s.whois(domain.Nick(cmd.Nick))

	case command.ListCommand:
		return s.listChannels()

	case command.InviteCommand:
		if cmd.Model == "" {
			return func() tea.Msg {
				return systemEventMsg{kind: components.EventWarning, lines: []string{"usage: /invite <model-id> [--persona <text>]"}}
			}
		}

		return s.inviteModel(domain.ModelID(cmd.Model), cmd.Persona)

	case command.KickCommand:
		return s.kickModel(domain.Nick(cmd.Nick))

	case command.ConfigCommand:
		return s.configure(cmd)

	case command.MsgCommand:
		return s.directMessage(domain.Nick(cmd.Nick), cmd.Body)

	case command.HelpCommand:
		return s.showHelp()

	default:
		return func() tea.Msg {
			return systemEventMsg{kind: components.EventError, lines: []string{
				fmt.Sprintf("unknown command: /%s", msg.Name),
			}}
		}
	}
}

func (s *ChatScreen) configure(cmd command.ConfigCommand) tea.Cmd {
	return func() tea.Msg {
		const usage = "usage: /config api-key <value> | /config poke-interval <duration>"

		ctx := s.ctx

		switch cmd.Key {
		case "":
			return systemEventMsg{kind: components.EventWarning, lines: []string{usage}}

		case "api-key":
			if strings.TrimSpace(cmd.Value) == "" {
				return systemEventMsg{kind: components.EventWarning, lines: []string{"usage: /config api-key <value>"}}
			}

			if _, err := s.sess.SetAPIKey(ctx, strings.TrimSpace(cmd.Value)); err != nil {
				return systemEventMsg{kind: components.EventError, lines: []string{err.Error()}}
			}

			return systemEventMsg{kind: components.EventSuccess, lines: []string{
				"OpenRouter API key saved. Restart modeloff to use it.",
			}}

		case "poke-interval":
			if strings.TrimSpace(cmd.Value) == "" {
				return systemEventMsg{kind: components.EventWarning, lines: []string{"usage: /config poke-interval <duration>"}}
			}

			interval, err := time.ParseDuration(strings.TrimSpace(cmd.Value))
			if err != nil {
				return systemEventMsg{kind: components.EventError, lines: []string{
					fmt.Sprintf("invalid duration %q: %v", cmd.Value, err),
				}}
			}

			if _, err := s.sess.SetPokeInterval(ctx, interval); err != nil {
				return systemEventMsg{kind: components.EventError, lines: []string{err.Error()}}
			}

			return systemEventMsg{kind: components.EventSuccess, lines: []string{
				fmt.Sprintf("Poke interval set to %s.", interval),
			}}

		default:
			return systemEventMsg{kind: components.EventError, lines: []string{
				fmt.Sprintf("unknown config key: %s", cmd.Key),
				usage,
			}}
		}
	}
}

func (s *ChatScreen) directMessage(nick domain.Nick, body string) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		ch, created, err := s.sess.OpenDM(ctx, nick)
		if err != nil {
			return systemEventMsg{kind: components.EventError, lines: []string{
				fmt.Sprintf("no such nick: %s", nick),
			}}
		}

		if strings.TrimSpace(body) != "" {
			if _, err := s.sess.SendMessage(ctx, ch.Name, body); err != nil {
				return systemEventMsg{kind: components.EventError, lines: []string{err.Error()}}
			}
		}

		channels, _ := s.sess.ListChannels(ctx)
		messages, _ := s.sess.Messages(ctx, ch.Name)

		var systemEvents []string
		if created {
			systemEvents = []string{
				fmt.Sprintf("Opened direct message with %s", nick),
			}
		}

		return commandResultMsg{
			channels:     channels,
			active:       ch.Name,
			title:        "",
			messages:     messages,
			eventKind:    components.EventSuccess,
			systemEvents: systemEvents,
		}
	}
}

func (s *ChatScreen) handlePoke() tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		if err := s.sess.Poke(ctx); err != nil {
			return systemEventMsg{kind: components.EventError, lines: []string{err.Error()}}
		}

		channels, _ := s.sess.ListChannels(ctx)
		messages, _ := s.sess.Messages(ctx, s.active)

		return commandResultMsg{
			channels: channels,
			active:   s.active,
			title:    s.title,
			messages: messages,
		}
	}
}

func (s *ChatScreen) joinChannel(name string) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		evt, err := s.sess.Join(ctx, name)
		if err != nil {
			return systemEventMsg{kind: components.EventError, lines: []string{err.Error()}}
		}

		channels, _ := s.sess.ListChannels(ctx)
		active := domain.ChannelName(name)
		messages, _ := s.sess.Messages(ctx, active)

		var title string
		if ch, err := s.sess.GetChannel(ctx, active); err == nil {
			title = ch.Title
		}

		event := fmt.Sprintf("Switched to %s", active)
		if evt.Created {
			event = fmt.Sprintf("Created channel %s", active)
		}

		return commandResultMsg{
			channels:     channels,
			active:       active,
			title:        title,
			messages:     messages,
			eventKind:    components.EventSuccess,
			systemEvents: []string{event},
		}
	}
}

func (s *ChatScreen) leaveChannel() tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		_, _ = s.sess.Leave(ctx, s.active)

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
		}
	}
}

func (s *ChatScreen) changeNick(nick domain.Nick) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		evt, err := s.sess.ChangeNick(ctx, nick)
		if err != nil {
			return systemEventMsg{kind: components.EventError, lines: []string{err.Error()}}
		}

		channels, _ := s.sess.ListChannels(ctx)
		messages, _ := s.sess.Messages(ctx, s.active)

		return commandResultMsg{
			channels:  channels,
			active:    s.active,
			title:     s.title,
			messages:  messages,
			eventKind: components.EventSuccess,
			systemEvents: []string{
				fmt.Sprintf("%s is now known as %s", evt.OldNick, evt.NewNick),
			},
		}
	}
}

func (s *ChatScreen) setTitle(title string) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		_, err := s.sess.SetTitle(ctx, s.active, title)
		if err != nil {
			return systemEventMsg{kind: components.EventError, lines: []string{err.Error()}}
		}

		channels, _ := s.sess.ListChannels(ctx)
		messages, _ := s.sess.Messages(ctx, s.active)

		event := fmt.Sprintf("topic for %s set to: %s", s.active, title)
		if title == "" {
			event = fmt.Sprintf("topic for %s cleared", s.active)
		}

		return commandResultMsg{
			channels:     channels,
			active:       s.active,
			title:        title,
			messages:     messages,
			eventKind:    components.EventSuccess,
			systemEvents: []string{event},
		}
	}
}

func (s *ChatScreen) whois(nick domain.Nick) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		inst, err := s.sess.Whois(ctx, nick)
		if err != nil {
			return systemEventMsg{kind: components.EventError, lines: []string{
				fmt.Sprintf("no such nick: %s", nick),
			}}
		}

		lines := []string{
			fmt.Sprintf("%s is %s", inst.Nick, inst.ModelID),
		}

		if inst.Persona != "" {
			lines = append(lines, fmt.Sprintf("  persona: %s", inst.Persona))
		}

		if len(inst.Channels) > 0 {
			var chStrs []string
			for ch := range inst.Channels.Sorted() {
				chStrs = append(chStrs, string(ch))
			}

			lines = append(lines, fmt.Sprintf("  channels: %s", strings.Join(chStrs, ", ")))
		}

		return systemEventMsg{kind: components.EventInfo, lines: lines}
	}
}

func (s *ChatScreen) inviteModel(modelID domain.ModelID, persona string) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		evt, err := s.sess.Invite(ctx, s.active, modelID, persona)
		if err != nil {
			return systemEventMsg{kind: components.EventError, lines: []string{err.Error()}}
		}

		channels, _ := s.sess.ListChannels(ctx)
		messages, _ := s.sess.Messages(ctx, s.active)

		event := fmt.Sprintf("%s (%s) has joined %s", evt.Instance.Nick, evt.Instance.ModelID, evt.Channel)
		if evt.Instance.Persona != "" {
			event = fmt.Sprintf("%s with persona %q", event, evt.Instance.Persona)
		}

		return commandResultMsg{
			channels:     channels,
			active:       s.active,
			title:        s.title,
			messages:     messages,
			eventKind:    components.EventSuccess,
			systemEvents: []string{event},
		}
	}
}

func (s *ChatScreen) kickModel(nick domain.Nick) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		evt, err := s.sess.Kick(ctx, s.active, nick)
		if err != nil {
			return systemEventMsg{kind: components.EventError, lines: []string{err.Error()}}
		}

		channels, _ := s.sess.ListChannels(ctx)
		messages, _ := s.sess.Messages(ctx, s.active)

		return commandResultMsg{
			channels:  channels,
			active:    s.active,
			title:     s.title,
			messages:  messages,
			eventKind: components.EventSuccess,
			systemEvents: []string{
				fmt.Sprintf("%s has been kicked from %s", evt.Nick, evt.Channel),
			},
		}
	}
}

func (s *ChatScreen) showHelp() tea.Cmd {
	return func() tea.Msg {
		return systemEventMsg{kind: components.EventInfo, lines: []string{
			"/join <channel>                   Join or create a channel",
			"/leave                            Leave the current channel",
			"/list                             List all channels",
			"/invite <model> [--persona text]  Invite a model to the channel",
			"/kick <nick>                      Remove a model from the channel",
			"/msg <nick> [message]             Open a direct message",
			"/nick <name>                      Change your nickname",
			"/title [text]                     Set or clear the channel title",
			"/whois <nick>                     Show info about a model",
			"/config api-key <key>             Set the OpenRouter API key",
			"/config poke-interval <duration>  Set the poke interval",
			"/help                             Show this help",
		}}
	}
}

func (s *ChatScreen) listChannels() tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		channels, err := s.sess.ListChannels(ctx)
		if err != nil {
			return systemEventMsg{kind: components.EventError, lines: []string{err.Error()}}
		}

		if len(channels) == 0 {
			return systemEventMsg{kind: components.EventInfo, lines: []string{"no channels"}}
		}

		lines := make([]string, len(channels))
		for i, ch := range channels {
			line := string(ch.Name)
			if ch.Title != "" {
				line += " — " + ch.Title
			}

			lines[i] = line
		}

		return systemEventMsg{kind: components.EventInfo, lines: lines}
	}
}
