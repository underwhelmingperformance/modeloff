package chatcmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
)

// ConfigCommand is a group node whose children are the individual
// config keys. Each subcommand has its own args and Run method.
type ConfigCommand struct {
	Reset           bool                  `optional:"" help:"Reset the selected setting to its default"`
	APIKey          APIKeyConfig          `cmd:"" name:"api-key" help:"Activate OpenRouter immediately."`
	BaseURL         BaseURLConfig         `cmd:"" name:"base-url" help:"Set the API base URL."`
	PokeInterval    PokeIntervalConfig    `cmd:"" name:"poke-interval" help:"Set the background poke cadence."`
	DrainTimeout    DrainTimeoutConfig    `cmd:"" name:"drain-timeout" help:"Bound the time /quit waits for in-flight LLM dispatches to drain on exit."`
	SmallModel      SmallModelConfig      `cmd:"" name:"small-model" help:"Set the model used for lightweight tasks."`
	EmbeddingModel  EmbeddingModelConfig  `cmd:"" name:"embedding-model" help:"Set the embedding model."`
	Highlight       HighlightConfig       `cmd:"" help:"Set words that trigger visual highlighting."`
	DefaultModes    DefaultModesConfig    `cmd:"" name:"default-modes" help:"Set the modes a freshly created channel starts with."`
	TimestampFormat TimestampFormatConfig `cmd:"" name:"timestamp-format" help:"Set or disable timestamp formatting."`
	Persona         PersonaConfig         `cmd:"" help:"Define a custom persona."`
}

// Run implements Command. A bare `/config` invocation, with no
// subcommand, prints every setting's current value, following the
// irssi `/set` convention. Each subcommand implements the same
// behaviour for its own bare form (e.g. [APIKeyConfig.Run]).
// [PersonaConfig] is excluded: it names a collection of personas,
// not a single value, and `/personas` already lists them.
func (c ConfigCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	return func() tea.Msg {
		cfg, err := rc.Config.Load(ctx)
		if err != nil {
			return rc.errorEvent("config", err)
		}

		return configNotice(rc, renderConfig(cfg))
	}
}

// APIKeyConfig represents `/config api-key <value>`.
type APIKeyConfig struct {
	Value string `arg:"" optional:"" help:"OpenRouter API key"`
}

// Run implements Command.
func (c APIKeyConfig) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			if err := rc.Manager.SetAPIKey(ctx, "", config.DefaultBaseURL); err != nil {
				return rc.errorEvent("config api-key", err)
			}

			if _, err := rc.Config.Update(ctx, func(cfg config.Config) config.Config {
				cfg.APIKey = ""
				return cfg
			}); err != nil {
				return rc.errorEvent("config api-key", err)
			}

			return APIKeySetResult{Reset: true}
		}
	}

	if strings.TrimSpace(c.Value) == "" {
		return func() tea.Msg {
			cfg, err := rc.Config.Load(ctx)
			if err != nil {
				return rc.errorEvent("config api-key", err)
			}

			return configNotice(rc, configLine("api-key", maskAPIKey(cfg.APIKey)))
		}
	}

	return func() tea.Msg {
		cfg, err := rc.Config.Load(ctx)
		if err != nil {
			return rc.errorEvent("config api-key", err)
		}

		if err := rc.Manager.SetAPIKey(ctx, c.Value, cfg.BaseURL); err != nil {
			return rc.errorEvent("config api-key", err)
		}

		updated, err := rc.Config.Update(ctx, func(cfg config.Config) config.Config {
			cfg.APIKey = c.Value
			return cfg
		})
		if err != nil {
			return rc.errorEvent("config api-key", err)
		}

		// A small-model id set before any key existed was persisted
		// unvalidated, on the promise it would be checked once a key
		// was set: this is that check. EmbeddingModel needs no
		// equivalent call here: SetAPIKey above already changed the
		// stored API key, which memory.NewDefaultStore's own
		// OnChange listener reacts to by re-probing the embedding
		// endpoint on its own.
		setResult := func() tea.Msg { return APIKeySetResult{} }

		if err := rc.Manager.EnsureStructuredOutputModel(ctx, updated.SmallModel); err != nil {
			warning := configNotice(rc, fmt.Sprintf(
				"warning: the stored small-model %s failed catalogue validation: %s",
				updated.SmallModel, err,
			))

			// tea.BatchMsg (not the tea.Cmd tea.Batch returns) is
			// what a tea.Cmd must return as its tea.Msg for the
			// bubbletea runtime to run both of these: it intercepts
			// a BatchMsg before the value ever reaches Update.
			return tea.BatchMsg{setResult, func() tea.Msg { return warning }}
		}

		return setResult()
	}
}

// BaseURLConfig represents `/config base-url <url>`.
type BaseURLConfig struct {
	URL string `arg:"" optional:"" help:"API base URL"`
}

// Run implements Command.
func (c BaseURLConfig) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			if err := rc.Manager.SetBaseURL(ctx, config.DefaultBaseURL); err != nil {
				return rc.errorEvent("config base-url", err)
			}

			if _, err := rc.Config.Update(ctx, func(cfg config.Config) config.Config {
				cfg.BaseURL = config.DefaultBaseURL
				return cfg
			}); err != nil {
				return rc.errorEvent("config base-url", err)
			}

			return BaseURLSetResult{URL: config.DefaultBaseURL, Reset: true}
		}
	}

	if strings.TrimSpace(c.URL) == "" {
		return func() tea.Msg {
			cfg, err := rc.Config.Load(ctx)
			if err != nil {
				return rc.errorEvent("config base-url", err)
			}

			return configNotice(rc, configLine("base-url", cfg.BaseURL))
		}
	}

	return func() tea.Msg {
		if err := rc.Manager.SetBaseURL(ctx, c.URL); err != nil {
			return rc.errorEvent("config base-url", err)
		}

		if _, err := rc.Config.Update(ctx, func(cfg config.Config) config.Config {
			cfg.BaseURL = c.URL
			return cfg
		}); err != nil {
			return rc.errorEvent("config base-url", err)
		}

		return BaseURLSetResult{URL: c.URL}
	}
}

// PokeIntervalConfig represents `/config poke-interval <duration>`.
type PokeIntervalConfig struct {
	Duration string `arg:"" optional:"" help:"Poke interval (e.g. 5m, 1h)"`
}

// Sources implements command.Completer.
func (PokeIntervalConfig) Sources() map[string]command.SuggestionSource[CompletionContext] {
	return map[string]command.SuggestionSource[CompletionContext]{
		"duration": command.LiteralSource[CompletionContext](
			command.Suggestion{Value: "5m", Label: "5m", Detail: "Fast poke cadence"},
			command.Suggestion{Value: "10m", Label: "10m", Detail: "Balanced poke cadence"},
			command.Suggestion{Value: "30m", Label: "30m", Detail: "Quiet channels"},
			command.Suggestion{Value: "1h", Label: "1h", Detail: "Very low activity"},
		),
	}
}

// Run implements Command.
func (c PokeIntervalConfig) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			if _, err := rc.Config.Update(ctx, func(cfg config.Config) config.Config {
				cfg.PokeInterval = config.DefaultPokeInterval
				return cfg
			}); err != nil {
				return rc.errorEvent("config poke-interval", err)
			}

			return PokeIntervalSetResult{Interval: config.DefaultPokeInterval, Reset: true}
		}
	}

	if strings.TrimSpace(c.Duration) == "" {
		return func() tea.Msg {
			cfg, err := rc.Config.Load(ctx)
			if err != nil {
				return rc.errorEvent("config poke-interval", err)
			}

			return configNotice(rc, configLine("poke-interval", cfg.PokeInterval.String()))
		}
	}

	return func() tea.Msg {
		interval, err := time.ParseDuration(c.Duration)
		if err != nil {
			return rc.errorEvent("config poke-interval", domain.InvalidDurationError{
				Input: c.Duration,
				Err:   err,
				At:    time.Now(),
			})
		}

		// A poke interval below the floor either disables the
		// spec-mandated poke feature outright (zero or negative) or
		// starts a tight, paid poke loop from a single typo (e.g. "5s"
		// meant as "5m"). MinPokeInterval catches both in one check.
		if interval < config.MinPokeInterval {
			return rc.errorEvent("config poke-interval", domain.PokeIntervalOutOfRangeError{
				Interval: interval,
				Floor:    config.MinPokeInterval,
				At:       time.Now(),
			})
		}

		if _, err := rc.Config.Update(ctx, func(cfg config.Config) config.Config {
			cfg.PokeInterval = interval
			return cfg
		}); err != nil {
			return rc.errorEvent("config poke-interval", err)
		}

		return PokeIntervalSetResult{Interval: interval}
	}
}

// DrainTimeoutConfig represents `/config drain-timeout <duration>`.
// The configured value bounds [session.Session.Shutdown] in `main`'s
// teardown sequence: how long the binary waits for in-flight LLM
// dispatches to drain before logging a warning and exiting anyway.
type DrainTimeoutConfig struct {
	Duration string `arg:"" optional:"" help:"Drain timeout (e.g. 5s, 10s, 30s)"`
}

// Sources implements command.Completer.
func (DrainTimeoutConfig) Sources() map[string]command.SuggestionSource[CompletionContext] {
	return map[string]command.SuggestionSource[CompletionContext]{
		"duration": command.LiteralSource[CompletionContext](
			command.Suggestion{Value: "5s", Label: "5s", Detail: "Quick drain"},
			command.Suggestion{Value: "10s", Label: "10s", Detail: "Default drain bound"},
			command.Suggestion{Value: "30s", Label: "30s", Detail: "Patient drain"},
			command.Suggestion{Value: "1m", Label: "1m", Detail: "Long-running dispatches"},
		),
	}
}

// Run implements Command.
func (c DrainTimeoutConfig) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			if _, err := rc.Config.Update(ctx, func(cfg config.Config) config.Config {
				cfg.DrainTimeout = config.DefaultDrainTimeout
				return cfg
			}); err != nil {
				return rc.errorEvent("config drain-timeout", err)
			}

			return DrainTimeoutSetResult{Timeout: config.DefaultDrainTimeout, Reset: true}
		}
	}

	if strings.TrimSpace(c.Duration) == "" {
		return func() tea.Msg {
			cfg, err := rc.Config.Load(ctx)
			if err != nil {
				return rc.errorEvent("config drain-timeout", err)
			}

			return configNotice(rc, configLine("drain-timeout", cfg.DrainTimeout.String()))
		}
	}

	return func() tea.Msg {
		timeout, err := time.ParseDuration(c.Duration)
		if err != nil {
			return rc.errorEvent("config drain-timeout", domain.InvalidDurationError{
				Input: c.Duration,
				Err:   err,
				At:    time.Now(),
			})
		}

		// A non-positive value leaves `main`'s shutdown sequence with
		// no drain bound at all, and a typo below the floor (e.g.
		// "10ms" meant as "10s") would abandon in-flight LLM
		// dispatches almost as soon as `/quit` is asked to wait for
		// them. Mirrors the poke-interval floor check above.
		if timeout < config.MinDrainTimeout {
			return rc.errorEvent("config drain-timeout", domain.DrainTimeoutOutOfRangeError{
				Timeout: timeout,
				Floor:   config.MinDrainTimeout,
				At:      time.Now(),
			})
		}

		if _, err := rc.Config.Update(ctx, func(cfg config.Config) config.Config {
			cfg.DrainTimeout = timeout
			return cfg
		}); err != nil {
			return rc.errorEvent("config drain-timeout", err)
		}

		return DrainTimeoutSetResult{Timeout: timeout}
	}
}

// SmallModelConfig represents `/config small-model <model-id>`.
type SmallModelConfig struct {
	ModelID string `arg:"" optional:"" help:"Model ID for lightweight tasks"`
}

// Run implements Command.
func (c SmallModelConfig) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			rc.Manager.SetSmallModel(ctx, config.DefaultSmallModel)

			if _, err := rc.Config.Update(ctx, func(cfg config.Config) config.Config {
				cfg.SmallModel = config.DefaultSmallModel
				return cfg
			}); err != nil {
				return rc.errorEvent("config small-model", err)
			}

			return SmallModelSetResult{ModelID: config.DefaultSmallModel, Reset: true}
		}
	}

	if strings.TrimSpace(c.ModelID) == "" {
		return func() tea.Msg {
			cfg, err := rc.Config.Load(ctx)
			if err != nil {
				return rc.errorEvent("config small-model", err)
			}

			return configNotice(rc, configLine("small-model", string(cfg.SmallModel)))
		}
	}

	// A typo in the model id would otherwise surface much later as
	// an opaque nick-generation failure. When a key is configured,
	// validate against the live catalogue before persisting; the
	// small model has to support structured tool-calling output
	// (nick generation asks it for JSON), so the same check `/add-
	// model` uses applies here. With no key yet, validation can't
	// run yet, so persist anyway and say so. A successful `/config
	// api-key` re-runs this same check against whatever small-model
	// value is stored by then (see APIKeyConfig.Run).
	modelID := domain.ModelID(c.ModelID)

	return func() tea.Msg {
		deferred := !rc.Manager.HasAPIKey()

		if !deferred {
			if err := rc.Manager.EnsureStructuredOutputModel(ctx, modelID); err != nil {
				return rc.errorEvent("config small-model", err)
			}
		}

		rc.Manager.SetSmallModel(ctx, modelID)

		if _, err := rc.Config.Update(ctx, func(cfg config.Config) config.Config {
			cfg.SmallModel = modelID
			return cfg
		}); err != nil {
			return rc.errorEvent("config small-model", err)
		}

		if deferred {
			return configNotice(rc, fmt.Sprintf(
				"small-model set to %s (not validated: no API key configured yet; it will be checked the next time an API key is set).",
				modelID,
			))
		}

		return SmallModelSetResult{ModelID: modelID}
	}
}

// EmbeddingModelConfig represents `/config embedding-model <model-id>`.
type EmbeddingModelConfig struct {
	ModelID string `arg:"" optional:"" help:"Model ID for embeddings"`
}

// Run implements Command.
func (c EmbeddingModelConfig) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			if _, err := rc.Config.Update(ctx, func(cfg config.Config) config.Config {
				cfg.EmbeddingModel = config.DefaultEmbeddingModel
				return cfg
			}); err != nil {
				return rc.errorEvent("config embedding-model", err)
			}

			return EmbeddingModelSetResult{ModelID: config.DefaultEmbeddingModel, Reset: true}
		}
	}

	if strings.TrimSpace(c.ModelID) == "" {
		return func() tea.Msg {
			cfg, err := rc.Config.Load(ctx)
			if err != nil {
				return rc.errorEvent("config embedding-model", err)
			}

			return configNotice(rc, configLine("embedding-model", string(cfg.EmbeddingModel)))
		}
	}

	// A typo here silently mixes vector spaces: search keeps running
	// against whatever embeddings the old model already wrote, with
	// no error until results look wrong. The model catalogue is not
	// the right oracle for this id: it lists chat-completion models,
	// and OpenRouter's catalogue carries no embedding models at all,
	// so checking membership there would reject every embedding
	// model id including the app's own default. The real check
	// already exists: memory.NewDefaultStore's OnChange listener
	// re-probes the embedding endpoint itself whenever EmbeddingModel
	// (or APIKey, or BaseURL) changes, and that probe runs
	// synchronously inside the Update call below, before it returns.
	// The value is persisted unconditionally; the reply reports what
	// the probe found, without gating the write on it.
	modelID := domain.ModelID(c.ModelID)

	return func() tea.Msg {
		if _, err := rc.Config.Update(ctx, func(cfg config.Config) config.Config {
			cfg.EmbeddingModel = modelID
			return cfg
		}); err != nil {
			return rc.errorEvent("config embedding-model", err)
		}

		if !rc.Manager.HasAPIKey() {
			return configNotice(rc, fmt.Sprintf(
				"embedding-model set to %s (not probed: no API key configured yet; semantic search will be probed the next time an API key is set).",
				modelID,
			))
		}

		if searchable, probeErr := rc.Manager.EmbeddingSearchable(); !searchable {
			return configNotice(rc, embeddingUnavailableText(modelID, probeErr))
		}

		return EmbeddingModelSetResult{ModelID: modelID}
	}
}

// embeddingUnavailableText reports that semantic search is not
// currently reachable through modelID, and why, for the reply to a
// `/config embedding-model` set that persisted successfully but
// whose probe failed. probeErr is nil when there was no embedding
// endpoint to probe at all (the plain, non-indexed memory store, for
// instance), which is a distinct case from a probe that ran and
// failed.
func embeddingUnavailableText(modelID domain.ModelID, probeErr error) string {
	if probeErr != nil {
		return fmt.Sprintf("embedding-model set to %s; semantic search is unavailable: %s.", modelID, probeErr)
	}

	return fmt.Sprintf("embedding-model set to %s; semantic search is not available this session.", modelID)
}

// HighlightConfig represents `/config highlight <word> [<word>...]`.
// $nick is preserved across a replace unless the caller explicitly
// names "-$nick" to drop it; see [applyHighlightWords].
type HighlightConfig struct {
	Words []string `arg:"" optional:"" help:"Words to highlight; $nick stays included unless you pass -$nick"`
}

// Run implements Command.
func (c HighlightConfig) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			words := append([]string(nil), config.DefaultHighlightWords...)

			if _, err := rc.Config.Update(ctx, func(cfg config.Config) config.Config {
				cfg.HighlightWords = words
				return cfg
			}); err != nil {
				return rc.errorEvent("config highlight", err)
			}

			return HighlightWordsSetResult{Words: words, Reset: true}
		}
	}

	if len(c.Words) == 0 {
		return func() tea.Msg {
			cfg, err := rc.Config.Load(ctx)
			if err != nil {
				return rc.errorEvent("config highlight", err)
			}

			return configNotice(rc, configLine("highlight", formatWords(cfg.HighlightWords)))
		}
	}

	return func() tea.Msg {
		words := applyHighlightWords(c.Words)

		if _, err := rc.Config.Update(ctx, func(cfg config.Config) config.Config {
			cfg.HighlightWords = words
			return cfg
		}); err != nil {
			return rc.errorEvent("config highlight", err)
		}

		return HighlightWordsSetResult{Words: words}
	}
}

// applyHighlightWords computes the persisted highlight-word list
// from a `/config highlight` invocation's words. A replacement list
// that omits "$nick" is ambiguous: it could mean "here are more
// words" or "stop highlighting my nick". This keeps $nick in the
// list unless the caller passes the literal token "-$nick", which is
// read as an explicit removal and dropped from the persisted list
// either way.
func applyHighlightWords(words []string) []string {
	const nick = "$nick"
	const removeNick = "-$nick"

	out := make([]string, 0, len(words)+1)
	hasNick := false
	explicitRemove := false

	for _, w := range words {
		switch w {
		case removeNick:
			explicitRemove = true
		case nick:
			hasNick = true
			out = append(out, w)
		default:
			out = append(out, w)
		}
	}

	if !hasNick && !explicitRemove {
		out = append([]string{nick}, out...)
	}

	return out
}

// DefaultModesConfig represents `/config default-modes <modes>`. The
// value, in [domain.ChannelModes.IRCString] form (e.g. "+nt"), sets
// the modes a freshly created channel starts with; see
// [config.Config.DefaultChannelModes].
type DefaultModesConfig struct {
	Modes string `arg:"" optional:"" help:"Default channel modes, e.g. +nt"`
}

// Run implements Command.
func (c DefaultModesConfig) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			if _, err := rc.Config.Update(ctx, func(cfg config.Config) config.Config {
				cfg.DefaultChannelModes = config.DefaultChannelModesSpec
				return cfg
			}); err != nil {
				return rc.errorEvent("config default-modes", err)
			}

			return configNotice(rc, configLine("default-modes", config.DefaultChannelModesSpec))
		}
	}

	if strings.TrimSpace(c.Modes) == "" {
		return func() tea.Msg {
			cfg, err := rc.Config.Load(ctx)
			if err != nil {
				return rc.errorEvent("config default-modes", err)
			}

			return configNotice(rc, configLine("default-modes", cfg.DefaultChannelModes))
		}
	}

	return func() tea.Msg {
		modes, err := domain.ParseChannelModes(c.Modes)
		if err != nil {
			return rc.errorEvent("config default-modes", err)
		}

		// Canonicalised, not the caller's raw spelling: two
		// equivalent inputs (e.g. "+tn" and "+nt") persist
		// identically, and the bare-show path always displays the
		// same canonical order IRCString defines.
		canonical := modes.IRCString()

		if _, err := rc.Config.Update(ctx, func(cfg config.Config) config.Config {
			cfg.DefaultChannelModes = canonical
			return cfg
		}); err != nil {
			return rc.errorEvent("config default-modes", err)
		}

		return configNotice(rc, configLine("default-modes", canonical))
	}
}

// TimestampFormatConfig represents `/config timestamp-format [<format>...]`.
type TimestampFormatConfig struct {
	Format []string `arg:"" optional:"" help:"Timestamp format"`
}

// Run implements Command. A bare invocation prints usage. An empty
// argument list already means something for this setting: it
// disables timestamps. Treating a bare invocation as a request to
// show the current value, the convention every other setting in
// this file follows, would silently turn a mutating command into a
// read on the same spelling. So this command keeps its bare form as
// usage; the explicit `""` / `”` spelling and `--reset` remain the
// disable and restore actions.
func (c TimestampFormatConfig) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			cfg, err := rc.Config.Update(ctx, func(cfg config.Config) config.Config {
				cfg.TimestampFormat = nil
				return cfg
			})
			if err != nil {
				return rc.errorEvent("config timestamp-format", err)
			}

			return TimestampFormatSetResult{Format: cfg.TimestampFormat, Reset: true}
		}
	}

	if len(c.Format) == 0 {
		return usageCmd("config", "/config timestamp-format <format>")
	}

	return func() tea.Msg {
		format := normaliseTimestampFormat(c.Format)

		if _, err := rc.Config.Update(ctx, func(cfg config.Config) config.Config {
			cfg.TimestampFormat = format
			return cfg
		}); err != nil {
			return rc.errorEvent("config timestamp-format", err)
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
func (c PersonaConfig) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			count, err := rc.Manager.ResetPersonas(ctx)
			if err != nil {
				return rc.errorEvent("config persona", err)
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
		if err := rc.Manager.SetPersona(ctx, c.ID, desc); err != nil {
			return rc.errorEvent("config persona", err)
		}

		return PersonaSetResult{ID: c.ID}
	}
}

func normaliseTimestampFormat(parts []string) *string {
	joined := strings.TrimSpace(strings.Join(parts, " "))
	if joined == `""` || joined == `''` {
		disabled := ""
		return &disabled
	}

	return &joined
}

// configSetting is one row of a `/config` bare-show listing.
type configSetting struct {
	name  string
	value string
}

// configSettings lists every scalar `/config` setting and its
// current value, in the order [ConfigCommand] declares its
// subcommands. Persona is excluded: it names a collection, not a
// single value.
func configSettings(cfg config.Config) []configSetting {
	return []configSetting{
		{"api-key", maskAPIKey(cfg.APIKey)},
		{"base-url", cfg.BaseURL},
		{"poke-interval", cfg.PokeInterval.String()},
		{"drain-timeout", cfg.DrainTimeout.String()},
		{"small-model", string(cfg.SmallModel)},
		{"embedding-model", string(cfg.EmbeddingModel)},
		{"highlight", formatWords(cfg.HighlightWords)},
		{"default-modes", cfg.DefaultChannelModes},
		{"timestamp-format", formatTimestampFormat(cfg.TimestampFormat)},
	}
}

// renderConfig formats every setting as one "key = value" line per
// [configSettings], the irssi `/set` convention, terminated by a
// trailing newline.
func renderConfig(cfg config.Config) string {
	settings := configSettings(cfg)

	lines := make([]string, len(settings))
	for i, s := range settings {
		lines[i] = configLine(s.name, s.value)
	}

	return strings.Join(lines, "\n") + "\n"
}

// configLine formats a single setting as one "key = value" line,
// the same shape [renderConfig] uses for the full listing.
func configLine(name, value string) string {
	return name + " = " + value
}

// maskAPIKeyRevealThreshold is the shortest key length maskAPIKey
// will reveal any characters of. Below it, revealing the last 4
// characters would show most or all of the key, so maskAPIKey shows
// none of it instead.
const maskAPIKeyRevealThreshold = 8

// maskAPIKey reports whether an API key is configured without
// exposing it: "not set", or "set (…" plus its last four
// characters, for a key long enough that doing so does not expose
// most of it.
func maskAPIKey(key string) string {
	if key == "" {
		return "not set"
	}

	if len(key) <= maskAPIKeyRevealThreshold {
		return "set (masked)"
	}

	return "set (…" + key[len(key)-4:] + ")"
}

// formatTimestampFormat renders the tri-state timestamp-format
// value: unset (locale default), explicitly disabled (empty
// string), or the configured format string.
func formatTimestampFormat(format *string) string {
	switch {
	case format == nil:
		return "(locale default)"
	case *format == "":
		return "(disabled)"
	default:
		return *format
	}
}

// formatWords renders a word list for display, e.g. highlight words.
func formatWords(words []string) string {
	if len(words) == 0 {
		return "(none)"
	}

	return strings.Join(words, " ")
}

// configNotice wraps text as a [domain.SystemNotice] targeting the
// invoking window, the shape every `/config` bare-show and full-
// dump reply uses.
func configNotice(rc Context, text string) domain.SystemNotice {
	return domain.SystemNotice{
		Target: rc.Active,
		Text:   text,
		At:     time.Now(),
	}
}
