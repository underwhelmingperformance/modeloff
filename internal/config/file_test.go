package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFileStore_LoadDefaults(t *testing.T) {
	t.Setenv("USER", "testuser")

	store := NewFileStore(t.TempDir())

	got, err := store.Load()
	require.NoError(t, err)

	want := Config{
		UserNick:     "testuser",
		PokeInterval: 5 * time.Minute,
		NickModel:    DefaultNickModel,
	}

	require.Equal(t, want, got)
}

func TestFileStore_LoadDefaultsNoUserEnv(t *testing.T) {
	t.Setenv("USER", "")

	store := NewFileStore(t.TempDir())

	got, err := store.Load()
	require.NoError(t, err)
	require.Equal(t, "user", got.UserNick)
}

func TestFileStore_SaveAndLoad(t *testing.T) {
	t.Setenv("USER", "testuser")

	dir := t.TempDir()
	store := NewFileStore(dir)

	saved := Config{
		APIKey:       "sk-test-key",
		UserNick:     "laney",
		PokeInterval: 10 * time.Minute,
	}

	require.NoError(t, store.Save(saved))

	got, err := store.Load()
	require.NoError(t, err)
	require.Equal(t, saved, got)
}

func TestFileStore_SaveCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	store := NewFileStore(dir)

	cfg := Config{UserNick: "test", PokeInterval: time.Minute}
	require.NoError(t, store.Save(cfg))

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

	got, err := store.Load()
	require.NoError(t, err)

	want := Config{
		APIKey:       "sk-partial",
		UserNick:     "testuser",
		PokeInterval: 5 * time.Minute,
		NickModel:    DefaultNickModel,
	}

	require.Equal(t, want, got)
}

func TestFileStore_LoadInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	require.NoError(t, os.WriteFile(path, []byte(`{not json`), 0o600))

	store := NewFileStore(dir)

	_, err := store.Load()
	require.Error(t, err)
}
