package memory

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/laney/modeloff/internal/domain"
)

// FileStore implements Store by persisting per-nick memories as
// individual JSON files in a directory.
type FileStore struct {
	dir string
}

// NewFileStore creates a FileStore rooted at the given directory. Each
// model instance's memories are stored in a separate JSON file keyed
// by nick.
func NewFileStore(dir string) *FileStore {
	return &FileStore{dir: dir}
}

// NewDefaultFileStore creates a FileStore using the system's default
// data directory (~/.local/share/modeloff/memories or equivalent).
func NewDefaultFileStore() (*FileStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	return NewFileStore(filepath.Join(home, ".local", "share", "modeloff", "memories")), nil
}

func (s *FileStore) path(nick domain.Nick) string {
	return filepath.Join(s.dir, string(nick)+".json")
}

func (s *FileStore) load(nick domain.Nick) ([]Entry, error) {
	data, err := os.ReadFile(s.path(nick))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}

	return entries, nil
}

func (s *FileStore) save(nick domain.Nick, entries []Entry) error {
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return err
	}

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(s.path(nick), data, 0o600)
}

// Read retrieves all memories for a given model instance.
func (s *FileStore) Read(_ context.Context, nick domain.Nick) ([]Entry, error) {
	entries, err := s.load(nick)
	if err != nil {
		return nil, err
	}

	if entries == nil {
		return []Entry{}, nil
	}

	return entries, nil
}

// Write stores a memory entry for a given model instance. If an entry
// with the same key already exists, it is overwritten.
func (s *FileStore) Write(_ context.Context, nick domain.Nick, entry Entry) error {
	entries, err := s.load(nick)
	if err != nil {
		return err
	}

	found := false
	for i, e := range entries {
		if e.Key == entry.Key {
			entries[i] = entry
			found = true
			break
		}
	}

	if !found {
		entries = append(entries, entry)
	}

	return s.save(nick, entries)
}

// Delete removes a specific memory entry by key.
func (s *FileStore) Delete(_ context.Context, nick domain.Nick, key string) error {
	entries, err := s.load(nick)
	if err != nil {
		return err
	}

	filtered := entries[:0]
	for _, e := range entries {
		if e.Key != key {
			filtered = append(filtered, e)
		}
	}

	if len(filtered) == len(entries) {
		return nil
	}

	return s.save(nick, filtered)
}
