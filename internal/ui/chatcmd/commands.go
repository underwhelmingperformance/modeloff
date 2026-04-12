package chatcmd

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/session"
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
		if err := c.executeJoin(rc.Ctx, rc.Session, rc.Nick); err != nil {
			return errorEvent("join", err)
		}

		return domain.ChannelFocusEvent{Channel: domain.ChannelName(c.Channel.String())}
	}
}

func (c JoinCommand) executeJoin(ctx context.Context, sess *session.Session, actor domain.Nick) error {
	return sess.JoinAs(ctx, actor, domain.ChannelName(c.Channel.String()))
}

// RunTool implements ToolCommand.
func (c JoinCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	if err := c.executeJoin(ctx, tc.Session, tc.Actor); err != nil {
		return session.ToolResultPayload{OK: false, Error: err.Error()}
	}

	return session.ToolResultPayload{
		OK:      true,
		Summary: "joined " + c.Channel.String(),
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

	return func() tea.Msg {
		if err := c.executePart(rc.Ctx, rc.Session, rc.Nick, rc.Active); err != nil {
			return errorEvent("part", err)
		}

		return nil
	}
}

func (c PartCommand) executePart(ctx context.Context, sess *session.Session, actor domain.Nick, ch domain.ChannelName) error {
	return sess.PartAs(ctx, actor, ch, strings.TrimSpace(strings.Join(c.Message, " ")))
}

// RunTool implements ToolCommand.
func (c PartCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	if tc.Channel == "" {
		return session.ToolResultPayload{OK: false, Error: "no active channel"}
	}

	if err := c.executePart(ctx, tc.Session, tc.Actor, tc.Channel); err != nil {
		return session.ToolResultPayload{OK: false, Error: err.Error()}
	}

	return session.ToolResultPayload{
		OK:      true,
		Summary: "parted " + string(tc.Channel),
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

// RunTool implements ToolCommand.
func (ListCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	channels, err := tc.Session.ListChannels(ctx)
	if err != nil {
		return session.ToolResultPayload{OK: false, Error: err.Error()}
	}

	return session.ToolResultPayload{
		OK:      true,
		Summary: "listed known channels",
		Data:    channels,
	}
}

// AddModelCommand represents `/add-model [model] [--persona text]`.
type AddModelCommand struct {
	Model         string   `arg:"" optional:"" help:"Model to invite"`
	Persona       []string `optional:"" help:"Optional persona"`
	modelSource   command.SuggestionSource
	personaSource command.SuggestionSource
}

// Sources implements command.Completer.
func (c AddModelCommand) Sources() map[string]command.SuggestionSource {
	return map[string]command.SuggestionSource{
		"model":   c.modelSource,
		"persona": c.personaSource,
	}
}

// Run implements Command.
func (c AddModelCommand) Run(rc Context) tea.Cmd {
	if rc.Active == "" {
		return noChannelCmd()
	}

	if c.Model == "" {
		return usageCmd("add-model", "/add-model <model-id> [--persona <text>]")
	}

	return func() tea.Msg {
		if err := rc.Session.AddModel(rc.Ctx, rc.Active, domain.ModelID(c.Model), strings.Join(c.Persona, " ")); err != nil {
			return errorEvent("add-model", err)
		}

		return nil
	}
}

// InviteCommand represents `/invite <nick> [channel]`.
type InviteCommand struct {
	Nick       string     `arg:"" optional:"" help:"Nick to invite"`
	Channel    ChannelArg `arg:"channel" optional:"" help:"Channel to invite them to"`
	nickSource command.SuggestionSource
}

// Sources implements command.Completer.
func (c InviteCommand) Sources() map[string]command.SuggestionSource {
	return map[string]command.SuggestionSource{"nick": c.nickSource}
}

// Run implements Command.
func (c InviteCommand) Run(rc Context) tea.Cmd {
	if rc.Active == "" && c.Channel == "" {
		return noChannelCmd()
	}

	if strings.TrimSpace(c.Nick) == "" {
		return usageCmd("invite", "/invite <nick> [channel]")
	}

	return func() tea.Msg {
		if err := c.executeInvite(rc.Ctx, rc.Session, rc.Nick, rc.Active); err != nil {
			return errorEvent("invite", err)
		}

		return nil
	}
}

func (c InviteCommand) executeInvite(
	ctx context.Context,
	sess *session.Session,
	actor domain.Nick,
	active domain.ChannelName,
) error {
	ch := active
	if c.Channel != "" {
		ch = domain.ChannelName(c.Channel.String())
	}

	return sess.InviteAs(ctx, actor, domain.Nick(c.Nick), ch)
}

// RunTool implements ToolCommand.
func (c InviteCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	if tc.Channel == "" && c.Channel == "" {
		return session.ToolResultPayload{OK: false, Error: "no active channel"}
	}

	if strings.TrimSpace(c.Nick) == "" {
		return session.ToolResultPayload{OK: false, Error: "target nick is required"}
	}

	if err := c.executeInvite(ctx, tc.Session, tc.Actor, tc.Channel); err != nil {
		return session.ToolResultPayload{OK: false, Error: err.Error()}
	}

	ch := tc.Channel
	if c.Channel != "" {
		ch = domain.ChannelName(c.Channel.String())
	}

	return session.ToolResultPayload{
		OK:      true,
		Summary: "invited " + c.Nick + " to " + string(ch),
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
		if err := c.executeKick(rc.Ctx, rc.Session, rc.Nick, rc.Active); err != nil {
			return errorEvent("kick", err)
		}

		return nil
	}
}

func (c KickCommand) executeKick(ctx context.Context, sess *session.Session, actor domain.Nick, ch domain.ChannelName) error {
	return sess.KickAs(ctx, actor, domain.Nick(c.Nick), ch)
}

// RunTool implements ToolCommand.
func (c KickCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	if tc.Channel == "" {
		return session.ToolResultPayload{OK: false, Error: "no active channel"}
	}

	if err := c.executeKick(ctx, tc.Session, tc.Actor, tc.Channel); err != nil {
		return session.ToolResultPayload{OK: false, Error: err.Error()}
	}

	return session.ToolResultPayload{
		OK:      true,
		Summary: "kicked " + c.Nick + " from " + string(tc.Channel),
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

		ch, created, err := c.executeOpenDM(rc.Ctx, rc.Session, rc.Nick)
		if err != nil {
			return errorEvent("msg", domain.UnknownNickError{Nick: nick})
		}

		if err := c.sendDMBody(rc.Ctx, rc.Session, rc.Nick, ch.Name); err != nil {
			return errorEvent("msg", err)
		}

		return domain.DMOpenedEvent{
			Channel: ch,
			Nick:    nick,
			Created: created,
			At:      time.Now(),
		}
	}
}

func (c MsgCommand) executeOpenDM(ctx context.Context, sess *session.Session, actor domain.Nick) (domain.Channel, bool, error) {
	return sess.OpenDMAs(ctx, actor, domain.Nick(c.Nick))
}

func (c MsgCommand) sendDMBody(ctx context.Context, sess *session.Session, actor domain.Nick, ch domain.ChannelName) error {
	body := strings.TrimSpace(strings.Join(c.Body, " "))
	if body == "" {
		return nil
	}

	return sess.SendMessageAs(ctx, actor, ch, body)
}

// RunTool implements ToolCommand.
func (c MsgCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	ch, created, err := c.executeOpenDM(ctx, tc.Session, tc.Actor)
	if err != nil {
		return session.ToolResultPayload{OK: false, Error: err.Error()}
	}

	if err := c.sendDMBody(ctx, tc.Session, tc.Actor, ch.Name); err != nil {
		return session.ToolResultPayload{OK: false, Error: err.Error()}
	}

	summary := "opened direct message with " + c.Nick
	if created {
		summary = "created direct message with " + c.Nick
	}

	return session.ToolResultPayload{
		OK:      true,
		Summary: summary,
		Data:    ch,
	}
}

// NickCommand represents `/nick <new_nick>`.
type NickCommand struct {
	Nick string `arg:"new-nick" help:"New nickname"`
}

// Run implements Command.
func (c NickCommand) Run(rc Context) tea.Cmd {
	return func() tea.Msg {
		nick := domain.Nick(c.Nick)

		if _, err := rc.updateConfig(func(cfg *config.Config) {
			cfg.UserNick = string(nick)
		}); err != nil {
			return errorEvent("nick", err)
		}

		if err := rc.Session.ChangeNick(rc.Ctx, nick); err != nil {
			return errorEvent("nick", err)
		}

		return nil
	}
}

// RunTool implements ToolCommand.
func (c NickCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	nick := domain.Nick(c.Nick)

	if err := tc.Session.ChangeNickAs(ctx, tc.Actor, nick); err != nil {
		return session.ToolResultPayload{OK: false, Error: err.Error()}
	}

	return session.ToolResultPayload{
		OK:      true,
		Summary: "changed nick to " + string(nick),
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
		if err := c.executeSetTopic(rc.Ctx, rc.Session, rc.Nick, rc.Active); err != nil {
			return errorEvent("topic", err)
		}

		return nil
	}
}

func (c TopicCommand) executeSetTopic(ctx context.Context, sess *session.Session, actor domain.Nick, ch domain.ChannelName) error {
	return sess.SetTopicAs(ctx, actor, ch, strings.Join(c.Topic, " "))
}

// RunTool implements ToolCommand.
func (c TopicCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	if tc.Channel == "" {
		return session.ToolResultPayload{OK: false, Error: "no active channel"}
	}

	if len(c.Topic) == 0 {
		ch, err := tc.Session.GetChannel(ctx, tc.Channel)
		if err != nil {
			return session.ToolResultPayload{OK: false, Error: err.Error()}
		}

		return session.ToolResultPayload{
			OK:      true,
			Summary: "returned current topic",
			Data:    ch,
		}
	}

	if err := c.executeSetTopic(ctx, tc.Session, tc.Actor, tc.Channel); err != nil {
		return session.ToolResultPayload{OK: false, Error: err.Error()}
	}

	return session.ToolResultPayload{
		OK:      true,
		Summary: "updated topic for " + string(tc.Channel),
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
		if err := c.executeAction(rc.Ctx, rc.Session, rc.Nick, rc.Active); err != nil {
			return errorEvent("me", err)
		}

		return nil
	}
}

func (c MeCommand) executeAction(ctx context.Context, sess *session.Session, actor domain.Nick, ch domain.ChannelName) error {
	return sess.SendActionAs(ctx, actor, ch, strings.TrimSpace(strings.Join(c.Action, " ")))
}

// RunTool implements ToolCommand.
func (c MeCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	if tc.Channel == "" {
		return session.ToolResultPayload{OK: false, Error: "no active channel"}
	}

	if err := c.executeAction(ctx, tc.Session, tc.Actor, tc.Channel); err != nil {
		return session.ToolResultPayload{OK: false, Error: err.Error()}
	}

	return session.ToolResultPayload{
		OK:      true,
		Summary: "sent action to " + string(tc.Channel),
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

// RunTool implements ToolCommand.
func (c WhoisCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	inst, err := tc.Session.Whois(ctx, domain.Nick(c.Nick))
	if err != nil {
		return session.ToolResultPayload{OK: false, Error: err.Error()}
	}

	return session.ToolResultPayload{
		OK:      true,
		Summary: "returned details for " + c.Nick,
		Data:    inst,
	}
}

// HelpCommand represents `/help`.
type HelpCommand struct{}

// Run implements Command.
func (HelpCommand) Run(_ Context) tea.Cmd {
	return func() tea.Msg { return HelpResult{} }
}

// RunTool implements ToolCommand.
func (HelpCommand) RunTool(_ context.Context, _ session.ToolContext) session.ToolResultPayload {
	return session.ToolResultPayload{
		OK:      true,
		Summary: "available command tools include join, part, list, invite, kick, msg, nick, topic, me, whois, help, and quit",
	}
}

// ClearCommand represents `/clear`.
type ClearCommand struct{}

// Run implements Command.
func (ClearCommand) Run(_ Context) tea.Cmd {
	return func() tea.Msg { return ClearResult{} }
}

// QuitCommand represents `/quit [message]`.
type QuitCommand struct {
	Message []string `arg:"" optional:"" nargs:"1" help:"Optional farewell message"`
}

// Run implements Command.
func (c QuitCommand) Run(rc Context) tea.Cmd {
	return func() tea.Msg {
		if err := c.executeQuit(rc.Ctx, rc.Session, rc.Nick); err != nil {
			return errorEvent("quit", err)
		}

		return tea.QuitMsg{}
	}
}

func (c QuitCommand) executeQuit(ctx context.Context, sess *session.Session, actor domain.Nick) error {
	return sess.QuitAs(ctx, actor, strings.TrimSpace(strings.Join(c.Message, " ")))
}

// RunTool implements ToolCommand.
func (c QuitCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	if err := c.executeQuit(ctx, tc.Session, tc.Actor); err != nil {
		return session.ToolResultPayload{OK: false, Error: err.Error()}
	}

	return session.ToolResultPayload{
		OK:      true,
		Summary: "shut down and left all channels",
	}
}

// PersonasCommand represents `/personas`.
type PersonasCommand struct{}

// Run implements Command.
func (PersonasCommand) Run(rc Context) tea.Cmd {
	return func() tea.Msg {
		personas, err := rc.Session.ListPersonas(rc.Ctx)
		if err != nil {
			return errorEvent("personas", err)
		}

		return PersonasListResult{Personas: personas}
	}
}

// RegeneratePersonasCommand represents `/regenerate-personas`.
type RegeneratePersonasCommand struct{}

// Run implements Command.
func (RegeneratePersonasCommand) Run(rc Context) tea.Cmd {
	return func() tea.Msg {
		personas, err := rc.Session.RegeneratePersonas(rc.Ctx)
		if err != nil {
			return errorEvent("regenerate-personas", err)
		}

		return PersonasRegeneratedResult{Count: len(personas)}
	}
}

// ConfigCommand is a group node whose children are the individual
// config keys. Each subcommand has its own args and Run method.
type ConfigCommand struct {
	Reset           bool                  `optional:"" help:"Reset the selected setting to its default"`
	APIKey          APIKeyConfig          `cmd:"" name:"api-key" help:"Activate OpenRouter immediately."`
	BaseURL         BaseURLConfig         `cmd:"" name:"base-url" help:"Set the API base URL."`
	PokeInterval    PokeIntervalConfig    `cmd:"" name:"poke-interval" help:"Set the background poke cadence."`
	SmallModel      SmallModelConfig      `cmd:"" name:"small-model" help:"Set the model used for lightweight tasks."`
	EmbeddingModel  EmbeddingModelConfig  `cmd:"" name:"embedding-model" help:"Set the embedding model."`
	Highlight       HighlightConfig       `cmd:"" help:"Set words that trigger visual highlighting."`
	TimestampFormat TimestampFormatConfig `cmd:"" name:"timestamp-format" help:"Set or disable timestamp formatting."`
	Persona         PersonaConfig         `cmd:"" help:"Define a custom persona."`
}

// APIKeyConfig represents `/config api-key <value>`.
type APIKeyConfig struct {
	Value string `arg:"" optional:"" help:"OpenRouter API key"`
}

// Run implements Command.
func (c APIKeyConfig) Run(rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			if err := rc.Session.SetAPIKey(rc.Ctx, "", config.DefaultBaseURL); err != nil {
				return errorEvent("config api-key", err)
			}

			if _, err := rc.updateConfig(func(cfg *config.Config) {
				cfg.APIKey = ""
			}); err != nil {
				return errorEvent("config api-key", err)
			}

			return APIKeySetResult{Reset: true}
		}
	}

	if strings.TrimSpace(c.Value) == "" {
		return usageCmd("config", "/config api-key <value>")
	}

	return func() tea.Msg {
		cfg, err := rc.Config.Load(rc.Ctx)
		if err != nil {
			return errorEvent("config api-key", err)
		}

		if err := rc.Session.SetAPIKey(rc.Ctx, c.Value, cfg.BaseURL); err != nil {
			return errorEvent("config api-key", err)
		}

		if _, err := rc.updateConfig(func(cfg *config.Config) {
			cfg.APIKey = c.Value
		}); err != nil {
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
			if err := rc.Session.SetBaseURL(rc.Ctx, config.DefaultBaseURL); err != nil {
				return errorEvent("config base-url", err)
			}

			if _, err := rc.updateConfig(func(cfg *config.Config) {
				cfg.BaseURL = config.DefaultBaseURL
			}); err != nil {
				return errorEvent("config base-url", err)
			}

			return BaseURLSetResult{URL: config.DefaultBaseURL, Reset: true}
		}
	}

	if strings.TrimSpace(c.URL) == "" {
		return usageCmd("config", "/config base-url <url>")
	}

	return func() tea.Msg {
		if err := rc.Session.SetBaseURL(rc.Ctx, c.URL); err != nil {
			return errorEvent("config base-url", err)
		}

		if _, err := rc.updateConfig(func(cfg *config.Config) {
			cfg.BaseURL = c.URL
		}); err != nil {
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
			if _, err := rc.updateConfig(func(cfg *config.Config) {
				cfg.PokeInterval = config.DefaultPokeInterval
			}); err != nil {
				return errorEvent("config poke-interval", err)
			}

			return PokeIntervalSetResult{Interval: config.DefaultPokeInterval, Reset: true}
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

		if _, err := rc.updateConfig(func(cfg *config.Config) {
			cfg.PokeInterval = interval
		}); err != nil {
			return errorEvent("config poke-interval", err)
		}

		return PokeIntervalSetResult{Interval: interval}
	}
}

// SmallModelConfig represents `/config small-model <model-id>`.
type SmallModelConfig struct {
	ModelID string `arg:"" optional:"" help:"Model ID for lightweight tasks"`
}

// Run implements Command.
func (c SmallModelConfig) Run(rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			rc.Session.SetSmallModel(rc.Ctx, config.DefaultSmallModel)

			if _, err := rc.updateConfig(func(cfg *config.Config) {
				cfg.SmallModel = config.DefaultSmallModel
			}); err != nil {
				return errorEvent("config small-model", err)
			}

			return SmallModelSetResult{ModelID: config.DefaultSmallModel, Reset: true}
		}
	}

	if strings.TrimSpace(c.ModelID) == "" {
		return usageCmd("config", "/config small-model <model-id>")
	}

	return func() tea.Msg {
		modelID := domain.ModelID(c.ModelID)
		rc.Session.SetSmallModel(rc.Ctx, modelID)

		if _, err := rc.updateConfig(func(cfg *config.Config) {
			cfg.SmallModel = modelID
		}); err != nil {
			return errorEvent("config small-model", err)
		}

		return SmallModelSetResult{ModelID: modelID}
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
			if _, err := rc.updateConfig(func(cfg *config.Config) {
				cfg.EmbeddingModel = config.DefaultEmbeddingModel
			}); err != nil {
				return errorEvent("config embedding-model", err)
			}

			return EmbeddingModelSetResult{ModelID: config.DefaultEmbeddingModel, Reset: true}
		}
	}

	if strings.TrimSpace(c.ModelID) == "" {
		return usageCmd("config", "/config embedding-model <model-id>")
	}

	return func() tea.Msg {
		modelID := domain.ModelID(c.ModelID)

		if _, err := rc.updateConfig(func(cfg *config.Config) {
			cfg.EmbeddingModel = modelID
		}); err != nil {
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
			words := append([]string(nil), config.DefaultHighlightWords...)

			if _, err := rc.updateConfig(func(cfg *config.Config) {
				cfg.HighlightWords = words
			}); err != nil {
				return errorEvent("config highlight", err)
			}

			return HighlightWordsSetResult{Words: words, Reset: true}
		}
	}

	if len(c.Words) == 0 {
		return usageCmd("config", "/config highlight <word> [<word>...]")
	}

	return func() tea.Msg {
		if _, err := rc.updateConfig(func(cfg *config.Config) {
			cfg.HighlightWords = c.Words
		}); err != nil {
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
			cfg, err := rc.updateConfig(func(cfg *config.Config) {
				cfg.TimestampFormat = nil
			})
			if err != nil {
				return errorEvent("config timestamp-format", err)
			}

			return TimestampFormatSetResult{Format: cfg.TimestampFormat, Reset: true}
		}
	}

	return func() tea.Msg {
		format := normaliseTimestampFormat(c.Format)

		if _, err := rc.updateConfig(func(cfg *config.Config) {
			cfg.TimestampFormat = format
		}); err != nil {
			return errorEvent("config timestamp-format", err)
		}

		return TimestampFormatSetResult{Format: format}
	}
}

// PersonaConfig represents `/config persona <id> <description...>`.
type PersonaConfig struct {
	ID          string   `arg:"" optional:"" help:"Persona identifier"`
	Description []string `arg:"" optional:"" help:"Persona description"`
}

// Run implements Command.
func (c PersonaConfig) Run(rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			count, err := rc.Session.ResetPersonas(rc.Ctx)
			if err != nil {
				return errorEvent("config persona", err)
			}

			return PersonaResetResult{Count: count}
		}
	}

	if strings.TrimSpace(c.ID) == "" {
		return usageCmd("config persona", "/config persona <id> <description...>")
	}

	desc := strings.TrimSpace(strings.Join(c.Description, " "))
	if desc == "" {
		return usageCmd("config persona", "/config persona <id> <description...>")
	}

	return func() tea.Msg {
		if err := rc.Session.SetPersona(rc.Ctx, c.ID, desc); err != nil {
			return errorEvent("config persona", err)
		}

		return PersonaSetResult{ID: c.ID}
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
