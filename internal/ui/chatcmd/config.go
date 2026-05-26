package chatcmd

import (
	"context"
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
	TimestampFormat TimestampFormatConfig `cmd:"" name:"timestamp-format" help:"Set or disable timestamp formatting."`
	Persona         PersonaConfig         `cmd:"" help:"Define a custom persona."`
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
				return errorEvent("config api-key", err)
			}

			if _, err := rc.updateConfig(ctx, func(cfg *config.Config) {
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
		cfg, err := rc.Config.Load(ctx)
		if err != nil {
			return errorEvent("config api-key", err)
		}

		if err := rc.Manager.SetAPIKey(ctx, c.Value, cfg.BaseURL); err != nil {
			return errorEvent("config api-key", err)
		}

		if _, err := rc.updateConfig(ctx, func(cfg *config.Config) {
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
func (c BaseURLConfig) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			if err := rc.Manager.SetBaseURL(ctx, config.DefaultBaseURL); err != nil {
				return errorEvent("config base-url", err)
			}

			if _, err := rc.updateConfig(ctx, func(cfg *config.Config) {
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
		if err := rc.Manager.SetBaseURL(ctx, c.URL); err != nil {
			return errorEvent("config base-url", err)
		}

		if _, err := rc.updateConfig(ctx, func(cfg *config.Config) {
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
			if _, err := rc.updateConfig(ctx, func(cfg *config.Config) {
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
				At:    time.Now(),
			})
		}

		if _, err := rc.updateConfig(ctx, func(cfg *config.Config) {
			cfg.PokeInterval = interval
		}); err != nil {
			return errorEvent("config poke-interval", err)
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
			if _, err := rc.updateConfig(ctx, func(cfg *config.Config) {
				cfg.DrainTimeout = config.DefaultDrainTimeout
			}); err != nil {
				return errorEvent("config drain-timeout", err)
			}

			return DrainTimeoutSetResult{Timeout: config.DefaultDrainTimeout, Reset: true}
		}
	}

	if strings.TrimSpace(c.Duration) == "" {
		return usageCmd("config", "/config drain-timeout <duration>")
	}

	return func() tea.Msg {
		timeout, err := time.ParseDuration(c.Duration)
		if err != nil {
			return errorEvent("config drain-timeout", domain.InvalidDurationError{
				Input: c.Duration,
				Err:   err,
				At:    time.Now(),
			})
		}

		if _, err := rc.updateConfig(ctx, func(cfg *config.Config) {
			cfg.DrainTimeout = timeout
		}); err != nil {
			return errorEvent("config drain-timeout", err)
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

			if _, err := rc.updateConfig(ctx, func(cfg *config.Config) {
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
		rc.Manager.SetSmallModel(ctx, modelID)

		if _, err := rc.updateConfig(ctx, func(cfg *config.Config) {
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
func (c EmbeddingModelConfig) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			if _, err := rc.updateConfig(ctx, func(cfg *config.Config) {
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

		if _, err := rc.updateConfig(ctx, func(cfg *config.Config) {
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
func (c HighlightConfig) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			words := append([]string(nil), config.DefaultHighlightWords...)

			if _, err := rc.updateConfig(ctx, func(cfg *config.Config) {
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
		if _, err := rc.updateConfig(ctx, func(cfg *config.Config) {
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
func (c TimestampFormatConfig) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			cfg, err := rc.updateConfig(ctx, func(cfg *config.Config) {
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

		if _, err := rc.updateConfig(ctx, func(cfg *config.Config) {
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
func (c PersonaConfig) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			count, err := rc.Manager.ResetPersonas(ctx)
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
		if err := rc.Manager.SetPersona(ctx, c.ID, desc); err != nil {
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
