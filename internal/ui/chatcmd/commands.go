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
		if err := rc.Session.Join(rc.Ctx, c.Channel.String()); err != nil {
			return errorEvent("join", err)
		}

		return nil
	}
}

// PartCommand represents `/part [message]`.
type PartCommand struct {
	Message []string `arg:"" optional:"" nargs:"1" help:"Optional farewell message"`
}

// Run implements Command.
func (c PartCommand) Run(rc Context) tea.Cmd {
	if rc.Active == "" {
		return noChannelCmd()
	}

	msg := strings.TrimSpace(strings.Join(c.Message, " "))

	return func() tea.Msg {
		if err := rc.Session.Part(rc.Ctx, rc.Active, msg); err != nil {
			return errorEvent("part", err)
		}

		return nil
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
		return usageCmd("invite", "/invite <model-id> [--persona <text>]")
	}

	return func() tea.Msg {
		if err := rc.Session.Invite(rc.Ctx, rc.Active, domain.ModelID(c.Model), strings.Join(c.Persona, " ")); err != nil {
			return errorEvent("invite", err)
		}

		return nil
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
		if err := rc.Session.Kick(rc.Ctx, rc.Active, domain.Nick(c.Nick)); err != nil {
			return errorEvent("kick", err)
		}

		return nil
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
			if err := rc.Session.SendMessage(rc.Ctx, ch.Name, body); err != nil {
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
		if err := rc.Session.ChangeNick(rc.Ctx, domain.Nick(c.Nick)); err != nil {
			return errorEvent("nick", err)
		}

		return nil
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

	if len(c.Topic) == 0 {
		return func() tea.Msg {
			ch, err := rc.Session.GetChannel(rc.Ctx, rc.Active)
			if err != nil {
				return errorEvent("topic", err)
			}

			return TopicInfoResult{Channel: ch}
		}
	}

	return func() tea.Msg {
		if err := rc.Session.SetTopic(rc.Ctx, rc.Active, strings.Join(c.Topic, " ")); err != nil {
			return errorEvent("topic", err)
		}

		return nil
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
		return usageCmd("me", "/me <action>")
	}

	return func() tea.Msg {
		if err := rc.Session.SendAction(rc.Ctx, rc.Active, body); err != nil {
			return errorEvent("me", err)
		}

		return nil
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

// QuitCommand represents `/quit [message]`.
type QuitCommand struct {
	Message []string `arg:"" optional:"" nargs:"1" help:"Optional farewell message"`
}

// Run implements Command.
func (c QuitCommand) Run(rc Context) tea.Cmd {
	msg := strings.TrimSpace(strings.Join(c.Message, " "))

	return func() tea.Msg {
		if err := rc.Session.Quit(rc.Ctx, msg); err != nil {
			return errorEvent("quit", err)
		}

		return tea.QuitMsg{}
	}
}

// ConfigCommand is a group node whose children are the individual
// config keys. Each subcommand has its own args and Run method.
type ConfigCommand struct {
	Reset           bool                  `optional:"" help:"Reset the selected setting to its default"`
	APIKey          APIKeyConfig          `cmd:"" name:"api-key" help:"Activate OpenRouter immediately."`
	BaseURL         BaseURLConfig         `cmd:"" name:"base-url" help:"Set the API base URL."`
	PokeInterval    PokeIntervalConfig    `cmd:"" name:"poke-interval" help:"Set the background poke cadence."`
	NickModel       NickModelConfig       `cmd:"" name:"nick-model" help:"Set the model used to generate nicknames."`
	EmbeddingModel  EmbeddingModelConfig  `cmd:"" name:"embedding-model" help:"Set the embedding model."`
	Highlight       HighlightConfig       `cmd:"" help:"Set words that trigger visual highlighting."`
	TimestampFormat TimestampFormatConfig `cmd:"" name:"timestamp-format" help:"Set or disable timestamp formatting."`
}

// APIKeyConfig represents `/config api-key <value>`.
type APIKeyConfig struct {
	Value string `arg:"" optional:"" help:"OpenRouter API key"`
}

// Run implements Command.
func (c APIKeyConfig) Run(rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			if _, err := rc.Session.ResetAPIKey(rc.Ctx); err != nil {
				return errorEvent("config api-key", err)
			}

			return APIKeySetResult{Reset: true}
		}
	}

	if strings.TrimSpace(c.Value) == "" {
		return usageCmd("config", "/config api-key <value>")
	}

	return func() tea.Msg {
		if _, err := rc.Session.SetAPIKey(rc.Ctx, c.Value); err != nil {
			return errorEvent("config api-key", err)
		}

		return APIKeySetResult{}
	}
}

// BaseURLConfig represents `/config base-url <url>`.
type BaseURLConfig struct {
	URL string `arg:"" optional:"" help:"API base URL"`
}

// Run implements Command.
func (c BaseURLConfig) Run(rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			cfg, err := rc.Session.ResetBaseURL(rc.Ctx)
			if err != nil {
				return errorEvent("config base-url", err)
			}

			return BaseURLSetResult{URL: cfg.BaseURL, Reset: true}
		}
	}

	if strings.TrimSpace(c.URL) == "" {
		return usageCmd("config", "/config base-url <url>")
	}

	return func() tea.Msg {
		if _, err := rc.Session.SetBaseURL(rc.Ctx, c.URL); err != nil {
			return errorEvent("config base-url", err)
		}

		return BaseURLSetResult{URL: c.URL}
	}
}

// PokeIntervalConfig represents `/config poke-interval <duration>`.
type PokeIntervalConfig struct {
	Duration string `arg:"" optional:"" help:"Poke interval (e.g. 5m, 1h)"`
}

// Sources implements command.Completer.
func (PokeIntervalConfig) Sources() map[string]command.SuggestionSource {
	return map[string]command.SuggestionSource{
		"duration": command.LiteralSource(
			command.Suggestion{Value: "5m", Label: "5m", Detail: "Fast poke cadence"},
			command.Suggestion{Value: "10m", Label: "10m", Detail: "Balanced poke cadence"},
			command.Suggestion{Value: "30m", Label: "30m", Detail: "Quiet channels"},
			command.Suggestion{Value: "1h", Label: "1h", Detail: "Very low activity"},
		),
	}
}

// Run implements Command.
func (c PokeIntervalConfig) Run(rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			cfg, err := rc.Session.ResetPokeInterval(rc.Ctx)
			if err != nil {
				return errorEvent("config poke-interval", err)
			}

			return PokeIntervalSetResult{Interval: cfg.PokeInterval, Reset: true}
		}
	}

	if strings.TrimSpace(c.Duration) == "" {
		return usageCmd("config", "/config poke-interval <duration>")
	}

	return func() tea.Msg {
		interval, err := time.ParseDuration(c.Duration)
		if err != nil {
			return errorEvent("config poke-interval", domain.InvalidDurationError{
				Input: c.Duration,
				Err:   err,
			})
		}

		if _, err := rc.Session.SetPokeInterval(rc.Ctx, interval); err != nil {
			return errorEvent("config poke-interval", err)
		}

		return PokeIntervalSetResult{Interval: interval}
	}
}

// NickModelConfig represents `/config nick-model <model-id>`.
type NickModelConfig struct {
	ModelID string `arg:"" optional:"" help:"Model ID for nick generation"`
}

// Run implements Command.
func (c NickModelConfig) Run(rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			cfg, err := rc.Session.ResetNickModel(rc.Ctx)
			if err != nil {
				return errorEvent("config nick-model", err)
			}

			return NickModelSetResult{ModelID: cfg.NickModel, Reset: true}
		}
	}

	if strings.TrimSpace(c.ModelID) == "" {
		return usageCmd("config", "/config nick-model <model-id>")
	}

	return func() tea.Msg {
		modelID := domain.ModelID(c.ModelID)
		if _, err := rc.Session.SetNickModel(rc.Ctx, modelID); err != nil {
			return errorEvent("config nick-model", err)
		}

		return NickModelSetResult{ModelID: modelID}
	}
}

// EmbeddingModelConfig represents `/config embedding-model <model-id>`.
type EmbeddingModelConfig struct {
	ModelID string `arg:"" optional:"" help:"Model ID for embeddings"`
}

// Run implements Command.
func (c EmbeddingModelConfig) Run(rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			cfg, err := rc.Session.ResetEmbeddingModel(rc.Ctx)
			if err != nil {
				return errorEvent("config embedding-model", err)
			}

			return EmbeddingModelSetResult{ModelID: cfg.EmbeddingModel, Reset: true}
		}
	}

	if strings.TrimSpace(c.ModelID) == "" {
		return usageCmd("config", "/config embedding-model <model-id>")
	}

	return func() tea.Msg {
		modelID := domain.ModelID(c.ModelID)
		if _, err := rc.Session.SetEmbeddingModel(rc.Ctx, modelID); err != nil {
			return errorEvent("config embedding-model", err)
		}

		return EmbeddingModelSetResult{ModelID: modelID}
	}
}

// HighlightConfig represents `/config highlight <word> [<word>...]`.
type HighlightConfig struct {
	Words []string `arg:"" optional:"" help:"Words to highlight"`
}

// Run implements Command.
func (c HighlightConfig) Run(rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			cfg, err := rc.Session.ResetHighlightWords(rc.Ctx)
			if err != nil {
				return errorEvent("config highlight", err)
			}

			return HighlightWordsSetResult{Words: cfg.HighlightWords, Reset: true}
		}
	}

	if len(c.Words) == 0 {
		return usageCmd("config", "/config highlight <word> [<word>...]")
	}

	return func() tea.Msg {
		if _, err := rc.Session.SetHighlightWords(rc.Ctx, c.Words); err != nil {
			return errorEvent("config highlight", err)
		}

		return HighlightWordsSetResult{Words: c.Words}
	}
}

// TimestampFormatConfig represents `/config timestamp-format [<format>...]`.
type TimestampFormatConfig struct {
	Format []string `arg:"" optional:"" help:"Timestamp format"`
}

// Run implements Command.
func (c TimestampFormatConfig) Run(rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			cfg, err := rc.Session.ResetTimestampFormat(rc.Ctx)
			if err != nil {
				return errorEvent("config timestamp-format", err)
			}

			return TimestampFormatSetResult{Format: cfg.TimestampFormat, Reset: true}
		}
	}

	return func() tea.Msg {
		format := normaliseTimestampFormat(c.Format)

		if _, err := rc.Session.SetTimestampFormat(rc.Ctx, format); err != nil {
			return errorEvent("config timestamp-format", err)
		}

		return TimestampFormatSetResult{Format: format}
	}
}

func normaliseTimestampFormat(parts []string) *string {
	if len(parts) == 0 {
		disabled := ""
		return &disabled
	}

	joined := strings.TrimSpace(strings.Join(parts, " "))
	if joined == `""` || joined == `''` {
		disabled := ""
		return &disabled
	}

	return &joined
}
