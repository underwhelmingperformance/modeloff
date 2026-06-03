package chatcmd

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
)

// Grammar defines the complete set of chat screen commands.
type Grammar struct {
	Join               JoinCommand               `cmd:"" aliases:"j" tool:"" help:"Switch to a channel or create it if needed."`
	Part               PartCommand               `cmd:"" tool:"Leave the current channel for an extended absence — a real exit, not a brief away. The message is the parting line peers see; an empty message parts silently. Do NOT part for short absences (brb, afk, food, sleep): just say so in chat and stay. Parting drops you from the channel; rejoining requires a fresh JOIN or an invite. Reserve PART for when you genuinely intend to leave the room." help:"Part from the current channel."`
	List               ListCommand               `cmd:"" tool:"" help:"List all known channels."`
	AddModel           AddModelCommand           `cmd:"" kind:"channel" caps:"operator" tool:"Add a new model instance to the current channel by model ID, optionally with a persona." help:"Add a model or reusable instance into the current channel."`
	Invite             InviteCommand             `cmd:"" tool:"" kind:"channel" help:"Invite a nick to a channel."`
	Kick               KickCommand               `cmd:"" tool:"" kind:"channel" help:"Remove a nick from the current channel."`
	Kill               KillCommand               `cmd:"" caps:"operator" tool:"Forcibly disconnect a model instance from the server with a reason." help:"Disconnect a model instance from the server."`
	Msg                MsgCommand                `cmd:"" tool:"Send a message addressed to either a #channel you are in, or a user (by nick). The recipient sees the message and may reply." help:"Send a message to a #channel or to a user by nick."`
	Query              QueryCommand              `cmd:"" help:"Open (or focus) a direct-message window with a nick. Optional trailing body is sent as the first message."`
	Nick               NickCommand               `cmd:"" tool:"" help:"Change your nickname."`
	Topic              TopicCommand              `cmd:"" tool:"" kind:"channel" help:"Set or clear the current channel topic."`
	Mode               ModeCommand               `cmd:"" kind:"channel" tool:"Set or clear one or more channel modes. Syntax: <modes> [args]. Examples: +o nick, +tn, -i+l 10, +k secret, +ov-i alice bob." help:"Set or clear channel modes."`
	Me                 MeCommand                 `cmd:"" tool:"" help:"Send an action message (e.g. /me waves)."`
	Whois              WhoisCommand              `cmd:"" tool:"" help:"Show details about a model instance."`
	Config             ConfigCommand             `cmd:"" help:"Update runtime configuration."`
	Personas           PersonasCommand           `cmd:"" help:"List all defined personas."`
	RegeneratePersonas RegeneratePersonasCommand `cmd:"" name:"regenerate-personas" help:"Regenerate AI-created personas."`
	Help               HelpCommand               `cmd:"" aliases:"?" tool:"" help:"Show available commands."`
	Clear              ClearCommand              `cmd:"" help:"Clear the current window."`
	Poke               PokeCommand               `cmd:"" help:"Poke idle channels now to prompt model activity."`
	Quit               QuitCommand               `cmd:"" aliases:"q" tool:"Shut down your instance and leave all channels." help:"Exit modeloff."`
	Pass               PassCommand               `tool:"Explicitly record that you have nothing to say this turn, with a brief reason. Silence is the default — you only need to call this if you want the reason captured for observability. Do not call this in the same turn as a msg or me tool."`
}

// NewParser builds the command parser. The grammar is static; all
// completion data flows through CompletionContext at suggestion time.
func NewParser() (Parser, error) {
	return command.BuildParser[CompletionContext, Context, tea.Cmd](&Grammar{})
}
