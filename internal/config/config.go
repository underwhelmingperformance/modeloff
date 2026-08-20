// Package config handles application configuration and persistence
// of settings such as the OpenRouter API key and user preferences.
package config

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/laney/modeloff/internal/domain"
)

// DefaultBaseURL is the OpenRouter-compatible API base URL used when
// no override has been configured.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// DefaultPokeInterval is the cadence used for idle channel pokes when
// no override has been configured.
const DefaultPokeInterval = 5 * time.Minute

// MinPokeInterval is the floor `/config poke-interval` enforces, so
// a single typo (e.g. "5s" meant as "5m") cannot start a tight,
// paid poke loop.
const MinPokeInterval = 30 * time.Second

// DefaultDrainTimeout is the deadline `main` allows
// [github.com/laney/modeloff/internal/session.Session.Shutdown] to
// drain in-flight dispatch goroutines before logging a warning.
// Mirrors the http.Server.Shutdown bound: long enough for typical
// LLM round-trips to finish, short enough that a wedged client
// does not hold the binary on exit.
const DefaultDrainTimeout = 10 * time.Second

// MinDrainTimeout is the floor `/config drain-timeout` enforces, the
// same shape MinPokeInterval enforces for the poke interval: a
// non-positive value would leave the shutdown sequence with no bound
// at all, and a typo below this floor would abandon in-flight LLM
// dispatches almost as soon as `/quit` is asked to wait for them.
const MinDrainTimeout = 1 * time.Second

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

// DefaultChannelModesSpec is the mode string a freshly created
// channel starts with when no override has been configured.
const DefaultChannelModesSpec = "+nt"

// Config holds all application settings.
type Config struct {
	APIKey         string         `json:"api_key"`
	BaseURL        string         `json:"base_url,omitempty"`
	UserNick       string         `json:"user_nick"`
	PokeInterval   time.Duration  `json:"poke_interval"`
	DrainTimeout   time.Duration  `json:"drain_timeout"`
	SmallModel     domain.ModelID `json:"small_model"`
	EmbeddingModel domain.ModelID `json:"embedding_model"`
	HighlightWords []string       `json:"highlight_words"`

	// DefaultChannelModes sets the modes a freshly created channel
	// starts with, in [domain.ChannelModes.IRCString] form (e.g.
	// "+nt"). The session applies it at channel creation; `/config`
	// only validates and persists it via [domain.ParseChannelModes].
	DefaultChannelModes string  `json:"default_channel_modes,omitempty"`
	TimestampFormat     *string `json:"timestamp_format,omitempty"`
}

// duration marshals a time.Duration as its human-readable string
// form (e.g. "5m0s"), so a hand-edited config.json accepts values
// like "5m". It also accepts a plain JSON number of nanoseconds, so
// a config.json holding a raw integer for this field keeps loading.
// It exists only to give [Config]'s own MarshalJSON/UnmarshalJSON
// this behaviour for PokeInterval and DrainTimeout; those fields
// stay plain time.Duration in Go so every other package keeps using
// them as such.
type duration time.Duration

func (d duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("parse duration %q: %w", s, err)
		}

		*d = duration(parsed)
		return nil
	}

	var ns int64
	if err := json.Unmarshal(data, &ns); err != nil {
		return fmt.Errorf("duration must be a string or a number of nanoseconds: %w", err)
	}

	*d = duration(ns)
	return nil
}

// MarshalJSON writes PokeInterval and DrainTimeout in their
// human-readable string form (e.g. "5m0s"), so config.json stays
// hand-editable. Every other field marshals exactly as its own
// struct tag says.
func (c Config) MarshalJSON() ([]byte, error) {
	type alias Config

	return json.Marshal(struct {
		alias

		PokeInterval duration `json:"poke_interval"`
		DrainTimeout duration `json:"drain_timeout"`
	}{
		alias:        alias(c),
		PokeInterval: duration(c.PokeInterval),
		DrainTimeout: duration(c.DrainTimeout),
	})
}

// UnmarshalJSON reads PokeInterval and DrainTimeout from their
// human-readable string form ("5m0s"), and also accepts the raw
// nanosecond integer a config.json holds for these fields. Fields
// the input JSON omits keep whatever value *c already carries — the
// same merge-with-defaults behaviour a plain json.Unmarshal into an
// already-populated Config has.
func (c *Config) UnmarshalJSON(data []byte) error {
	type alias Config

	aux := struct {
		*alias

		PokeInterval duration `json:"poke_interval"`
		DrainTimeout duration `json:"drain_timeout"`
	}{
		alias:        (*alias)(c),
		PokeInterval: duration(c.PokeInterval),
		DrainTimeout: duration(c.DrainTimeout),
	}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	c.PokeInterval = time.Duration(aux.PokeInterval)
	c.DrainTimeout = time.Duration(aux.DrainTimeout)

	return nil
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

	// Update loads the current configuration, applies fn, and
	// persists the result as a single operation serialised against
	// every other Save and Update on the same Store. A caller that
	// calls Load, mutates the result itself, and then calls Save
	// races every other such caller: two callers can each read the
	// same "current" value, so the second Save clobbers the first
	// caller's change with a value that never accounted for it.
	// Update returns the configuration fn produced.
	Update(ctx context.Context, fn func(Config) Config) (Config, error)

	// OnChange registers a callback to be invoked after every
	// successful Save or Update. It returns a function that removes
	// the callback when called.
	OnChange(fn ChangeFunc) UnsubscribeFunc
}
