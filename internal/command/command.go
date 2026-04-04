// Package command parses and represents IRC-style slash commands.
package command

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/laney/modeloff/internal/domain"
)

// ChannelArg is a command-layer wrapper around domain.ChannelName
// that implements FieldDecoder to ensure the # prefix is present.
type ChannelArg string

// Decode implements FieldDecoder.
func (c *ChannelArg) Decode(raw string) error {
	if !strings.HasPrefix(raw, domain.ChannelPrefix) {
		raw = domain.ChannelPrefix + raw
	}

	*c = ChannelArg(raw)
	return nil
}

// String returns the channel name as a plain string.
func (c ChannelArg) String() string { return string(c) }

// JoinCommand represents `/join <channel>`.
type JoinCommand struct {
	Channel ChannelArg `arg:"channel" help:"Channel to join or create"`
}

// LeaveCommand represents `/leave`.
type LeaveCommand struct{}

// ListCommand represents `/list`.
type ListCommand struct{}

// InviteCommand represents `/invite [model] [--persona text]`. Model
// may be empty, indicating the user should be prompted to pick one.
type InviteCommand struct {
	Model   string   `arg:"" optional:"" help:"Model to invite"`
	Persona []string `optional:"" help:"Optional persona"`
}

// KickCommand represents `/kick <nick>`.
type KickCommand struct {
	Nick string `arg:"" help:"Nick to kick"`
}

// MsgCommand represents `/msg <nick> [message]`.
type MsgCommand struct {
	Nick string   `arg:"" help:"Nick to message"`
	Body []string `arg:"" optional:"" nargs:"1" help:"Message text"`
}

// NickCommand represents `/nick <new_nick>`.
type NickCommand struct {
	Nick string `arg:"new-nick" help:"New nickname"`
}

// TopicCommand represents `/topic [text]`. An empty topic clears it.
type TopicCommand struct {
	Topic []string `arg:"" optional:"" help:"Topic text"`
}

// WhoisCommand represents `/whois <nick>`.
type WhoisCommand struct {
	Nick string `arg:"" help:"Nick to look up"`
}

// HelpCommand represents `/help`.
type HelpCommand struct{}

// QuitCommand represents `/quit`.
type QuitCommand struct{}

// ConfigCommand represents `/config`.
type ConfigCommand struct {
	Key   string   `arg:"" optional:"" help:"Configuration key"`
	Value []string `arg:"" optional:"" help:"Configuration value"`
}

// Build reflects over a grammar struct and produces a Set. Each
// field tagged with `cmd:""` becomes a command node. Name derives
// from the field name (kebab-cased) or from a `name:""` tag. Help
// comes from the `help:""` tag. The grammar must be a pointer to a
// struct. Screens define their own grammar structs locally and call
// Build to produce their portion of the command tree.
func Build(grammar any) Set {
	nodes, err := build(grammar)
	if err != nil {
		panic(fmt.Sprintf("building command set: %v", err))
	}

	return Set{Commands: nodes}
}

// Parse tokenises a raw slash-command string, resolves the matching
// node in the set, and populates a command struct from the arguments.
// The returned Invocation can be Run() to execute the handler.
func (s Set) Parse(input string) (*Invocation, error) {
	input = strings.TrimSpace(input)

	if input == "" || input[0] != '/' {
		return nil, fmt.Errorf("not a command: %q", input)
	}

	fields := strings.Fields(input)
	name := strings.TrimPrefix(fields[0], "/")
	args := fields[1:]

	node := s.Find(name)
	if node == nil {
		return nil, fmt.Errorf("unknown command: /%s", name)
	}

	if node.factory == nil {
		return nil, fmt.Errorf("command /%s has no factory", name)
	}

	cmd := node.factory()

	if err := ParseInto(cmd, args); err != nil {
		return nil, err
	}

	// Dereference so callers get value types (e.g. JoinCommand, not
	// *JoinCommand), matching the convention used throughout the UI.
	parsed := reflect.ValueOf(cmd).Elem().Interface()

	return &Invocation{
		Raw:    input,
		Name:   name,
		Args:   args,
		parsed: parsed,
		node:   node,
	}, nil
}
