package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const defaultPokeInterval = 5 * time.Minute

// FileStore implements Store by reading and writing a JSON file on disc.
type FileStore struct {
	path string
}

// NewFileStore creates a FileStore that persists configuration to the
// given directory. The configuration file will be stored as
// config.json within that directory.
func NewFileStore(dir string) *FileStore {
	return &FileStore{
		path: filepath.Join(dir, "config.json"),
	}
}

// NewDefaultFileStore creates a FileStore using the system's default
// configuration directory (~/.config/modeloff or equivalent).
func NewDefaultFileStore() (*FileStore, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}

	return NewFileStore(filepath.Join(base, "modeloff")), nil
}

func defaults() Config {
	nick := "user"

	if u := os.Getenv("USER"); u != "" {
		nick = u
	}

	return Config{
		UserNick:     nick,
		PokeInterval: defaultPokeInterval,
		NickModel:    DefaultNickModel,
	}
}

// Load reads the configuration from disk, returning defaults if the
// file does not yet exist.
func (s *FileStore) Load() (Config, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return defaults(), nil
	}
	if err != nil {
		return Config{}, err
	}

	cfg := defaults()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// Save writes the configuration to disk, creating the directory if
// necessary.
func (s *FileStore) Save(cfg Config) error {
	dir := filepath.Dir(s.path)

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}

	data, err := json.MarshalIndent(cfg, "", "  ") //nolint:gosec // G117: API key is intentionally persisted to the config file.
	if err != nil {
		return err
	}

	return os.WriteFile(s.path, data, 0o600)
}
