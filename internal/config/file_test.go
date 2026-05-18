package config

import (
	"os"
	"path/filepath"
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
