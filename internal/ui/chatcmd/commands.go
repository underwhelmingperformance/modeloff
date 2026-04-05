package chatcmd

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
)

// ChannelArg is a command-layer wrapper around domain.ChannelName
// that implements FieldDecoder to ensure the # prefix is present.
type ChannelArg string

// Decode implements command.FieldDecoder.
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
	Channel       ChannelArg `arg:"channel" help:"Channel to join or create"`
	channelSource command.SuggestionSource
}

// Sources implements command.Completer.
func (c JoinCommand) Sources() map[string]command.SuggestionSource {
	return map[string]command.SuggestionSource{"channel": c.channelSource}
}

// Run implements Command.
func (c JoinCommand) Run(rc Context) tea.Cmd {
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

// Run implements Command.
func (PartCommand) Run(rc Context) tea.Cmd {
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

// Run implements Command.
func (ListCommand) Run(rc Context) tea.Cmd {
	return func() tea.Msg {
		channels, err := rc.Session.ListChannels(rc.Ctx)
		if err != nil {
			return errorEvent("list", err)
		}

		return ListResult{Channels: channels}
	}
}

// InviteCommand represents `/invite [model] [--persona text]`.
type InviteCommand struct {
	Model       string   `arg:"" optional:"" help:"Model to invite"`
	Persona     []string `optional:"" help:"Optional persona"`
	modelSource command.SuggestionSource
}

// Sources implements command.Completer.
func (c InviteCommand) Sources() map[string]command.SuggestionSource {
	return map[string]command.SuggestionSource{"model": c.modelSource}
}

// Run implements Command.
func (c InviteCommand) Run(rc Context) tea.Cmd {
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
	Nick       string `arg:"" help:"Nick to kick"`
	nickSource command.SuggestionSource
}

// Sources implements command.Completer.
func (c KickCommand) Sources() map[string]command.SuggestionSource {
	return map[string]command.SuggestionSource{"nick": c.nickSource}
}

// Run implements Command.
func (c KickCommand) Run(rc Context) tea.Cmd {
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
	Nick       string   `arg:"" help:"Nick to message"`
	Body       []string `arg:"" optional:"" nargs:"1" help:"Message text"`
	nickSource command.SuggestionSource
}

// Sources implements command.Completer.
func (c MsgCommand) Sources() map[string]command.SuggestionSource {
	return map[string]command.SuggestionSource{"nick": c.nickSource}
}

// Run implements Command.
func (c MsgCommand) Run(rc Context) tea.Cmd {
	return func() tea.Msg {
		nick := domain.Nick(c.Nick)

		ch, created, err := rc.Session.OpenDM(rc.Ctx, nick)
		if err != nil {
			return errorEvent("msg", domain.UnknownNickError{Nick: nick})
		}

		body := strings.TrimSpace(strings.Join(c.Body, " "))
		if body != "" {
			if _, err := rc.Session.SendMessage(rc.Ctx, ch.Name, body); err != nil {
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

// Run implements Command.
func (c NickCommand) Run(rc Context) tea.Cmd {
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

// Run implements Command.
func (c TopicCommand) Run(rc Context) tea.Cmd {
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

// MeCommand represents `/me <action>`.
type MeCommand struct {
	Action []string `arg:"" nargs:"1" help:"Action text"`
}

// Run implements Command.
func (c MeCommand) Run(rc Context) tea.Cmd {
	if rc.Active == "" {
		return noChannelCmd()
	}

	body := strings.TrimSpace(strings.Join(c.Action, " "))
	if body == "" {
		return usageCmd("me")
	}

	return func() tea.Msg {
		evt, err := rc.Session.SendAction(rc.Ctx, rc.Active, body)
		if err != nil {
			return errorEvent("me", err)
		}

		return evt
	}
}

// WhoisCommand represents `/whois <nick>`.
type WhoisCommand struct {
	Nick       string `arg:"" help:"Nick to look up"`
	nickSource command.SuggestionSource
}

// Sources implements command.Completer.
func (c WhoisCommand) Sources() map[string]command.SuggestionSource {
	return map[string]command.SuggestionSource{"nick": c.nickSource}
}

// Run implements Command.
func (c WhoisCommand) Run(rc Context) tea.Cmd {
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

// Run implements Command.
func (HelpCommand) Run(_ Context) tea.Cmd {
	return func() tea.Msg { return HelpResult{} }
}

// QuitCommand represents `/quit`.
type QuitCommand struct{}

// Run implements Command.
func (QuitCommand) Run(_ Context) tea.Cmd { return tea.Quit }

// ConfigCommand represents `/config`.
type ConfigCommand struct {
	Key         string   `arg:"" optional:"" help:"Choose a config key."`
	Value       []string `arg:"" optional:"" help:"Values are free-form after the key."`
	keySource   command.SuggestionSource
	valueSource command.SuggestionSource
}

// Sources implements command.Completer.
func (c ConfigCommand) Sources() map[string]command.SuggestionSource {
	return map[string]command.SuggestionSource{
		"key":   c.keySource,
		"value": c.valueSource,
	}
}

// Run implements Command.
func (c ConfigCommand) Run(rc Context) tea.Cmd {
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

		case "highlight":
			if len(c.Value) == 0 {
				return UsageError{Command: "config highlight"}
			}

			if _, err := rc.Session.SetHighlightWords(rc.Ctx, c.Value); err != nil {
				return errorEvent("config highlight", err)
			}

			return HighlightWordsSetResult{Words: c.Value}

		default:
			return errorEvent("config", domain.UnknownConfigKeyError{Key: c.Key})
		}
	}
}
