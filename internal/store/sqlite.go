package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/adrg/xdg"
	_ "github.com/ncruces/go-sqlite3/driver" // SQLite driver.

	"github.com/laney/modeloff/internal/domain"
)

const schema = `
CREATE TABLE IF NOT EXISTS channels (
    name TEXT PRIMARY KEY,
    data TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
    id      INTEGER PRIMARY KEY,
    channel TEXT NOT NULL,
    type    TEXT NOT NULL,
    data    TEXT NOT NULL,
    at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_channel_id
    ON events (channel, id);

CREATE TABLE IF NOT EXISTS instances (
    nick TEXT PRIMARY KEY,
    data TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS memories (
    nick    TEXT NOT NULL,
    key     TEXT NOT NULL,
    content TEXT NOT NULL,
    PRIMARY KEY (nick, key)
);

CREATE TABLE IF NOT EXISTS state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS last_read (
    channel  TEXT PRIMARY KEY REFERENCES channels(name) ON DELETE CASCADE,
    event_id INTEGER NOT NULL REFERENCES events(id)
);
`

// SQLiteStore implements Store using a single SQLite database.
type SQLiteStore struct {
	db *sql.DB
}

// NewDefaultSQLiteStore creates a SQLiteStore using the XDG data
// directory ($XDG_DATA_HOME/modeloff/modeloff.db).
func NewDefaultSQLiteStore() (*SQLiteStore, error) {
	dir := filepath.Join(xdg.DataHome, "modeloff")

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	db, err := sql.Open("sqlite3", filepath.Join(dir, "modeloff.db"))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	return NewSQLiteStore(db)
}

// NewSQLiteStore creates a store backed by the given database. The
// caller owns the connection and its configuration (pool size, DSN,
// etc.). The schema is created if it does not already exist.
func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("create schema: %w", err)
	}

	return &SQLiteStore{db: db}, nil
}

// Close closes the underlying database connection.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// ListChannels implements Store.
func (s *SQLiteStore) ListChannels(ctx context.Context) ([]domain.Channel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM channels ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanJSON[domain.Channel](rows)
}

// GetChannel implements Store.
func (s *SQLiteStore) GetChannel(ctx context.Context, name domain.ChannelName) (domain.Channel, error) {
	var data string

	err := s.db.QueryRowContext(ctx, `SELECT data FROM channels WHERE name = ?`, name).Scan(&data)
	if err != nil {
		return domain.Channel{}, fmt.Errorf("channel %q: %w", name, err)
	}

	var ch domain.Channel
	if err := json.Unmarshal([]byte(data), &ch); err != nil {
		return domain.Channel{}, err
	}

	return ch, nil
}

// SaveChannel implements Store.
func (s *SQLiteStore) SaveChannel(ctx context.Context, ch domain.Channel) error {
	data, err := json.Marshal(ch)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO channels (name, data) VALUES (?, ?)
		 ON CONFLICT (name) DO UPDATE SET data = excluded.data`,
		ch.Name, string(data))

	return err
}

// DeleteChannel implements Store.
func (s *SQLiteStore) DeleteChannel(ctx context.Context, name domain.ChannelName) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM channels WHERE name = ?`, name)

	return err
}

// AppendEvent implements Store.
func (s *SQLiteStore) AppendEvent(ctx context.Context, ch domain.ChannelName, event domain.ChannelEvent) (int64, error) {
	data, err := domain.MarshalChannelEvent(event)
	if err != nil {
		return 0, fmt.Errorf("marshal event: %w", err)
	}

	result, err := s.db.ExecContext(ctx,
		`INSERT INTO events (channel, type, data, at) VALUES (?, ?, ?, ?)`,
		ch, domain.ChannelEventType(event), string(data), domain.ChannelEventTime(event).Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}

	return result.LastInsertId()
}

// EventsBefore implements Store.
func (s *SQLiteStore) EventsBefore(ctx context.Context, ch domain.ChannelName, before *int64, n int) ([]domain.StoredEvent, error) {
	var rows *sql.Rows
	var err error

	if before == nil {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, data FROM events WHERE channel = ?
			 ORDER BY id DESC LIMIT ?`, ch, n)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, data FROM events WHERE channel = ? AND id < ?
			 ORDER BY id DESC LIMIT ?`, ch, *before, n)
	}

	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	events, err := scanStoredEvents(rows)
	if err != nil {
		return nil, err
	}

	// Reverse to chronological order.
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}

	return events, nil
}

// EventsFrom implements Store.
func (s *SQLiteStore) EventsFrom(ctx context.Context, ch domain.ChannelName, from *int64, n int) ([]domain.StoredEvent, error) {
	var rows *sql.Rows
	var err error

	if from == nil {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, data FROM events WHERE channel = ?
			 ORDER BY id ASC LIMIT ?`, ch, n)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, data FROM events WHERE channel = ? AND id >= ?
			 ORDER BY id ASC LIMIT ?`, ch, *from, n)
	}

	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanStoredEvents(rows)
}

// ListInstances implements Store.
func (s *SQLiteStore) ListInstances(ctx context.Context) ([]domain.Instance, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM instances ORDER BY nick`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanJSON[domain.Instance](rows)
}

// GetInstance implements Store.
func (s *SQLiteStore) GetInstance(ctx context.Context, nick domain.Nick) (domain.Instance, error) {
	var data string

	err := s.db.QueryRowContext(ctx, `SELECT data FROM instances WHERE nick = ?`, nick).Scan(&data)
	if err != nil {
		return domain.Instance{}, fmt.Errorf("instance %q: %w", nick, err)
	}

	var inst domain.Instance
	if err := json.Unmarshal([]byte(data), &inst); err != nil {
		return domain.Instance{}, err
	}

	return inst, nil
}

// SaveInstance implements Store.
func (s *SQLiteStore) SaveInstance(ctx context.Context, inst domain.Instance) error {
	data, err := json.Marshal(inst)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO instances (nick, data) VALUES (?, ?)
		 ON CONFLICT (nick) DO UPDATE SET data = excluded.data`,
		inst.Nick, string(data))

	return err
}

// DeleteInstance implements Store.
func (s *SQLiteStore) DeleteInstance(ctx context.Context, nick domain.Nick) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM instances WHERE nick = ?`, nick)

	return err
}

// GetLastChannel implements Store.
func (s *SQLiteStore) GetLastChannel(ctx context.Context) (domain.ChannelName, error) {
	return getState[domain.ChannelName](ctx, s.db, "last_channel")
}

// SetLastChannel implements Store.
func (s *SQLiteStore) SetLastChannel(ctx context.Context, name domain.ChannelName) error {
	return setState(ctx, s.db, "last_channel", string(name))
}

// GetLastRead implements Store.
func (s *SQLiteStore) GetLastRead(ctx context.Context, ch domain.ChannelName) (int64, error) {
	var eventID int64

	err := s.db.QueryRowContext(ctx,
		`SELECT event_id FROM last_read WHERE channel = ?`, ch).Scan(&eventID)
	if err == sql.ErrNoRows {
		return 0, nil
	}

	return eventID, err
}

// SetLastRead implements Store.
func (s *SQLiteStore) SetLastRead(ctx context.Context, ch domain.ChannelName, eventID int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO last_read (channel, event_id) VALUES (?, ?)
		 ON CONFLICT (channel) DO UPDATE SET event_id = excluded.event_id`,
		ch, eventID)

	return err
}

// ReadMemories implements Store.
func (s *SQLiteStore) ReadMemories(ctx context.Context, nick domain.Nick) ([]MemoryEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, content FROM memories WHERE nick = ? ORDER BY key`, nick)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var entries []MemoryEntry

	for rows.Next() {
		var e MemoryEntry
		if err := rows.Scan(&e.Key, &e.Content); err != nil {
			return nil, err
		}

		entries = append(entries, e)
	}

	return entries, rows.Err()
}

// WriteMemory implements Store.
func (s *SQLiteStore) WriteMemory(ctx context.Context, nick domain.Nick, key, content string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO memories (nick, key, content) VALUES (?, ?, ?)
		 ON CONFLICT (nick, key) DO UPDATE SET content = excluded.content`,
		nick, key, content)

	return err
}

// DeleteMemory implements Store.
func (s *SQLiteStore) DeleteMemory(ctx context.Context, nick domain.Nick, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE nick = ? AND key = ?`, nick, key)

	return err
}

// ResetMemories implements Store.
func (s *SQLiteStore) ResetMemories(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM memories`)

	return err
}

// Reset implements Store.
func (s *SQLiteStore) Reset(ctx context.Context) error {
	// Order: children before parents (last_read → channels, events).
	for _, stmt := range []string{
		`DELETE FROM last_read`,
		`DELETE FROM channels`,
		`DELETE FROM events`,
		`DELETE FROM instances`,
		`DELETE FROM memories`,
		`DELETE FROM state`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("reset: %w", err)
		}
	}

	return nil
}

func getState[T ~string](ctx context.Context, db *sql.DB, key string) (T, error) {
	var value string

	err := db.QueryRowContext(ctx, `SELECT value FROM state WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}

	return T(value), nil
}

func setState(ctx context.Context, db *sql.DB, key, value string) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO state (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		key, value)

	return err
}

func scanJSON[T any](rows *sql.Rows) ([]T, error) {
	var result []T

	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}

		var v T
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			return nil, err
		}

		result = append(result, v)
	}

	return result, rows.Err()
}

func scanStoredEvents(rows *sql.Rows) ([]domain.StoredEvent, error) {
	var result []domain.StoredEvent

	for rows.Next() {
		var (
			id   int64
			data string
		)

		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}

		event, err := domain.UnmarshalChannelEvent([]byte(data))
		if err != nil {
			return nil, err
		}

		result = append(result, domain.StoredEvent{ID: id, Event: event})
	}

	return result, rows.Err()
}
