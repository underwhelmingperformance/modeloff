package screens

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/components"
)

// Commands implements ui.CommandSource.
func (s *ChatScreen) Commands() command.Set {
	if len(s.commands.Commands) > 0 {
		return s.commands
	}

	type grammar struct {
		Join   command.JoinCommand   `cmd:"" help:"Switch to a channel or create it if needed."`
		Leave  command.LeaveCommand  `cmd:"" help:"Leave the current channel."`
		List   command.ListCommand   `cmd:"" help:"List all known channels."`
		Invite command.InviteCommand `cmd:"" help:"Invite a model or reusable instance into the current channel."`
		Kick   command.KickCommand   `cmd:"" help:"Remove a model instance from the current channel."`
		Msg    command.MsgCommand    `cmd:"" help:"Open a direct message and optionally send text."`
		Nick   command.NickCommand   `cmd:"" help:"Change your nickname."`
		Topic  command.TopicCommand  `cmd:"" help:"Set or clear the current channel topic."`
		Whois  command.WhoisCommand  `cmd:"" help:"Show details about a model instance."`
		Config command.ConfigCommand `cmd:"" help:"Update runtime configuration."`
		Help   command.HelpCommand   `cmd:"" help:"Show available commands."`
		Quit   command.QuitCommand   `cmd:"" help:"Exit modeloff."`
	}

	cmds := command.Build(&grammar{})

	// Bind handlers.

	command.Bind(cmds, "join", func(cmd command.JoinCommand) tea.Cmd {
		return s.joinChannel(cmd.Channel.String())
	})

	command.Bind(cmds, "leave", func(_ command.LeaveCommand) tea.Cmd {
		if s.active == "" {
			return s.noChannelCmd()
		}

		return s.leaveChannel()
	})

	command.Bind(cmds, "list", func(_ command.ListCommand) tea.Cmd {
		return s.listChannels()
	})

	command.Bind(cmds, "invite", func(cmd command.InviteCommand) tea.Cmd {
		if s.active == "" {
			return s.noChannelCmd()
		}

		if cmd.Model == "" {
			return s.usageCmd("invite")
		}

		return s.inviteModel(domain.ModelID(cmd.Model), strings.Join(cmd.Persona, " "))
	})

	command.Bind(cmds, "kick", func(cmd command.KickCommand) tea.Cmd {
		if s.active == "" {
			return s.noChannelCmd()
		}

		return s.kickModel(domain.Nick(cmd.Nick))
	})

	command.Bind(cmds, "msg", func(cmd command.MsgCommand) tea.Cmd {
		return s.directMessage(domain.Nick(cmd.Nick), strings.Join(cmd.Body, " "))
	})

	command.Bind(cmds, "nick", func(cmd command.NickCommand) tea.Cmd {
		return s.changeNick(domain.Nick(cmd.Nick))
	})

	command.Bind(cmds, "topic", func(cmd command.TopicCommand) tea.Cmd {
		if s.active == "" {
			return s.noChannelCmd()
		}

		return s.setTopic(strings.Join(cmd.Topic, " "))
	})

	command.Bind(cmds, "whois", func(cmd command.WhoisCommand) tea.Cmd {
		return s.whois(domain.Nick(cmd.Nick))
	})

	command.Bind(cmds, "help", func(_ command.HelpCommand) tea.Cmd {
		return s.showHelp()
	})

	command.Bind(cmds, "quit", func(_ command.QuitCommand) tea.Cmd {
		return tea.Quit
	})

	// Bind suggestion sources.

	cmds.Find("join").SetSource("channel", command.ChannelsSource())

	invite := cmds.Find("invite")
	invite.SetSource("model", command.ComposeSources(
		command.ReusableInstancesSource(),
		command.LiveModelsSource(),
	))

	cmds.Find("kick").SetSource("nick", command.ActiveMembersSource())
	cmds.Find("msg").SetSource("nick", command.InstancesSource())
	cmds.Find("whois").SetSource("nick", command.InstancesSource())

	// Config has custom positionals with dynamic completion sources.
	configNode := cmds.Find("config")
	configNode.Positionals = []command.Positional{
		{
			Name: "key",
			Help: "Choose a config key.",
			Source: command.LiteralSource(
				command.Suggestion{Value: "api-key", Label: "api-key", Detail: "Activate OpenRouter immediately."},
				command.Suggestion{Value: "nick-model", Label: "nick-model", Detail: "Set the model used to generate nicknames."},
				command.Suggestion{Value: "poke-interval", Label: "poke-interval", Detail: "Set the background poke cadence."},
			),
		},
		{
			Name:     "value",
			Help:     "Values are free-form after the key.",
			Optional: true,
			Source: func(_ command.CompletionContext, state command.InvocationState) []command.Suggestion {
				if len(state.Args) == 0 || state.Args[0] != "poke-interval" {
					return nil
				}

				return []command.Suggestion{
					{Value: "5m", Label: "5m", Detail: "Fast poke cadence"},
					{Value: "10m", Label: "10m", Detail: "Balanced poke cadence"},
					{Value: "30m", Label: "30m", Detail: "Quiet channels"},
					{Value: "1h", Label: "1h", Detail: "Very low activity"},
				}
			},
		},
	}
	command.Bind(cmds, "config", func(cmd command.ConfigCommand) tea.Cmd {
		return s.configure(cmd)
	})

	s.commands = cmds

	return s.commands
}

func (s *ChatScreen) usageCmd(commandName string) tea.Cmd {
	return func() tea.Msg {
		return components.AppendLinesMsg{
			Channel: s.active,
			Lines:   []components.ChatLine{components.UsageHint{Command: commandName}},
		}
	}
}

func (s *ChatScreen) noChannelCmd() tea.Cmd {
	return func() tea.Msg {
		return components.AppendLinesMsg{
			Channel: s.active,
			Lines:   []components.ChatLine{components.NoChannel{}},
		}
	}
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
			return components.AppendLinesMsg{
				Channel: s.active,
				Lines:   []components.ChatLine{components.UsageHint{Command: "config"}},
			}

		case "api-key":
			value := strings.TrimSpace(strings.Join(cmd.Value, " "))
			if value == "" {
				return components.AppendLinesMsg{
					Channel: s.active,
					Lines:   []components.ChatLine{components.UsageHint{Command: "config api-key"}},
				}
			}

			if _, err := s.sess.SetAPIKey(ctx, value); err != nil {
				return errorEvent("config api-key", err)
			}

			return apiKeyActivatedMsg{}

		case "poke-interval":
			value := strings.TrimSpace(strings.Join(cmd.Value, " "))
			if value == "" {
				return components.AppendLinesMsg{
					Channel: s.active,
					Lines:   []components.ChatLine{components.UsageHint{Command: "config poke-interval"}},
				}
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

			return components.AppendLinesMsg{
				Channel: s.active,
				Lines:   []components.ChatLine{components.PokeIntervalSet{Interval: interval}},
			}

		case "nick-model":
			value := strings.TrimSpace(strings.Join(cmd.Value, " "))
			if value == "" {
				return components.AppendLinesMsg{
					Channel: s.active,
					Lines:   []components.ChatLine{components.UsageHint{Command: "config nick-model"}},
				}
			}

			modelID := domain.ModelID(value)

			if _, err := s.sess.SetNickModel(ctx, modelID); err != nil {
				return errorEvent("config nick-model", err)
			}

			return components.AppendLinesMsg{
				Channel: s.active,
				Lines:   []components.ChatLine{components.NickModelSet{ModelID: modelID}},
			}

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

		return components.AppendLinesMsg{
			Channel: s.active,
			Lines:   []components.ChatLine{components.Whois{ModelInstance: inst}},
		}
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
		return components.AppendLinesMsg{
			Channel: s.active,
			Lines:   []components.ChatLine{components.Help{}},
		}
	}
}

func (s *ChatScreen) listChannels() tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		channels, err := s.sess.ListChannels(ctx)
		if err != nil {
			return errorEvent("list", err)
		}

		return components.AppendLinesMsg{
			Channel: s.active,
			Lines:   []components.ChatLine{components.ChannelList{Channels: channels}},
		}
	}
}
