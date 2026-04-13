package chatcmd

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
)

// Grammar defines the complete set of chat screen commands.
type Grammar struct {
	Join               JoinCommand               `cmd:"" aliases:"j" tool:"" help:"Switch to a channel or create it if needed."`
	Part               PartCommand               `cmd:"" tool:"Leave the current channel with an optional farewell message." help:"Part from the current channel."`
	List               ListCommand               `cmd:"" tool:"" help:"List all known channels."`
	AddModel           AddModelCommand           `cmd:"" kind:"channel" help:"Add a model or reusable instance into the current channel."`
	Invite             InviteCommand             `cmd:"" tool:"" kind:"channel" help:"Invite a nick to a channel."`
	Kick               KickCommand               `cmd:"" tool:"" kind:"channel" help:"Remove a nick from the current channel."`
	Msg                MsgCommand                `cmd:"" tool:"" help:"Open a direct message and optionally send text."`
	Nick               NickCommand               `cmd:"" tool:"" help:"Change your nickname."`
	Topic              TopicCommand              `cmd:"" tool:"" kind:"channel" help:"Set or clear the current channel topic."`
	Me                 MeCommand                 `cmd:"" tool:"" help:"Send an action message (e.g. /me waves)."`
	Whois              WhoisCommand              `cmd:"" tool:"" help:"Show details about a model instance."`
	Config             ConfigCommand             `cmd:"" help:"Update runtime configuration."`
	Personas           PersonasCommand           `cmd:"" help:"List all defined personas."`
	RegeneratePersonas RegeneratePersonasCommand `cmd:"" name:"regenerate-personas" help:"Regenerate AI-created personas."`
	Help               HelpCommand               `cmd:"" tool:"" help:"Show available commands."`
	Clear              ClearCommand              `cmd:"" help:"Clear the current window."`
	Quit               QuitCommand               `cmd:"" aliases:"q" tool:"Shut down your instance and leave all channels." help:"Exit modeloff."`
}

// NewParser builds the command parser. The grammar is static; all
// completion data flows through CompletionContext at suggestion time.
func NewParser() (Parser, error) {
	return command.BuildParser[Context, tea.Cmd](&Grammar{})
}
