// Package command parses and represents IRC-style slash commands.
package command

import (
	"fmt"
	"strings"

	"github.com/laney/modeloff/internal/domain"
)

// Command is the interface implemented by all parsed commands.
type Command interface {
	commandMarker()
}

// JoinCommand represents `/join <channel>`.
type JoinCommand struct {
	Channel string `arg:"channel" help:"Channel to join or create"`
}

func (JoinCommand) commandMarker() {}

// LeaveCommand represents `/leave`.
type LeaveCommand struct{}

func (LeaveCommand) commandMarker() {}

// ListCommand represents `/list`.
type ListCommand struct{}

func (ListCommand) commandMarker() {}

// InviteCommand represents `/invite [model] [--persona text]`. Model
// may be empty, indicating the user should be prompted to pick one.
type InviteCommand struct {
	Model   string `optional:"" help:"Model to invite"`
	Persona string `optional:"" help:"Optional persona"`
}

func (InviteCommand) commandMarker() {}

// KickCommand represents `/kick <nick>`.
type KickCommand struct {
	Nick string `help:"Nick to kick"`
}

func (KickCommand) commandMarker() {}

// MsgCommand represents `/msg <nick> [message]`.
type MsgCommand struct {
	Nick string   `help:"Nick to message"`
	Body []string `nargs:"1" help:"Message text"`
}

func (MsgCommand) commandMarker() {}

// NickCommand represents `/nick <new_nick>`.
type NickCommand struct {
	Nick string `arg:"new-nick" help:"New nickname"`
}

func (NickCommand) commandMarker() {}

// TopicCommand represents `/topic [text]`. An empty topic clears it.
type TopicCommand struct {
	Topic []string `optional:"" help:"Topic text"`
}

func (TopicCommand) commandMarker() {}

// WhoisCommand represents `/whois <nick>`.
type WhoisCommand struct {
	Nick string `help:"Nick to look up"`
}

func (WhoisCommand) commandMarker() {}

// HelpCommand represents `/help`.
type HelpCommand struct{}

func (HelpCommand) commandMarker() {}

// QuitCommand represents `/quit`.
type QuitCommand struct{}

func (QuitCommand) commandMarker() {}

// ConfigCommand represents `/config`.
type ConfigCommand struct {
	Key   string   `optional:"" help:"Configuration key"`
	Value []string `optional:"" help:"Configuration value"`
}

func (ConfigCommand) commandMarker() {}

// Parse takes a raw input string starting with `/` and returns the
// corresponding Command, or an error if the input is not a valid
// command.
func Parse(input string) (Command, error) {
	input = strings.TrimSpace(input)

	if input == "" || input[0] != '/' {
		return nil, fmt.Errorf("not a command: %q", input)
	}

	fields := strings.Fields(input)
	name := fields[0]
	args := fields[1:]

	switch name {
	case "/join":
		return parseJoin(args)
	case "/leave":
		return LeaveCommand{}, nil
	case "/list":
		return ListCommand{}, nil
	case "/invite":
		return parseInvite(args), nil
	case "/kick":
		return parseKick(args)
	case "/msg":
		return parseMsg(args)
	case "/nick":
		return parseNick(args)
	case "/topic":
		return parseTopic(input), nil
	case "/whois":
		return parseWhois(args)
	case "/config":
		return parseConfig(args), nil
	case "/help":
		return HelpCommand{}, nil
	case "/quit":
		return QuitCommand{}, nil
	default:
		return nil, fmt.Errorf("unknown command: %s", name)
	}
}

func parseJoin(args []string) (Command, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("/join requires a channel name")
	}

	channel := args[0]
	if !strings.HasPrefix(channel, domain.ChannelPrefix) {
		channel = domain.ChannelPrefix + channel
	}

	return JoinCommand{Channel: channel}, nil
}

func parseInvite(args []string) Command {
	if len(args) == 0 {
		return InviteCommand{}
	}

	cmd := InviteCommand{Model: args[0]}

	for i := 1; i < len(args); i++ {
		if args[i] != "--persona" {
			continue
		}

		if i+1 < len(args) {
			cmd.Persona = strings.Join(args[i+1:], " ")
		}

		break
	}

	return cmd
}

func parseKick(args []string) (Command, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("/kick requires a nick")
	}

	return KickCommand{Nick: args[0]}, nil
}

func parseMsg(args []string) (Command, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("/msg requires a nick")
	}

	cmd := MsgCommand{Nick: args[0]}

	if len(args) > 1 {
		cmd.Body = args[1:]
	}

	return cmd, nil
}

func parseNick(args []string) (Command, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("/nick requires a new nickname")
	}

	return NickCommand{Nick: args[0]}, nil
}

func parseTopic(input string) Command {
	rest := strings.TrimSpace(strings.TrimPrefix(input, "/topic"))

	if rest == "" {
		return TopicCommand{}
	}

	return TopicCommand{Topic: strings.Fields(rest)}
}

func parseWhois(args []string) (Command, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("/whois requires a nick")
	}

	return WhoisCommand{Nick: args[0]}, nil
}

func parseConfig(args []string) Command {
	if len(args) == 0 {
		return ConfigCommand{}
	}

	cmd := ConfigCommand{
		Key: args[0],
	}

	if len(args) > 1 {
		cmd.Value = args[1:]
	}

	return cmd
}
