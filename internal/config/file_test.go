package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileStore_LoadDefaults(t *testing.T) {
	t.Setenv("USER", "testuser")

	store := NewFileStore(t.TempDir())

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	want := Config{
		UserNick:     "testuser",
		PokeInterval: 5 * time.Minute,
	}

	if got != want {
		t.Errorf("Load() = %+v, want %+v", got, want)
	}
}

func TestFileStore_LoadDefaultsNoUserEnv(t *testing.T) {
	t.Setenv("USER", "")

	store := NewFileStore(t.TempDir())

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if got.UserNick != "user" {
		t.Errorf("UserNick = %q, want %q", got.UserNick, "user")
	}
}

func TestFileStore_SaveAndLoad(t *testing.T) {
	t.Setenv("USER", "testuser")

	dir := t.TempDir()
	store := NewFileStore(dir)

	saved := Config{
		APIKey:       "sk-test-key",
		UserNick:     "laney",
		PokeInterval: 10 * time.Minute,
		LastRoom:     "¢general",
	}

	if err := store.Save(saved); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if got != saved {
		t.Errorf("Load() = %+v, want %+v", got, saved)
	}
}

func TestFileStore_SaveCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	store := NewFileStore(dir)

	cfg := Config{UserNick: "test", PokeInterval: time.Minute}

	if err := store.Save(cfg); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("config file not created: %v", err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file permissions = %o, want 600", perm)
	}
}

func TestFileStore_LoadMergesWithDefaults(t *testing.T) {
	t.Setenv("USER", "testuser")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// Write a partial config that only sets the API key.
	data := []byte(`{"api_key": "sk-partial"}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	store := NewFileStore(dir)

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	want := Config{
		APIKey:       "sk-partial",
		UserNick:     "testuser",
		PokeInterval: 5 * time.Minute,
	}

	if got != want {
		t.Errorf("Load() = %+v, want %+v", got, want)
	}
}

func TestFileStore_LoadInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	store := NewFileStore(dir)

	_, err := store.Load()
	if err == nil {
		t.Fatal("Load() expected error for invalid JSON, got nil")
	}
}
