package screens

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/components"
)

func noChannelMsg() tea.Msg {
	return systemEventMsg{events: []components.ChatLine{components.NoChannel{}}}
}

func errorEvent(operation string, err error) domain.ErrorEvent {
	return domain.ErrorEvent{Operation: operation, Err: err, At: time.Now()}
}

func (s *ChatScreen) handleCommand(msg components.CommandSubmitMsg) tea.Cmd {
	cmd, err := command.Execute(s.Commands(), msg.Raw)
	if err != nil {
		return func() tea.Msg { return errorEvent("command", err) }
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
				return errorEvent("config api-key", err)
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
				return errorEvent("config poke-interval", domain.InvalidDurationError{
					Input: value,
					Err:   err,
				})
			}

			if _, err := s.sess.SetPokeInterval(ctx, interval); err != nil {
				return errorEvent("config poke-interval", err)
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
				return errorEvent("config nick-model", err)
			}

			return systemEventMsg{events: []components.ChatLine{
				components.NickModelSet{ModelID: modelID},
			}}

		default:
			return errorEvent("config", domain.UnknownConfigKeyError{Key: cmd.Key})
		}
	}
}

func (s *ChatScreen) directMessage(nick domain.Nick, body string) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		ch, created, err := s.sess.OpenDM(ctx, nick)
		if err != nil {
			return errorEvent("msg", domain.UnknownNickError{Nick: nick})
		}

		if strings.TrimSpace(body) != "" {
			if _, _, err := s.sess.SendMessage(ctx, ch.Name, body); err != nil {
				return errorEvent("msg", err)
			}
		}

		return domain.DMOpenedEvent{
			Channel: ch,
			Nick:    nick,
			Created: created,
			At:      time.Now(),
		}
	}
}

func (s *ChatScreen) handlePoke() tea.Cmd {
	return func() tea.Msg {
		events, err := s.sess.Poke(s.ctx)
		if err != nil {
			return errorEvent("poke", err)
		}

		if len(events) == 0 {
			return components.PendingResponseMsg{Pending: false}
		}

		return eventBatchMsg{events: events}
	}
}

func (s *ChatScreen) joinChannel(name string) tea.Cmd {
	return func() tea.Msg {
		evt, err := s.sess.Join(s.ctx, name)
		if err != nil {
			return errorEvent("join", err)
		}

		return evt
	}
}

func (s *ChatScreen) leaveChannel() tea.Cmd {
	return func() tea.Msg {
		evt, err := s.sess.Leave(s.ctx, s.active)
		if err != nil {
			return errorEvent("leave", err)
		}

		return evt
	}
}

func (s *ChatScreen) changeNick(nick domain.Nick) tea.Cmd {
	return func() tea.Msg {
		evt, err := s.sess.ChangeNick(s.ctx, nick)
		if err != nil {
			return errorEvent("nick", err)
		}

		return evt
	}
}

func (s *ChatScreen) setTopic(topic string) tea.Cmd {
	return func() tea.Msg {
		evt, err := s.sess.SetTopic(s.ctx, s.active, topic)
		if err != nil {
			return errorEvent("topic", err)
		}

		return evt
	}
}

func (s *ChatScreen) whois(nick domain.Nick) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		inst, err := s.sess.Whois(ctx, nick)
		if err != nil {
			return errorEvent("whois", domain.UnknownNickError{Nick: nick})
		}

		return systemEventMsg{events: []components.ChatLine{
			components.Whois{ModelInstance: inst},
		}}
	}
}

func (s *ChatScreen) inviteModel(modelID domain.ModelID, persona string) tea.Cmd {
	return func() tea.Msg {
		evt, err := s.sess.Invite(s.ctx, s.active, modelID, persona)
		if err != nil {
			return errorEvent("invite", err)
		}

		return evt
	}
}

func (s *ChatScreen) kickModel(nick domain.Nick) tea.Cmd {
	return func() tea.Msg {
		evt, err := s.sess.Kick(s.ctx, s.active, nick)
		if err != nil {
			return errorEvent("kick", err)
		}

		return evt
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
			return errorEvent("list", err)
		}

		return systemEventMsg{events: []components.ChatLine{
			components.ChannelList{Channels: channels},
		}}
	}
}
