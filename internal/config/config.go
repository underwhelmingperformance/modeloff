// Package config handles application configuration and persistence
// of settings such as the OpenRouter API key and user preferences.
package config

import (
	"time"

	"github.com/laney/modeloff/internal/domain"
)

// DefaultNickModel is the model used to generate nicknames for invited
// model instances when no override has been configured.
const DefaultNickModel = domain.ModelID("anthropic/claude-haiku-4.5")

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
	NickModel       domain.ModelID `json:"nick_model"`
	EmbeddingModel  domain.ModelID `json:"embedding_model"`
	HighlightWords  []string       `json:"highlight_words"`
	TimestampFormat string         `json:"timestamp_format,omitempty"`
}

// ChangeFunc is called after a successful Save with the old and new
// configuration values. Callbacks should compare the fields they
// care about and return early if nothing relevant changed.
type ChangeFunc func(prev, curr Config)

// UnsubscribeFunc cancels a change subscription when called.
type UnsubscribeFunc func()

// Store defines the interface for loading and saving configuration.
type Store interface {
	Load() (Config, error)
	Save(cfg Config) error

	// OnChange registers a callback to be invoked after every
	// successful Save. It returns a function that removes the
	// callback when called.
	OnChange(fn ChangeFunc) UnsubscribeFunc
}
