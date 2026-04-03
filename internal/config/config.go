// Package config handles application configuration and persistence
// of settings such as the OpenRouter API key and user preferences.
package config

import "time"

// Config holds all application settings.
type Config struct {
	APIKey       string        `json:"api_key"`
	UserNick     string        `json:"user_nick"`
	PokeInterval time.Duration `json:"poke_interval"`
	LastRoom     string        `json:"last_room"`
}

// Store defines the interface for loading and saving configuration.
type Store interface {
	Load() (Config, error)
	Save(cfg Config) error
}
