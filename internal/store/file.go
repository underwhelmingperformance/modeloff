package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/laney/modeloff/internal/domain"
)

// FileStore implements Store by persisting data as JSON files in a
// directory hierarchy.
type FileStore struct {
	dir string
}

// NewFileStore creates a FileStore rooted at the given directory.
func NewFileStore(dir string) *FileStore {
	return &FileStore{dir: dir}
}

// NewDefaultFileStore creates a FileStore using the system's default
// data directory (~/.local/share/modeloff or equivalent).
func NewDefaultFileStore() (*FileStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	return NewFileStore(filepath.Join(home, ".local", "share", "modeloff")), nil
}

// --- Rooms ---

func (s *FileStore) roomsDir() string {
	return filepath.Join(s.dir, "rooms")
}

func (s *FileStore) roomPath(name domain.RoomName) string {
	return filepath.Join(s.roomsDir(), sanitise(string(name))+".json")
}

// ListRooms returns all persisted rooms.
func (s *FileStore) ListRooms(_ context.Context) ([]domain.Room, error) {
	return loadAll[domain.Room](s.roomsDir())
}

// GetRoom retrieves a room by name.
func (s *FileStore) GetRoom(_ context.Context, name domain.RoomName) (domain.Room, error) {
	var room domain.Room

	data, err := os.ReadFile(s.roomPath(name))
	if err != nil {
		return room, fmt.Errorf("room %q: %w", name, err)
	}

	if err := json.Unmarshal(data, &room); err != nil {
		return room, err
	}

	return room, nil
}

// SaveRoom persists a room, creating or overwriting as needed.
func (s *FileStore) SaveRoom(_ context.Context, room domain.Room) error {
	return saveJSON(s.roomPath(room.Name), room)
}

// DeleteRoom removes a room from the store.
func (s *FileStore) DeleteRoom(_ context.Context, name domain.RoomName) error {
	err := os.Remove(s.roomPath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	return err
}

// --- Messages ---

func (s *FileStore) messagesDir(room domain.RoomName) string {
	return filepath.Join(s.dir, "messages", sanitise(string(room)))
}

func (s *FileStore) messagePath(msg domain.Message) string {
	return filepath.Join(s.messagesDir(msg.Room), msg.ID+".json")
}

// ListMessages returns all messages for a room, in file-order.
func (s *FileStore) ListMessages(_ context.Context, room domain.RoomName) ([]domain.Message, error) {
	return loadAll[domain.Message](s.messagesDir(room))
}

// SaveMessage persists a single message.
func (s *FileStore) SaveMessage(_ context.Context, msg domain.Message) error {
	return saveJSON(s.messagePath(msg), msg)
}

// --- Model instances ---

func (s *FileStore) instancesDir() string {
	return filepath.Join(s.dir, "instances")
}

func (s *FileStore) instancePath(nick domain.Nick) string {
	return filepath.Join(s.instancesDir(), sanitise(string(nick))+".json")
}

// ListInstances returns all persisted model instances.
func (s *FileStore) ListInstances(_ context.Context) ([]domain.ModelInstance, error) {
	return loadAll[domain.ModelInstance](s.instancesDir())
}

// GetInstance retrieves a model instance by nick.
func (s *FileStore) GetInstance(_ context.Context, nick domain.Nick) (domain.ModelInstance, error) {
	var inst domain.ModelInstance

	data, err := os.ReadFile(s.instancePath(nick))
	if err != nil {
		return inst, fmt.Errorf("instance %q: %w", nick, err)
	}

	if err := json.Unmarshal(data, &inst); err != nil {
		return inst, err
	}

	return inst, nil
}

// SaveInstance persists a model instance.
func (s *FileStore) SaveInstance(_ context.Context, inst domain.ModelInstance) error {
	return saveJSON(s.instancePath(inst.Nick), inst)
}

// DeleteInstance removes a model instance from the store.
func (s *FileStore) DeleteInstance(_ context.Context, nick domain.Nick) error {
	err := os.Remove(s.instancePath(nick))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	return err
}

// --- State ---

type appState struct {
	LastRoom domain.RoomName `json:"last_room"`
}

func (s *FileStore) statePath() string {
	return filepath.Join(s.dir, "state.json")
}

func (s *FileStore) loadState() (appState, error) {
	data, err := os.ReadFile(s.statePath())
	if errors.Is(err, fs.ErrNotExist) {
		return appState{}, nil
	}
	if err != nil {
		return appState{}, err
	}

	var st appState
	if err := json.Unmarshal(data, &st); err != nil {
		return appState{}, err
	}

	return st, nil
}

// GetLastRoom returns the name of the room that was open when the
// application last closed. Returns an empty RoomName if none was set.
func (s *FileStore) GetLastRoom(_ context.Context) (domain.RoomName, error) {
	st, err := s.loadState()
	if err != nil {
		return "", err
	}

	return st.LastRoom, nil
}

// SetLastRoom records which room is currently active so it can be
// restored on next launch.
func (s *FileStore) SetLastRoom(_ context.Context, name domain.RoomName) error {
	st, err := s.loadState()
	if err != nil {
		return err
	}

	st.LastRoom = name

	return saveJSON(s.statePath(), st)
}

// --- Helpers ---

// sanitise replaces characters that are unsafe in filenames.
func sanitise(name string) string {
	return strings.NewReplacer(
		"/", "_",
		"\\", "_",
		"\x00", "_",
	).Replace(name)
}

// saveJSON marshals a value and writes it to the given path, creating
// parent directories as needed.
func saveJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}

	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0o600)
}

// loadAll reads all JSON files in a directory and unmarshals them into
// a slice of T.
func loadAll[T any](dir string) ([]T, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var result []T

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}

		p := filepath.Clean(filepath.Join(dir, e.Name()))

		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}

		var v T
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, err
		}

		result = append(result, v)
	}

	return result, nil
}
