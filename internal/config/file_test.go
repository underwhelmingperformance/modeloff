package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/observability/oteltest"
)

func TestFileStore_LoadDefaults(t *testing.T) {
	t.Setenv("USER", "testuser")

	store := NewFileStore(t.TempDir())

	got, err := store.Load(t.Context())
	require.NoError(t, err)

	want := Config{
		BaseURL:        "https://openrouter.ai/api/v1",
		UserNick:       "testuser",
		PokeInterval:   5 * time.Minute,
		DrainTimeout:   DefaultDrainTimeout,
		SmallModel:     DefaultSmallModel,
		EmbeddingModel: DefaultEmbeddingModel,
		HighlightWords: []string{"$nick"},
	}

	require.Equal(t, want, got)
}

func TestFileStore_LoadDefaultsNoUserEnv(t *testing.T) {
	t.Setenv("USER", "")

	store := NewFileStore(t.TempDir())

	got, err := store.Load(t.Context())
	require.NoError(t, err)
	require.Equal(t, "user", got.UserNick)
}

func TestFileStore_SaveAndLoad(t *testing.T) {
	t.Setenv("USER", "testuser")

	dir := t.TempDir()
	store := NewFileStore(dir)

	saved := Config{
		APIKey:         "sk-test-key",
		BaseURL:        "https://openrouter.ai/api/v1",
		UserNick:       "laney",
		PokeInterval:   10 * time.Minute,
		EmbeddingModel: "openai/text-embedding-3-large",
	}

	require.NoError(t, store.Save(t.Context(), saved))

	got, err := store.Load(t.Context())
	require.NoError(t, err)
	require.Equal(t, saved, got)
}

func TestFileStore_Save_recordsSpan(t *testing.T) {
	recorder, provider := oteltest.NewSpanRecorder(t)
	store := NewFileStore(t.TempDir()).WithTracerProvider(provider)

	require.NoError(t, store.Save(t.Context(), Config{UserNick: "laney", PokeInterval: time.Minute}))

	span := oteltest.FindSpan(t, recorder, "config.file.save")
	require.Equal(t, "config.file.save", oteltest.AttrValue(span.Attributes(), observability.AttrOperation))
	require.Equal(t, observability.ResultOK, oteltest.AttrValue(span.Attributes(), observability.AttrResult))
}

func TestFileStore_SaveCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	store := NewFileStore(dir)

	cfg := Config{UserNick: "test", PokeInterval: time.Minute}
	require.NoError(t, store.Save(t.Context(), cfg))

	info, err := os.Stat(filepath.Join(dir, "config.json"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestFileStore_LoadMergesWithDefaults(t *testing.T) {
	t.Setenv("USER", "testuser")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	data := []byte(`{"api_key": "sk-partial"}`)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	store := NewFileStore(dir)

	got, err := store.Load(t.Context())
	require.NoError(t, err)

	want := Config{
		APIKey:         "sk-partial",
		BaseURL:        "https://openrouter.ai/api/v1",
		UserNick:       "testuser",
		PokeInterval:   5 * time.Minute,
		DrainTimeout:   DefaultDrainTimeout,
		SmallModel:     DefaultSmallModel,
		EmbeddingModel: DefaultEmbeddingModel,
		HighlightWords: []string{"$nick"},
	}

	require.Equal(t, want, got)
}

func TestFileStore_LoadInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	require.NoError(t, os.WriteFile(path, []byte(`{not json`), 0o600))

	store := NewFileStore(dir)

	_, err := store.Load(t.Context())
	require.Error(t, err)
}

func TestFileStore_Save_writes_human_readable_durations_on_disk(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)

	require.NoError(t, store.Save(t.Context(), Config{
		UserNick:     "laney",
		PokeInterval: 5 * time.Minute,
		DrainTimeout: 10 * time.Second,
	}))

	raw, err := os.ReadFile(filepath.Join(dir, "config.json")) //nolint:gosec // G304: dir is t.TempDir(), not external input.
	require.NoError(t, err)

	var got struct {
		PokeInterval string `json:"poke_interval"`
		DrainTimeout string `json:"drain_timeout"`
	}
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, "5m0s", got.PokeInterval)
	require.Equal(t, "10s", got.DrainTimeout)
}

// TestFileStore_Save_leaves_no_temp_file pins that the atomic
// temp-then-rename write cleans up after itself: only config.json
// remains in the directory once Save returns.
func TestFileStore_Save_leaves_no_temp_file(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)

	require.NoError(t, store.Save(t.Context(), Config{UserNick: "laney", PokeInterval: time.Minute}))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}

	require.Equal(t, []string{"config.json"}, names)
}

// TestFileStore_Save_concurrent_calls_serialize_correct_diffs pins
// that concurrent Save calls never interleave: every OnChange
// callback invocation is handed a (prev, curr) pair that actually
// occurred in some serial ordering of the Saves, never a prev value
// some other, later Save already overwrote.
func TestFileStore_Save_concurrent_calls_serialize_correct_diffs(t *testing.T) {
	store := NewFileStore(t.TempDir())

	seen := make(map[time.Duration]time.Duration)
	var mu sync.Mutex

	store.OnChange(func(prev, curr Config) {
		mu.Lock()
		seen[prev.PokeInterval] = curr.PokeInterval
		mu.Unlock()
	})

	const n = 20

	// Offset well clear of defaults().PokeInterval (5 minutes) so
	// the very first Save's prev — the default, since no file exists
	// yet — can never coincide by value with any call's own curr.
	var wg sync.WaitGroup
	for i := 1; i <= n; i++ {
		wg.Go(func() {
			require.NoError(t, store.Save(t.Context(), Config{PokeInterval: time.Duration(100+i) * time.Minute}))
		})
	}
	wg.Wait()

	// A mutex-serialized Save reads a fresh "old" value only after
	// every prior Save has fully committed, so each of the n calls
	// observes a distinct prev value and records its own curr under
	// that key. A race would let two calls read the same stale prev,
	// so one call's curr would overwrite another's under the
	// colliding key — comparing the full sorted set of recorded curr
	// values against every curr this test actually wrote catches
	// that collision (fewer than n distinct curr values) as well as
	// a wrong-pairing bug a bare count would miss (n curr values
	// present, but not the right n).
	gotCurrs := make([]time.Duration, 0, len(seen))
	for _, curr := range seen {
		gotCurrs = append(gotCurrs, curr)
	}
	slices.Sort(gotCurrs)

	wantCurrs := make([]time.Duration, n)
	for i := range n {
		wantCurrs[i] = time.Duration(101+i) * time.Minute
	}

	require.Equal(t, wantCurrs, gotCurrs)
}

func TestFileStore_OnChange_fires_callback(t *testing.T) {
	t.Setenv("USER", "testuser")

	store := NewFileStore(t.TempDir())

	var received []Config

	store.OnChange(func(prev, curr Config) {
		received = append(received, prev, curr)
	})

	saved := Config{APIKey: "sk-new", UserNick: "laney", PokeInterval: time.Minute}
	require.NoError(t, store.Save(t.Context(), saved))

	require.Equal(t, []Config{
		{
			BaseURL:        "https://openrouter.ai/api/v1",
			UserNick:       "testuser",
			PokeInterval:   5 * time.Minute,
			DrainTimeout:   DefaultDrainTimeout,
			SmallModel:     DefaultSmallModel,
			EmbeddingModel: DefaultEmbeddingModel,
			HighlightWords: []string{"$nick"},
		},
		saved,
	}, received)
}

func TestFileStore_OnChange_unsubscribe(t *testing.T) {
	store := NewFileStore(t.TempDir())

	calls := 0
	unsub := store.OnChange(func(_, _ Config) { calls++ })

	require.NoError(t, store.Save(t.Context(), Config{UserNick: "a"}))
	require.Equal(t, 1, calls)

	unsub()

	require.NoError(t, store.Save(t.Context(), Config{UserNick: "b"}))
	require.Equal(t, 1, calls)
}

func TestFileStore_OnChange_multiple_callbacks(t *testing.T) {
	store := NewFileStore(t.TempDir())

	var mu sync.Mutex
	var order []int

	for i := range 3 {
		store.OnChange(func(_, _ Config) {
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
		})
	}

	require.NoError(t, store.Save(t.Context(), Config{UserNick: "x"}))

	mu.Lock()
	defer mu.Unlock()

	require.ElementsMatch(t, []int{0, 1, 2}, order)
}

func TestFileStore_OnChange_concurrent_safety(t *testing.T) {
	store := NewFileStore(t.TempDir())

	var wg sync.WaitGroup

	for range 10 {
		wg.Go(func() {
			unsub := store.OnChange(func(_, _ Config) {})
			unsub()
		})
	}

	wg.Wait()
}

func TestFileStore_SaveAndLoadHighlightWords(t *testing.T) {
	t.Setenv("USER", "testuser")

	dir := t.TempDir()
	store := NewFileStore(dir)

	saved := Config{
		BaseURL:        "https://openrouter.ai/api/v1",
		UserNick:       "testuser",
		PokeInterval:   5 * time.Minute,
		DrainTimeout:   DefaultDrainTimeout,
		SmallModel:     DefaultSmallModel,
		HighlightWords: []string{"$nick", "important", "urgent"},
	}

	require.NoError(t, store.Save(t.Context(), saved))

	got, err := store.Load(t.Context())
	require.NoError(t, err)
	require.Equal(t, saved, got)
}

func TestFileStore_SaveAndLoadTimestampFormat(t *testing.T) {
	t.Setenv("USER", "testuser")

	dir := t.TempDir()
	store := NewFileStore(dir)
	custom := "%c"

	saved := Config{
		BaseURL:         "https://openrouter.ai/api/v1",
		UserNick:        "testuser",
		PokeInterval:    5 * time.Minute,
		DrainTimeout:    DefaultDrainTimeout,
		SmallModel:      DefaultSmallModel,
		EmbeddingModel:  DefaultEmbeddingModel,
		HighlightWords:  []string{"$nick"},
		TimestampFormat: &custom,
	}

	require.NoError(t, store.Save(t.Context(), saved))

	got, err := store.Load(t.Context())
	require.NoError(t, err)
	require.Equal(t, saved, got)
}

func TestFileStore_SaveAndLoadDisabledTimestampFormat(t *testing.T) {
	t.Setenv("USER", "testuser")

	dir := t.TempDir()
	store := NewFileStore(dir)
	disabled := ""

	saved := Config{
		BaseURL:         "https://openrouter.ai/api/v1",
		UserNick:        "testuser",
		PokeInterval:    5 * time.Minute,
		DrainTimeout:    DefaultDrainTimeout,
		SmallModel:      DefaultSmallModel,
		EmbeddingModel:  DefaultEmbeddingModel,
		HighlightWords:  []string{"$nick"},
		TimestampFormat: &disabled,
	}

	require.NoError(t, store.Save(t.Context(), saved))

	got, err := store.Load(t.Context())
	require.NoError(t, err)
	require.NotNil(t, got.TimestampFormat)
	require.Equal(t, "", *got.TimestampFormat)
}
