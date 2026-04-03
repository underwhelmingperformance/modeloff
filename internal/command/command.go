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
	Channel string
}

func (JoinCommand) commandMarker() {}

// LeaveCommand represents `/leave`.
type LeaveCommand struct{}

func (LeaveCommand) commandMarker() {}

// ListCommand represents `/list`.
type ListCommand struct{}

func (ListCommand) commandMarker() {}

// InviteCommand represents `/invite [model]`. Model may be empty,
// indicating the user should be prompted to pick one.
type InviteCommand struct {
	Model string
}

func (InviteCommand) commandMarker() {}

// KickCommand represents `/kick <nick>`.
type KickCommand struct {
	Nick string
}

func (KickCommand) commandMarker() {}

// MsgCommand represents `/msg <nick> [message]`.
type MsgCommand struct {
	Nick string
	Body string
}

func (MsgCommand) commandMarker() {}

// NickCommand represents `/nick <new_nick>`.
type NickCommand struct {
	Nick string
}

func (NickCommand) commandMarker() {}

// TitleCommand represents `/title [title]`. An empty title clears it.
type TitleCommand struct {
	Title string
}

func (TitleCommand) commandMarker() {}

// WhoisCommand represents `/whois <nick>`.
type WhoisCommand struct {
	Nick string
}

func (WhoisCommand) commandMarker() {}

// ConfigCommand represents `/config`.
type ConfigCommand struct {
	Key   string
	Value string
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
	case "/title":
		return parseTitle(input), nil
	case "/whois":
		return parseWhois(args)
	case "/config":
		return parseConfig(args), nil
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

	return InviteCommand{Model: args[0]}
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
		cmd.Body = strings.Join(args[1:], " ")
	}

	return cmd, nil
}

func parseNick(args []string) (Command, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("/nick requires a new nickname")
	}

	return NickCommand{Nick: args[0]}, nil
}

func parseTitle(input string) Command {
	// Strip the `/title` prefix and trim to get the full title text.
	rest := strings.TrimSpace(strings.TrimPrefix(input, "/title"))

	return TitleCommand{Title: rest}
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
		cmd.Value = strings.Join(args[1:], " ")
	}

	return cmd
}
