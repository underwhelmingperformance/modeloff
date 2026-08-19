package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/config"
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

	cfg, cfgStore, err := loadConfig(t.Context())
	require.NoError(t, err)
	require.NotNil(t, cfgStore)
	require.Equal(t, "testuser", cfg.UserNick)
	require.Equal(t, config.DefaultChannelModesSpec, cfg.DefaultChannelModes)
}

func TestLoadConfig_recoversFromCorruptFile(t *testing.T) {
	dir := newDefaultFileStoreDir(t)
	path := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	cfg, cfgStore, err := loadConfig(t.Context())
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

	_, _, err := loadConfig(t.Context())
	require.Error(t, err)
	require.False(t, errors.Is(err, config.ErrCorrupt))

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
