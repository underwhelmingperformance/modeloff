package config

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// durationFields is the subset of Config's marshaled JSON this file
// cares about. Decoding into it lets each assertion compare a typed
// field, immune to incidental formatting elsewhere in the document.
type durationFields struct {
	PokeInterval json.RawMessage `json:"poke_interval"`
	DrainTimeout json.RawMessage `json:"drain_timeout"`
}

func TestConfig_MarshalJSON_writes_durations_as_strings(t *testing.T) {
	cfg := Config{PokeInterval: 5 * time.Minute, DrainTimeout: 10 * time.Second}

	data, err := json.Marshal(cfg)
	require.NoError(t, err)

	var got durationFields
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, durationFields{
		PokeInterval: json.RawMessage(`"5m0s"`),
		DrainTimeout: json.RawMessage(`"10s"`),
	}, got)
}

func TestConfig_UnmarshalJSON_accepts_duration_strings(t *testing.T) {
	var got Config
	require.NoError(t, json.Unmarshal(
		[]byte(`{"poke_interval": "5m", "drain_timeout": "30s"}`), &got))

	require.Equal(t, 5*time.Minute, got.PokeInterval)
	require.Equal(t, 30*time.Second, got.DrainTimeout)
}

func TestConfig_UnmarshalJSON_accepts_legacy_nanosecond_integers(t *testing.T) {
	var got Config
	require.NoError(t, json.Unmarshal(
		[]byte(`{"poke_interval": 300000000000, "drain_timeout": 10000000000}`), &got))

	require.Equal(t, 5*time.Minute, got.PokeInterval)
	require.Equal(t, 10*time.Second, got.DrainTimeout)
}

func TestConfig_UnmarshalJSON_rejects_unparseable_duration_string(t *testing.T) {
	var got Config
	err := json.Unmarshal([]byte(`{"poke_interval": "not-a-duration"}`), &got)
	require.Error(t, err)
}

func TestConfig_UnmarshalJSON_preserves_omitted_durations(t *testing.T) {
	got := Config{PokeInterval: 5 * time.Minute, DrainTimeout: 10 * time.Second}
	require.NoError(t, json.Unmarshal([]byte(`{"user_nick": "laney"}`), &got))

	require.Equal(t, 5*time.Minute, got.PokeInterval)
	require.Equal(t, 10*time.Second, got.DrainTimeout)
	require.Equal(t, "laney", got.UserNick)
}

func TestConfig_JSON_round_trip(t *testing.T) {
	custom := "%c"
	want := Config{
		APIKey:          "sk-test",
		BaseURL:         "https://openrouter.ai/api/v1",
		UserNick:        "laney",
		PokeInterval:    17 * time.Minute,
		DrainTimeout:    3 * time.Second,
		SmallModel:      "openai/gpt-5.4-mini",
		EmbeddingModel:  "openai/text-embedding-3-small",
		HighlightWords:  []string{"$nick", "urgent"},
		TimestampFormat: &custom,
	}

	data, err := json.Marshal(want)
	require.NoError(t, err)

	var got Config
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, want, got)
}
