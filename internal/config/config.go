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

// Config holds all application settings.
type Config struct {
	APIKey         string         `json:"api_key"`
	UserNick       string         `json:"user_nick"`
	PokeInterval   time.Duration  `json:"poke_interval"`
	NickModel      domain.ModelID `json:"nick_model"`
	HighlightWords []string       `json:"highlight_words"`
}

// Store defines the interface for loading and saving configuration.
type Store interface {
	Load() (Config, error)
	Save(cfg Config) error
}
