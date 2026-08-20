package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
)

// stubConfigStore is a [config.Store] that returns a fixed config and
// load error, so the poke-schedule adapter can be exercised without a
// real file store.
type stubConfigStore struct {
	cfg config.Config
	err error
}

func (s stubConfigStore) Load(context.Context) (config.Config, error) { return s.cfg, s.err }
func (s stubConfigStore) Save(context.Context, config.Config) error   { return nil }

func (s stubConfigStore) Update(_ context.Context, fn func(config.Config) config.Config) (config.Config, error) {
	if s.err != nil {
		return config.Config{}, s.err
	}

	return fn(s.cfg), nil
}

func (s stubConfigStore) OnChange(config.ChangeFunc) config.UnsubscribeFunc {
	return func() {}
}

func TestPokeScheduleFromConfig(t *testing.T) {
	cases := []struct {
		name         string
		cfg          config.Config
		loadErr      error
		wantInterval time.Duration
		wantEnabled  bool
	}{
		{
			name:         "enabled with key and positive interval",
			cfg:          config.Config{APIKey: "k", PokeInterval: 5 * time.Minute},
			wantInterval: 5 * time.Minute,
			wantEnabled:  true,
		},
		{
			name: "disabled without api key",
			cfg:  config.Config{PokeInterval: 5 * time.Minute},
		},
		{
			name: "disabled with non-positive interval",
			cfg:  config.Config{APIKey: "k"},
		},
		{
			name:    "disabled on load error",
			cfg:     config.Config{APIKey: "k", PokeInterval: 5 * time.Minute},
			loadErr: errors.New("boom"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			schedule := pokeScheduleFromConfig(stubConfigStore{cfg: tc.cfg, err: tc.loadErr})

			interval, enabled := schedule(context.Background())

			require.Equal(t, tc.wantInterval, interval)
			require.Equal(t, tc.wantEnabled, enabled)
		})
	}
}

func TestDefaultChannelModesFromConfig(t *testing.T) {
	builtIn, err := domain.ParseChannelModes(config.DefaultChannelModesSpec)
	require.NoError(t, err)

	cases := []struct {
		name string
		cfg  config.Config
		want domain.ChannelModes
	}{
		{
			name: "parses the configured spec",
			cfg:  config.Config{DefaultChannelModes: "+mst"},
			want: domain.ChannelModes{Moderated: true, Secret: true, TopicLock: true},
		},
		{
			name: "an empty spec is not a mode string, so the built-in default applies",
			cfg:  config.Config{},
			want: builtIn,
		},
		{
			name: "a hand-edited spec this build cannot parse falls back",
			cfg:  config.Config{DefaultChannelModes: "+zz"},
			want: builtIn,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			supplier, err := defaultChannelModesFromConfig(tc.cfg, &recordingConfigStore{})
			require.NoError(t, err)

			require.Equal(t, tc.want, supplier(t.Context()))
		})
	}
}

// TestDefaultChannelModesFromConfig_refreshes_on_a_config_change pins
// that a `/config default-modes` edit applies to the next channel
// created, without the session being rebuilt. A saved value this
// build cannot parse leaves the built-in default in place.
func TestDefaultChannelModesFromConfig_refreshes_on_a_config_change(t *testing.T) {
	builtIn, err := domain.ParseChannelModes(config.DefaultChannelModesSpec)
	require.NoError(t, err)

	store := &recordingConfigStore{}

	supplier, err := defaultChannelModesFromConfig(config.Config{DefaultChannelModes: "+m"}, store)
	require.NoError(t, err)

	require.Equal(t, domain.ChannelModes{Moderated: true}, supplier(t.Context()))

	store.change(t.Context(), config.Config{DefaultChannelModes: "+m"}, config.Config{DefaultChannelModes: "+i"})
	require.Equal(t, domain.ChannelModes{InviteOnly: true}, supplier(t.Context()))

	store.change(t.Context(), config.Config{DefaultChannelModes: "+i"}, config.Config{DefaultChannelModes: "+zz"})
	require.Equal(t, builtIn, supplier(t.Context()))
}

// TestDefaultChannelModesFromConfig_supplier_does_not_read_the_store
// pins the property finding 1 of the landing-gate review was about.
// The session calls the supplier on its command loop, where every
// other client's command waits behind it, so a call must not reach
// the config store and the file read behind it.
func TestDefaultChannelModesFromConfig_supplier_does_not_read_the_store(t *testing.T) {
	store := &recordingConfigStore{}

	supplier, err := defaultChannelModesFromConfig(config.Config{DefaultChannelModes: "+m"}, store)
	require.NoError(t, err)

	for range 3 {
		require.Equal(t, domain.ChannelModes{Moderated: true}, supplier(t.Context()))
	}

	require.Zero(t, store.loads.Load())
}

// recordingConfigStore is a [config.Store] that counts Load calls and
// lets a test fire the registered change callbacks by hand, which is
// what `/config default-modes` does through Update in production.
type recordingConfigStore struct {
	loads     atomic.Int64
	callbacks []config.ChangeFunc
}

func (s *recordingConfigStore) Load(context.Context) (config.Config, error) {
	s.loads.Add(1)

	return config.Config{}, nil
}

func (s *recordingConfigStore) Save(context.Context, config.Config) error { return nil }

func (s *recordingConfigStore) Update(_ context.Context, fn func(config.Config) config.Config) (config.Config, error) {
	return fn(config.Config{}), nil
}

func (s *recordingConfigStore) OnChange(fn config.ChangeFunc) config.UnsubscribeFunc {
	s.callbacks = append(s.callbacks, fn)

	return func() {}
}

// change fires every registered callback, as a successful Save or
// Update does.
func (s *recordingConfigStore) change(ctx context.Context, prev, curr config.Config) {
	for _, fn := range s.callbacks {
		fn(ctx, prev, curr)
	}
}

// newDefaultFileStoreDir points config.NewDefaultFileStore at a
// fresh temp directory for the duration of the test, and returns the
// directory config.json lives in.
func newDefaultFileStoreDir(t *testing.T) string {
	t.Helper()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("USER", "testuser")

	cfgStore, err := config.NewDefaultFileStore()
	require.NoError(t, err)

	dir := filepath.Dir(cfgStore.Path())
	require.NoError(t, os.MkdirAll(dir, 0o750))

	return dir
}

func TestLoadConfig_missingFileReturnsDefaults(t *testing.T) {
	newDefaultFileStoreDir(t)

	cfg, cfgStore, recoveredFrom, err := loadConfig(t.Context())
	require.NoError(t, err)
	require.NotNil(t, cfgStore)
	require.Equal(t, "testuser", cfg.UserNick)
	require.Equal(t, config.DefaultChannelModesSpec, cfg.DefaultChannelModes)
	require.Empty(t, recoveredFrom, "no recovery ran, so there is nothing to report")
}

func TestLoadConfig_recoversFromCorruptFile(t *testing.T) {
	dir := newDefaultFileStoreDir(t)
	path := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	cfg, cfgStore, recoveredFrom, err := loadConfig(t.Context())
	require.NoError(t, err)
	require.NotNil(t, cfgStore)
	require.Equal(t, "testuser", cfg.UserNick)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}

	// RecoverCorrupt renames the corrupt file aside; loadConfig
	// returns the resulting defaults in memory without writing them
	// back, so config.json itself does not reappear here. The
	// directory listing must be exactly the one backup file
	// RecoverCorrupt created: comparing the whole slice against a
	// single-element slice built from its first entry catches a
	// stray extra entry with the same assertion that catches a
	// missing one.
	require.NotEmpty(t, names)
	backup := names[0]
	require.Equal(t, []string{backup}, names)
	require.Regexp(t, `^config\.json\.corrupt-\d{8}T\d{6}Z$`, backup)
	require.Equal(t, filepath.Join(dir, backup), recoveredFrom,
		"the reported backup path must be the one RecoverCorrupt created")

	backupData, err := os.ReadFile(filepath.Join(dir, backup)) //nolint:gosec // G304: dir is a t.TempDir() path in this test.
	require.NoError(t, err)
	require.Equal(t, "{not json", string(backupData))
}

// TestLoadConfig_nonCorruptFailureIsNotRecovered pins that loadConfig
// only recovers a genuine decode failure. A config.json that exists
// but cannot be read for an unrelated reason (here, the path is a
// directory, so os.ReadFile fails without ever reaching
// json.Unmarshal) must surface as the fatal startup error it always
// was, with the original path left exactly as it was: a permissions
// hiccup or a flaky filesystem is not corruption, and renaming the
// path aside would discard a config file that was never broken.
func TestLoadConfig_nonCorruptFailureIsNotRecovered(t *testing.T) {
	dir := newDefaultFileStoreDir(t)
	path := filepath.Join(dir, "config.json")
	require.NoError(t, os.Mkdir(path, 0o750))

	_, _, recoveredFrom, err := loadConfig(t.Context())
	require.Error(t, err)
	require.False(t, errors.Is(err, config.ErrCorrupt))
	require.Empty(t, recoveredFrom, "a failure that was never recovered reports no backup path")

	info, statErr := os.Stat(path)
	require.NoError(t, statErr)
	require.True(t, info.IsDir(), "the original path must be left untouched")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	require.Equal(t, []string{"config.json"}, names, "no backup file must have been created")
}

// TestNewAPIClient covers the boot-time decision the manager's
// nil-client handling depends on. A nil client is what every LLM path
// reads as "no key", and each has a quiet answer ready for it. An
// empty key must therefore produce no client at all: one built around
// an empty key looks configured to every caller that checks, and then
// fails against OpenRouter on every turn, nick generation and
// catalogue load.
func TestNewAPIClient(t *testing.T) {
	cases := []struct {
		name       string
		apiKey     string
		wantClient bool
	}{
		{name: "no key configured", apiKey: "", wantClient: false},
		{name: "whitespace is not a key", apiKey: "   \t\n ", wantClient: false},
		{name: "a configured key builds a client", apiKey: "sk-test", wantClient: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newAPIClient(config.Config{APIKey: tc.apiKey, BaseURL: "https://example.invalid"})

			if !tc.wantClient {
				require.Nil(t, client)
				return
			}

			require.NotNil(t, client)
		})
	}
}
