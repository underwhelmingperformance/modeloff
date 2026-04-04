// Package command parses and represents IRC-style slash commands.
package command

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

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

// Run implements Runner.
func (c JoinCommand) Run(rc RunContext) tea.Cmd {
	return func() tea.Msg {
		evt, err := rc.Session.Join(rc.Ctx, c.Channel.String())
		if err != nil {
			return errorEvent("join", err)
		}

		return evt
	}
}

// PartCommand represents `/part`.
type PartCommand struct{}

// Run implements Runner.
func (PartCommand) Run(rc RunContext) tea.Cmd {
	if rc.Active == "" {
		return noChannelCmd()
	}

	return func() tea.Msg {
		evt, err := rc.Session.Leave(rc.Ctx, rc.Active)
		if err != nil {
			return errorEvent("part", err)
		}

		return evt
	}
}

// ListCommand represents `/list`.
type ListCommand struct{}

// Run implements Runner.
func (ListCommand) Run(rc RunContext) tea.Cmd {
	return func() tea.Msg {
		channels, err := rc.Session.ListChannels(rc.Ctx)
		if err != nil {
			return errorEvent("list", err)
		}

		return ListResult{Channels: channels}
	}
}

// InviteCommand represents `/invite [model] [--persona text]`. Model
// may be empty, indicating the user should be prompted to pick one.
type InviteCommand struct {
	Model   string   `arg:"" optional:"" help:"Model to invite"`
	Persona []string `optional:"" help:"Optional persona"`
}

// Run implements Runner.
func (c InviteCommand) Run(rc RunContext) tea.Cmd {
	if rc.Active == "" {
		return noChannelCmd()
	}

	if c.Model == "" {
		return usageCmd("invite")
	}

	return func() tea.Msg {
		evt, err := rc.Session.Invite(rc.Ctx, rc.Active, domain.ModelID(c.Model), strings.Join(c.Persona, " "))
		if err != nil {
			return errorEvent("invite", err)
		}

		return evt
	}
}

// KickCommand represents `/kick <nick>`.
type KickCommand struct {
	Nick string `arg:"" help:"Nick to kick"`
}

// Run implements Runner.
func (c KickCommand) Run(rc RunContext) tea.Cmd {
	if rc.Active == "" {
		return noChannelCmd()
	}

	return func() tea.Msg {
		evt, err := rc.Session.Kick(rc.Ctx, rc.Active, domain.Nick(c.Nick))
		if err != nil {
			return errorEvent("kick", err)
		}

		return evt
	}
}

// MsgCommand represents `/msg <nick> [message]`.
type MsgCommand struct {
	Nick string   `arg:"" help:"Nick to message"`
	Body []string `arg:"" optional:"" nargs:"1" help:"Message text"`
}

// Run implements Runner.
func (c MsgCommand) Run(rc RunContext) tea.Cmd {
	return func() tea.Msg {
		nick := domain.Nick(c.Nick)

		ch, created, err := rc.Session.OpenDM(rc.Ctx, nick)
		if err != nil {
			return errorEvent("msg", domain.UnknownNickError{Nick: nick})
		}

		body := strings.TrimSpace(strings.Join(c.Body, " "))
		if body != "" {
			if _, _, err := rc.Session.SendMessage(rc.Ctx, ch.Name, body); err != nil {
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

// NickCommand represents `/nick <new_nick>`.
type NickCommand struct {
	Nick string `arg:"new-nick" help:"New nickname"`
}

// Run implements Runner.
func (c NickCommand) Run(rc RunContext) tea.Cmd {
	return func() tea.Msg {
		evt, err := rc.Session.ChangeNick(rc.Ctx, domain.Nick(c.Nick))
		if err != nil {
			return errorEvent("nick", err)
		}

		return evt
	}
}

// TopicCommand represents `/topic [text]`. An empty topic clears it.
type TopicCommand struct {
	Topic []string `arg:"" optional:"" help:"Topic text"`
}

// Run implements Runner.
func (c TopicCommand) Run(rc RunContext) tea.Cmd {
	if rc.Active == "" {
		return noChannelCmd()
	}

	return func() tea.Msg {
		evt, err := rc.Session.SetTopic(rc.Ctx, rc.Active, strings.Join(c.Topic, " "))
		if err != nil {
			return errorEvent("topic", err)
		}

		return evt
	}
}

// WhoisCommand represents `/whois <nick>`.
type WhoisCommand struct {
	Nick string `arg:"" help:"Nick to look up"`
}

// Run implements Runner.
func (c WhoisCommand) Run(rc RunContext) tea.Cmd {
	return func() tea.Msg {
		inst, err := rc.Session.Whois(rc.Ctx, domain.Nick(c.Nick))
		if err != nil {
			return errorEvent("whois", domain.UnknownNickError{Nick: domain.Nick(c.Nick)})
		}

		return WhoisResult{Instance: inst}
	}
}

// HelpCommand represents `/help`.
type HelpCommand struct{}

// Run implements Runner.
func (HelpCommand) Run(_ RunContext) tea.Cmd {
	return func() tea.Msg { return HelpResult{} }
}

// QuitCommand represents `/quit`.
type QuitCommand struct{}

// Run implements Runner.
func (QuitCommand) Run(_ RunContext) tea.Cmd { return tea.Quit }

// ConfigCommand represents `/config`.
type ConfigCommand struct {
	Key   string   `arg:"" optional:"" help:"Configuration key"`
	Value []string `arg:"" optional:"" help:"Configuration value"`
}

// Run implements Runner.
func (c ConfigCommand) Run(rc RunContext) tea.Cmd {
	return func() tea.Msg {
		switch c.Key {
		case "":
			return UsageError{Command: "config"}

		case "api-key":
			value := strings.TrimSpace(strings.Join(c.Value, " "))
			if value == "" {
				return UsageError{Command: "config api-key"}
			}

			if _, err := rc.Session.SetAPIKey(rc.Ctx, value); err != nil {
				return errorEvent("config api-key", err)
			}

			return APIKeySetResult{}

		case "poke-interval":
			value := strings.TrimSpace(strings.Join(c.Value, " "))
			if value == "" {
				return UsageError{Command: "config poke-interval"}
			}

			interval, err := time.ParseDuration(value)
			if err != nil {
				return errorEvent("config poke-interval", domain.InvalidDurationError{
					Input: value,
					Err:   err,
				})
			}

			if _, err := rc.Session.SetPokeInterval(rc.Ctx, interval); err != nil {
				return errorEvent("config poke-interval", err)
			}

			return PokeIntervalSetResult{Interval: interval}

		case "nick-model":
			value := strings.TrimSpace(strings.Join(c.Value, " "))
			if value == "" {
				return UsageError{Command: "config nick-model"}
			}

			modelID := domain.ModelID(value)
			if _, err := rc.Session.SetNickModel(rc.Ctx, modelID); err != nil {
				return errorEvent("config nick-model", err)
			}

			return NickModelSetResult{ModelID: modelID}

		default:
			return errorEvent("config", domain.UnknownConfigKeyError{Key: c.Key})
		}
	}
}

func errorEvent(operation string, err error) domain.ErrorEvent {
	return domain.ErrorEvent{Operation: operation, Err: err, At: time.Now()}
}

func usageCmd(command string) tea.Cmd {
	return func() tea.Msg { return UsageError{Command: command} }
}

func noChannelCmd() tea.Cmd {
	return func() tea.Msg { return NoChannelError{} }
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
// The returned Runner is the populated command struct ready to execute.
func (s Set) Parse(input string) (Runner, error) {
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

	runner, ok := parsed.(Runner)
	if !ok {
		return nil, fmt.Errorf("command /%s does not implement Runner", name)
	}

	return runner, nil
}
