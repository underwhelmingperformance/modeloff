package chatcmd

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
)

// Grammar defines the complete set of chat screen commands.
type Grammar struct {
	Join   JoinCommand   `cmd:"" help:"Switch to a channel or create it if needed."`
	Part   PartCommand   `cmd:"" help:"Part from the current channel."`
	List   ListCommand   `cmd:"" help:"List all known channels."`
	Invite InviteCommand `cmd:"" help:"Invite a model or reusable instance into the current channel."`
	Kick   KickCommand   `cmd:"" help:"Remove a model instance from the current channel."`
	Msg    MsgCommand    `cmd:"" help:"Open a direct message and optionally send text."`
	Nick   NickCommand   `cmd:"" help:"Change your nickname."`
	Topic  TopicCommand  `cmd:"" help:"Set or clear the current channel topic."`
	Me     MeCommand     `cmd:"" help:"Send an action message (e.g. /me waves)."`
	Whois  WhoisCommand  `cmd:"" help:"Show details about a model instance."`
	Config ConfigCommand `cmd:"" help:"Update runtime configuration."`
	Help   HelpCommand   `cmd:"" help:"Show available commands."`
	Quit   QuitCommand   `cmd:"" help:"Exit modeloff."`
}

// Sources carries snapshot data for command completion. Each field
// holds the current value at the time the parser is built.
type Sources struct {
	Channels      []domain.Channel
	Instances     []domain.ModelInstance
	ActiveChannel domain.ChannelName
	ActiveMembers []domain.Nick
	UserNick      domain.Nick
	LiveModels    []ModelOption
}

// BuildParser creates a typed Parser from a snapshot of the current
// application state. It should be rebuilt whenever the completion-
// relevant state changes (channels, instances, active channel, etc.).
func BuildParser(src Sources) Parser {
	instancesSource := InstancesSource(src.Instances)

	grammar := &Grammar{
		Join: JoinCommand{
			channelSource: ChannelsSource(src.Channels),
		},
		Invite: InviteCommand{
			modelSource: command.ComposeSources(
				ReusableInstancesSource(src.Instances, src.ActiveChannel),
				LiveModelsSource(src.LiveModels),
			),
		},
		Kick: KickCommand{
			nickSource: ActiveMembersSource(src.ActiveMembers, src.UserNick),
		},
		Msg: MsgCommand{
			nickSource: instancesSource,
		},
		Whois: WhoisCommand{
			nickSource: instancesSource,
		},
		Config: ConfigCommand{
			keySource: command.LiteralSource(
				command.Suggestion{Value: "api-key", Label: "api-key", Detail: "Activate OpenRouter immediately."},
				command.Suggestion{Value: "nick-model", Label: "nick-model", Detail: "Set the model used to generate nicknames."},
				command.Suggestion{Value: "poke-interval", Label: "poke-interval", Detail: "Set the background poke cadence."},
				command.Suggestion{Value: "highlight", Label: "highlight", Detail: "Set words that trigger visual highlighting."},
			),
			valueSource: func(state command.InvocationState) []command.Suggestion {
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

	return command.BuildParser[Context, tea.Cmd](grammar)
}
