package screens

import (
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
		Part   command.PartCommand   `cmd:"" help:"Part from the current channel."`
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

	s.commands = cmds

	return s.commands
}

func (s *ChatScreen) runContext() command.RunContext {
	return command.RunContext{
		Ctx:     s.ctx,
		Session: s.sess,
		Active:  s.active,
		Nick:    s.sess.UserNick(),
	}
}

func errorEvent(operation string, err error) domain.ErrorEvent {
	return domain.ErrorEvent{Operation: operation, Err: err, At: time.Now()}
}

func (s *ChatScreen) handleCommand(msg components.CommandSubmitMsg) tea.Cmd {
	runner, err := s.Commands().Parse(msg.Raw)
	if err != nil {
		return func() tea.Msg { return errorEvent("command", err) }
	}

	return runner.Run(s.runContext())
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
