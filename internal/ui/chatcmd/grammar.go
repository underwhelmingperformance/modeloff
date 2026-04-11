package chatcmd

import (
	"iter"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
)

// Grammar defines the complete set of chat screen commands.
type Grammar struct {
	Join               JoinCommand               `cmd:"" tool:"" help:"Switch to a channel or create it if needed."`
	Part               PartCommand               `cmd:"" tool:"Leave the current channel with an optional farewell message." help:"Part from the current channel."`
	List               ListCommand               `cmd:"" tool:"" help:"List all known channels."`
	AddModel           AddModelCommand           `cmd:"" help:"Add a model or reusable instance into the current channel."`
	Invite             InviteCommand             `cmd:"" tool:"" help:"Invite a nick to a channel."`
	Kick               KickCommand               `cmd:"" tool:"" help:"Remove a nick from the current channel."`
	Msg                MsgCommand                `cmd:"" tool:"" help:"Open a direct message and optionally send text."`
	Nick               NickCommand               `cmd:"" tool:"" help:"Change your nickname."`
	Topic              TopicCommand              `cmd:"" tool:"" help:"Set or clear the current channel topic."`
	Me                 MeCommand                 `cmd:"" tool:"" help:"Send an action message (e.g. /me waves)."`
	Whois              WhoisCommand              `cmd:"" tool:"" help:"Show details about a model instance."`
	Config             ConfigCommand             `cmd:"" help:"Update runtime configuration."`
	Personas           PersonasCommand           `cmd:"" help:"List all defined personas."`
	RegeneratePersonas RegeneratePersonasCommand `cmd:"" name:"regenerate-personas" help:"Regenerate AI-created personas."`
	Help               HelpCommand               `cmd:"" tool:"" help:"Show available commands."`
	Quit               QuitCommand               `cmd:"" tool:"Shut down your instance and leave all channels." help:"Exit modeloff."`
}

// Sources provides live accessors for command completion data. Each
// field is a function so the grammar can be built once and completion
// always reflects the latest state without rebuilding.
type Sources struct {
	Channels      func() iter.Seq[domain.Channel]
	Instances     func() iter.Seq[domain.Instance]
	ActiveChannel func() domain.ChannelName
	ActiveMembers func() iter.Seq[domain.Nick]
	UserNick      func() domain.Nick
	LiveModels    func() []ModelOption
}

// BuildParser creates a typed Parser from a snapshot of the current
// application state. It should be rebuilt whenever the completion-
// relevant state changes (channels, instances, active channel, etc.).
func BuildParser(src Sources) Parser {
	lazy := func(fn func(command.InvocationState) []command.Suggestion) command.SuggestionSource {
		return fn
	}

	grammar := &Grammar{
		Join: JoinCommand{
			channelSource: lazy(func(s command.InvocationState) []command.Suggestion {
				return ChannelsSource(src.Channels())(s)
			}),
		},
		AddModel: AddModelCommand{
			modelSource: lazy(func(s command.InvocationState) []command.Suggestion {
				return command.ComposeSources(
					ReusableInstancesSource(src.Instances(), src.ActiveChannel()),
					LiveModelsSource(src.LiveModels()),
				)(s)
			}),
		},
		Invite: InviteCommand{
			nickSource: lazy(func(s command.InvocationState) []command.Suggestion {
				return InstancesSource(src.Instances())(s)
			}),
		},
		Kick: KickCommand{
			nickSource: lazy(func(s command.InvocationState) []command.Suggestion {
				return ActiveMembersSource(src.ActiveMembers(), src.UserNick())(s)
			}),
		},
		Msg: MsgCommand{
			nickSource: lazy(func(s command.InvocationState) []command.Suggestion {
				return InstancesSource(src.Instances())(s)
			}),
		},
		Whois: WhoisCommand{
			nickSource: lazy(func(s command.InvocationState) []command.Suggestion {
				return InstancesSource(src.Instances())(s)
			}),
		},
		Config: ConfigCommand{},
	}

	return command.BuildParser[Context, tea.Cmd](grammar)
}
