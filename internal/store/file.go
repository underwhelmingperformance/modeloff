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

// --- Channels ---

func (s *FileStore) channelsDir() string {
	return filepath.Join(s.dir, "channels")
}

func (s *FileStore) channelPath(name domain.ChannelName) string {
	return filepath.Join(s.channelsDir(), sanitise(string(name))+".json")
}

// ListChannels returns all persisted channels.
func (s *FileStore) ListChannels(_ context.Context) ([]domain.Channel, error) {
	return loadAll[domain.Channel](s.channelsDir())
}

// GetChannel retrieves a channel by name.
func (s *FileStore) GetChannel(_ context.Context, name domain.ChannelName) (domain.Channel, error) {
	var ch domain.Channel

	data, err := os.ReadFile(s.channelPath(name))
	if err != nil {
		return ch, fmt.Errorf("channel %q: %w", name, err)
	}

	if err := json.Unmarshal(data, &ch); err != nil {
		return ch, err
	}

	return ch, nil
}

// SaveChannel persists a channel, creating or overwriting as needed.
func (s *FileStore) SaveChannel(_ context.Context, ch domain.Channel) error {
	return saveJSON(s.channelPath(ch.Name), ch)
}

// DeleteChannel removes a channel from the store.
func (s *FileStore) DeleteChannel(_ context.Context, name domain.ChannelName) error {
	err := os.Remove(s.channelPath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	return err
}

// --- Messages ---

func (s *FileStore) messagesDir(ch domain.ChannelName) string {
	return filepath.Join(s.dir, "messages", sanitise(string(ch)))
}

func (s *FileStore) messagePath(msg domain.Message) string {
	return filepath.Join(s.messagesDir(msg.Channel), msg.ID+".json")
}

// ListMessages returns all messages for a channel, in file-order.
func (s *FileStore) ListMessages(_ context.Context, ch domain.ChannelName) ([]domain.Message, error) {
	return loadAll[domain.Message](s.messagesDir(ch))
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
	LastChannel domain.ChannelName            `json:"last_channel"`
	LastRead    map[domain.ChannelName]string `json:"last_read,omitempty"`
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

// GetLastChannel returns the name of the channel that was open when
// the application last closed. Returns an empty ChannelName if none
// was set.
func (s *FileStore) GetLastChannel(_ context.Context) (domain.ChannelName, error) {
	st, err := s.loadState()
	if err != nil {
		return "", err
	}

	return st.LastChannel, nil
}

// SetLastChannel records which channel is currently active so it can
// be restored on next launch.
func (s *FileStore) SetLastChannel(_ context.Context, name domain.ChannelName) error {
	st, err := s.loadState()
	if err != nil {
		return err
	}

	st.LastChannel = name

	return saveJSON(s.statePath(), st)
}

// --- Last-read tracking ---

// GetLastRead returns the ID of the last message the user read in a
// channel. Returns an empty string if nothing has been read yet.
func (s *FileStore) GetLastRead(_ context.Context, ch domain.ChannelName) (string, error) {
	st, err := s.loadState()
	if err != nil {
		return "", err
	}

	return st.LastRead[ch], nil
}

// SetLastRead records the ID of the last message the user saw in a
// channel, so the UI can show unread indicators.
func (s *FileStore) SetLastRead(_ context.Context, ch domain.ChannelName, messageID string) error {
	st, err := s.loadState()
	if err != nil {
		return err
	}

	if st.LastRead == nil {
		st.LastRead = make(map[domain.ChannelName]string)
	}

	st.LastRead[ch] = messageID

	return saveJSON(s.statePath(), st)
}

// --- Reset ---

// Reset removes all channels, messages, instances, and state,
// returning the store to an empty state.
func (s *FileStore) Reset(_ context.Context) error {
	dirs := []string{
		s.channelsDir(),
		filepath.Join(s.dir, "messages"),
		s.instancesDir(),
	}

	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("reset %s: %w", dir, err)
		}
	}

	if err := os.Remove(s.statePath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reset state: %w", err)
	}

	return nil
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
