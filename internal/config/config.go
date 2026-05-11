// Package config handles application configuration and persistence
// of settings such as the OpenRouter API key and user preferences.
package config

import (
	"context"
	"time"

	"github.com/laney/modeloff/internal/domain"
)

// DefaultBaseURL is the OpenRouter-compatible API base URL used when
// no override has been configured.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// DefaultPokeInterval is the cadence used for idle channel pokes when
// no override has been configured.
const DefaultPokeInterval = 5 * time.Minute

// DefaultDrainTimeout is the deadline `main` allows
// [github.com/laney/modeloff/internal/session.Session.Shutdown] to
// drain in-flight dispatch goroutines before logging a warning.
// Mirrors the http.Server.Shutdown bound: long enough for typical
// LLM round-trips to finish, short enough that a wedged client
// does not hold the binary on exit.
const DefaultDrainTimeout = 10 * time.Second

// DefaultSmallModel is the model used for lightweight tasks such as
// nick and persona generation when no override has been configured.
const DefaultSmallModel = domain.ModelID("openai/gpt-5.4-mini")

// DefaultHighlightWords is the default set of words that trigger
// visual highlighting. The $nick placeholder is expanded to the
// user's current nick at render time.
var DefaultHighlightWords = []string{"$nick"}

// DefaultEmbeddingModel is the model used to generate vector
// embeddings for the memory system.
const DefaultEmbeddingModel = domain.ModelID("openai/text-embedding-3-small")

// Config holds all application settings.
type Config struct {
	APIKey          string         `json:"api_key"`
	BaseURL         string         `json:"base_url,omitempty"`
	UserNick        string         `json:"user_nick"`
	PokeInterval    time.Duration  `json:"poke_interval"`
	DrainTimeout    time.Duration  `json:"drain_timeout"`
	SmallModel      domain.ModelID `json:"small_model"`
	EmbeddingModel  domain.ModelID `json:"embedding_model"`
	HighlightWords  []string       `json:"highlight_words"`
	TimestampFormat *string        `json:"timestamp_format,omitempty"`
}

// ChangeFunc is called after a successful Save with the old and new
// configuration values. Callbacks should compare the fields they
// care about and return early if nothing relevant changed.
type ChangeFunc func(prev, curr Config)

// UnsubscribeFunc cancels a change subscription when called.
type UnsubscribeFunc func()

// Store defines the interface for loading and saving configuration.
type Store interface {
	Load(ctx context.Context) (Config, error)
	Save(ctx context.Context, cfg Config) error

	// OnChange registers a callback to be invoked after every
	// successful Save. It returns a function that removes the
	// callback when called.
	OnChange(fn ChangeFunc) UnsubscribeFunc
}
