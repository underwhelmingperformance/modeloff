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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
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

CREATE TABLE IF NOT EXISTS personas (
    id          TEXT PRIMARY KEY,
    description TEXT NOT NULL,
    origin      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS autojoin (
    name TEXT PRIMARY KEY
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
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.list_channels")
	defer span.End()

	rows, err := s.db.QueryContext(ctx, `SELECT data FROM channels ORDER BY name`)
	if err != nil {
		recordSQLiteError(span, err)
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	channels, err := scanJSON[domain.Channel](rows)
	if err != nil {
		recordSQLiteError(span, err)
		return nil, err
	}

	recordSQLiteSuccess(span)
	return channels, nil
}

// GetChannel implements Store.
func (s *SQLiteStore) GetChannel(ctx context.Context, name domain.ChannelName) (domain.Channel, error) {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.get_channel", attribute.String(observability.AttrChannel, string(name)))
	defer span.End()

	var data string

	err := s.db.QueryRowContext(ctx, `SELECT data FROM channels WHERE name = ?`, name).Scan(&data)
	if err != nil {
		recordSQLiteError(span, err)
		return domain.Channel{}, fmt.Errorf("channel %q: %w", name, err)
	}

	var ch domain.Channel
	if err := json.Unmarshal([]byte(data), &ch); err != nil {
		recordSQLiteError(span, err)
		return domain.Channel{}, err
	}

	recordSQLiteSuccess(span)
	return ch, nil
}

// SaveChannel implements Store.
func (s *SQLiteStore) SaveChannel(ctx context.Context, ch domain.Channel) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.save_channel", attribute.String(observability.AttrChannel, string(ch.Name)))
	defer span.End()

	data, err := json.Marshal(ch)
	if err != nil {
		recordSQLiteError(span, err)
		return err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO channels (name, data) VALUES (?, ?)
		 ON CONFLICT (name) DO UPDATE SET data = excluded.data`,
		ch.Name, string(data))

	if err != nil {
		recordSQLiteError(span, err)
		return err
	}

	recordSQLiteSuccess(span)
	return nil
}

// DeleteChannel implements Store.
func (s *SQLiteStore) DeleteChannel(ctx context.Context, name domain.ChannelName) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.delete_channel", attribute.String(observability.AttrChannel, string(name)))
	defer span.End()

	_, err := s.db.ExecContext(ctx, `DELETE FROM channels WHERE name = ?`, name)
	if err != nil {
		recordSQLiteError(span, err)
		return err
	}

	recordSQLiteSuccess(span)
	return nil
}

// AppendEvent implements Store.
func (s *SQLiteStore) AppendEvent(ctx context.Context, ch domain.ChannelName, event domain.ChannelEvent) (int64, error) {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.append_event", attribute.String(observability.AttrChannel, string(ch)))
	defer span.End()

	data, err := domain.MarshalChannelEvent(event)
	if err != nil {
		recordSQLiteError(span, err)
		return 0, fmt.Errorf("marshal event: %w", err)
	}

	result, err := s.db.ExecContext(ctx,
		`INSERT INTO events (channel, type, data, at) VALUES (?, ?, ?, ?)`,
		ch, domain.ChannelEventType(event), string(data), domain.ChannelEventTime(event).Format(time.RFC3339Nano))
	if err != nil {
		recordSQLiteError(span, err)
		return 0, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		recordSQLiteError(span, err)
		return 0, err
	}

	recordSQLiteSuccess(span)
	return id, nil
}

// EventsBefore implements Store.
func (s *SQLiteStore) EventsBefore(ctx context.Context, ch domain.ChannelName, before *int64, n int) ([]domain.StoredEvent, error) {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.events_before", attribute.String(observability.AttrChannel, string(ch)))
	defer span.End()

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
		recordSQLiteError(span, err)
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	events, err := scanStoredEvents(rows)
	if err != nil {
		recordSQLiteError(span, err)
		return nil, err
	}

	// Reverse to chronological order.
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}

	recordSQLiteSuccess(span)
	return events, nil
}

// EventsFrom implements Store.
func (s *SQLiteStore) EventsFrom(ctx context.Context, ch domain.ChannelName, from *int64, n int) ([]domain.StoredEvent, error) {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.events_from", attribute.String(observability.AttrChannel, string(ch)))
	defer span.End()

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
		recordSQLiteError(span, err)
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	events, err := scanStoredEvents(rows)
	if err != nil {
		recordSQLiteError(span, err)
		return nil, err
	}

	recordSQLiteSuccess(span)
	return events, nil
}

// ListInstances implements Store.
func (s *SQLiteStore) ListInstances(ctx context.Context) ([]domain.Instance, error) {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.list_instances")
	defer span.End()

	rows, err := s.db.QueryContext(ctx, `SELECT data FROM instances ORDER BY nick`)
	if err != nil {
		recordSQLiteError(span, err)
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	instances, err := scanJSON[domain.Instance](rows)
	if err != nil {
		recordSQLiteError(span, err)
		return nil, err
	}

	recordSQLiteSuccess(span)
	return instances, nil
}

// GetInstance implements Store.
func (s *SQLiteStore) GetInstance(ctx context.Context, nick domain.Nick) (domain.Instance, error) {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.get_instance", attribute.String(observability.AttrNick, string(nick)))
	defer span.End()

	var data string

	err := s.db.QueryRowContext(ctx, `SELECT data FROM instances WHERE nick = ?`, nick).Scan(&data)
	if err != nil {
		recordSQLiteError(span, err)
		return domain.Instance{}, fmt.Errorf("instance %q: %w", nick, err)
	}

	var inst domain.Instance
	if err := json.Unmarshal([]byte(data), &inst); err != nil {
		recordSQLiteError(span, err)
		return domain.Instance{}, err
	}

	recordSQLiteSuccess(span)
	return inst, nil
}

// SaveInstance implements Store.
func (s *SQLiteStore) SaveInstance(ctx context.Context, inst domain.Instance) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.save_instance", attribute.String(observability.AttrNick, string(inst.Nick)))
	defer span.End()

	data, err := json.Marshal(inst)
	if err != nil {
		recordSQLiteError(span, err)
		return err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO instances (nick, data) VALUES (?, ?)
		 ON CONFLICT (nick) DO UPDATE SET data = excluded.data`,
		inst.Nick, string(data))

	if err != nil {
		recordSQLiteError(span, err)
		return err
	}

	recordSQLiteSuccess(span)
	return nil
}

// DeleteInstance implements Store.
func (s *SQLiteStore) DeleteInstance(ctx context.Context, nick domain.Nick) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.delete_instance", attribute.String(observability.AttrNick, string(nick)))
	defer span.End()

	_, err := s.db.ExecContext(ctx, `DELETE FROM instances WHERE nick = ?`, nick)
	if err != nil {
		recordSQLiteError(span, err)
		return err
	}

	recordSQLiteSuccess(span)
	return nil
}

// GetLastChannel implements Store.
func (s *SQLiteStore) GetLastChannel(ctx context.Context) (domain.ChannelName, error) {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.get_last_channel")
	defer span.End()

	value, err := getState[domain.ChannelName](ctx, s.db, "last_channel")
	if err != nil {
		recordSQLiteError(span, err)
		return "", err
	}

	recordSQLiteSuccess(span)
	return value, nil
}

// SetLastChannel implements Store.
func (s *SQLiteStore) SetLastChannel(ctx context.Context, name domain.ChannelName) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.set_last_channel", attribute.String(observability.AttrChannel, string(name)))
	defer span.End()

	if err := setState(ctx, s.db, "last_channel", string(name)); err != nil {
		recordSQLiteError(span, err)
		return err
	}

	recordSQLiteSuccess(span)
	return nil
}

// GetLastRead implements Store.
func (s *SQLiteStore) GetLastRead(ctx context.Context, ch domain.ChannelName) (int64, error) {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.get_last_read", attribute.String(observability.AttrChannel, string(ch)))
	defer span.End()

	var eventID int64

	err := s.db.QueryRowContext(ctx,
		`SELECT event_id FROM last_read WHERE channel = ?`, ch).Scan(&eventID)
	if err == sql.ErrNoRows {
		recordSQLiteSuccess(span)
		return 0, nil
	}

	if err != nil {
		recordSQLiteError(span, err)
		return 0, err
	}

	recordSQLiteSuccess(span)
	return eventID, nil
}

// SetLastRead implements Store.
func (s *SQLiteStore) SetLastRead(ctx context.Context, ch domain.ChannelName, eventID int64) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.set_last_read", attribute.String(observability.AttrChannel, string(ch)))
	defer span.End()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO last_read (channel, event_id) VALUES (?, ?)
		 ON CONFLICT (channel) DO UPDATE SET event_id = excluded.event_id`,
		ch, eventID)
	if err != nil {
		recordSQLiteError(span, err)
		return err
	}

	recordSQLiteSuccess(span)
	return nil
}

// ReadMemories implements Store.
func (s *SQLiteStore) ReadMemories(ctx context.Context, nick domain.Nick) ([]MemoryEntry, error) {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.read_memories", attribute.String(observability.AttrNick, string(nick)))
	defer span.End()

	rows, err := s.db.QueryContext(ctx,
		`SELECT key, content FROM memories WHERE nick = ? ORDER BY key`, nick)
	if err != nil {
		recordSQLiteError(span, err)
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var entries []MemoryEntry

	for rows.Next() {
		var e MemoryEntry
		if err := rows.Scan(&e.Key, &e.Content); err != nil {
			recordSQLiteError(span, err)
			return nil, err
		}

		entries = append(entries, e)
	}

	if err := rows.Err(); err != nil {
		recordSQLiteError(span, err)
		return nil, err
	}

	recordSQLiteSuccess(span)
	return entries, nil
}

// WriteMemory implements Store.
func (s *SQLiteStore) WriteMemory(ctx context.Context, nick domain.Nick, key, content string) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.write_memory", attribute.String(observability.AttrNick, string(nick)))
	defer span.End()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO memories (nick, key, content) VALUES (?, ?, ?)
		 ON CONFLICT (nick, key) DO UPDATE SET content = excluded.content`,
		nick, key, content)

	if err != nil {
		recordSQLiteError(span, err)
		return err
	}

	recordSQLiteSuccess(span)
	return nil
}

// DeleteMemory implements Store.
func (s *SQLiteStore) DeleteMemory(ctx context.Context, nick domain.Nick, key string) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.delete_memory", attribute.String(observability.AttrNick, string(nick)))
	defer span.End()

	_, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE nick = ? AND key = ?`, nick, key)
	if err != nil {
		recordSQLiteError(span, err)
		return err
	}

	recordSQLiteSuccess(span)
	return nil
}

// ListPersonas implements Store.
func (s *SQLiteStore) ListPersonas(ctx context.Context) ([]domain.Persona, error) {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.list_personas")
	defer span.End()

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, description, origin FROM personas ORDER BY id`)
	if err != nil {
		recordSQLiteError(span, err)
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var personas []domain.Persona

	for rows.Next() {
		var p domain.Persona
		if err := rows.Scan(&p.ID, &p.Description, &p.Origin); err != nil {
			recordSQLiteError(span, err)
			return nil, err
		}

		personas = append(personas, p)
	}

	if err := rows.Err(); err != nil {
		recordSQLiteError(span, err)
		return nil, err
	}

	recordSQLiteSuccess(span)
	return personas, nil
}

// GetPersona implements Store.
func (s *SQLiteStore) GetPersona(ctx context.Context, id string) (domain.Persona, error) {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.get_persona", attribute.String("persona.id", id))
	defer span.End()

	var p domain.Persona

	err := s.db.QueryRowContext(ctx,
		`SELECT id, description, origin FROM personas WHERE id = ?`, id).
		Scan(&p.ID, &p.Description, &p.Origin)
	if err != nil {
		recordSQLiteError(span, err)
		return domain.Persona{}, fmt.Errorf("persona %q: %w", id, err)
	}

	recordSQLiteSuccess(span)
	return p, nil
}

// SavePersona implements Store.
func (s *SQLiteStore) SavePersona(ctx context.Context, p domain.Persona) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.save_persona", attribute.String("persona.id", p.ID))
	defer span.End()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO personas (id, description, origin) VALUES (?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET description = excluded.description, origin = excluded.origin`,
		p.ID, p.Description, p.Origin)
	if err != nil {
		recordSQLiteError(span, err)
		return err
	}

	recordSQLiteSuccess(span)
	return nil
}

// DeletePersonasByOrigin implements Store.
func (s *SQLiteStore) DeletePersonasByOrigin(ctx context.Context, origin domain.PersonaOrigin) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.delete_personas_by_origin", attribute.String("persona.origin", string(origin)))
	defer span.End()

	_, err := s.db.ExecContext(ctx, `DELETE FROM personas WHERE origin = ?`, origin)
	if err != nil {
		recordSQLiteError(span, err)
		return err
	}

	recordSQLiteSuccess(span)
	return nil
}

// ReplaceGeneratedPersonas implements Store. It atomically deletes all
// generated personas and inserts the given replacements in a single
// transaction.
func (s *SQLiteStore) ReplaceGeneratedPersonas(ctx context.Context, personas []domain.Persona) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.replace_generated_personas")
	defer span.End()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		recordSQLiteError(span, err)
		return fmt.Errorf("begin tx: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM personas WHERE origin = ?`, domain.PersonaGenerated); err != nil {
		recordSQLiteError(span, err)
		return fmt.Errorf("delete generated: %w", err)
	}

	for _, p := range personas {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO personas (id, description, origin) VALUES (?, ?, ?)
			 ON CONFLICT (id) DO UPDATE SET description = excluded.description, origin = excluded.origin`,
			p.ID, p.Description, p.Origin); err != nil {
			recordSQLiteError(span, err)
			return fmt.Errorf("insert persona %q: %w", p.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		recordSQLiteError(span, err)
		return fmt.Errorf("commit: %w", err)
	}

	recordSQLiteSuccess(span)
	return nil
}

// ResetMemories implements Store.
func (s *SQLiteStore) ResetMemories(ctx context.Context) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.reset_memories")
	defer span.End()

	_, err := s.db.ExecContext(ctx, `DELETE FROM memories`)
	if err != nil {
		recordSQLiteError(span, err)
		return err
	}

	recordSQLiteSuccess(span)
	return nil
}

// SavePendingQuit implements Store.
func (s *SQLiteStore) SavePendingQuit(ctx context.Context, pq domain.PendingQuit) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.save_pending_quit")
	defer span.End()

	data, err := json.Marshal(pq)
	if err != nil {
		recordSQLiteError(span, err)
		return fmt.Errorf("marshal pending quit: %w", err)
	}

	if err := setState(ctx, s.db, "pending_quit", string(data)); err != nil {
		recordSQLiteError(span, err)
		return err
	}

	recordSQLiteSuccess(span)
	return nil
}

// GetPendingQuit implements Store. Returns nil if no pending quit
// exists.
func (s *SQLiteStore) GetPendingQuit(ctx context.Context) (*domain.PendingQuit, error) {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.get_pending_quit")
	defer span.End()

	raw, err := getState[string](ctx, s.db, "pending_quit")
	if err != nil {
		recordSQLiteError(span, err)
		return nil, err
	}

	if raw == "" {
		recordSQLiteSuccess(span)
		return nil, nil
	}

	var pq domain.PendingQuit
	if err := json.Unmarshal([]byte(raw), &pq); err != nil {
		recordSQLiteError(span, err)
		return nil, fmt.Errorf("unmarshal pending quit: %w", err)
	}

	recordSQLiteSuccess(span)
	return &pq, nil
}

// ClearPendingQuit implements Store.
func (s *SQLiteStore) ClearPendingQuit(ctx context.Context) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.clear_pending_quit")
	defer span.End()

	_, err := s.db.ExecContext(ctx, `DELETE FROM state WHERE key = ?`, "pending_quit")
	if err != nil {
		recordSQLiteError(span, err)
		return err
	}

	recordSQLiteSuccess(span)
	return nil
}

// ListAutojoinChannels implements Store.
func (s *SQLiteStore) ListAutojoinChannels(ctx context.Context) ([]domain.ChannelName, error) {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.list_autojoin_channels")
	defer span.End()

	rows, err := s.db.QueryContext(ctx, `SELECT name FROM autojoin ORDER BY name`)
	if err != nil {
		recordSQLiteError(span, err)
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var channels []domain.ChannelName

	for rows.Next() {
		var name domain.ChannelName
		if err := rows.Scan(&name); err != nil {
			recordSQLiteError(span, err)
			return nil, err
		}

		channels = append(channels, name)
	}

	if err := rows.Err(); err != nil {
		recordSQLiteError(span, err)
		return nil, err
	}

	recordSQLiteSuccess(span)
	return channels, nil
}

// SetAutojoinChannels implements Store.
func (s *SQLiteStore) SetAutojoinChannels(ctx context.Context, channels []domain.ChannelName) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.set_autojoin_channels")
	defer span.End()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		recordSQLiteError(span, err)
		return fmt.Errorf("begin tx: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM autojoin`); err != nil {
		recordSQLiteError(span, err)
		return fmt.Errorf("clear autojoin: %w", err)
	}

	for _, ch := range channels {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO autojoin (name) VALUES (?)`, ch); err != nil {
			recordSQLiteError(span, err)
			return fmt.Errorf("insert autojoin %q: %w", ch, err)
		}
	}

	if err := tx.Commit(); err != nil {
		recordSQLiteError(span, err)
		return fmt.Errorf("commit: %w", err)
	}

	recordSQLiteSuccess(span)
	return nil
}

// Reset implements Store.
func (s *SQLiteStore) Reset(ctx context.Context) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.reset")
	defer span.End()

	// Order: children before parents (last_read → channels, events).
	for _, stmt := range []string{
		`DELETE FROM last_read`,
		`DELETE FROM channels`,
		`DELETE FROM events`,
		`DELETE FROM instances`,
		`DELETE FROM memories`,
		`DELETE FROM personas`,
		`DELETE FROM state`,
		`DELETE FROM autojoin`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			recordSQLiteError(span, err)
			return fmt.Errorf("reset: %w", err)
		}
	}

	recordSQLiteSuccess(span)
	return nil
}

func startSQLiteSpan(ctx context.Context, operation string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	tracer := otel.Tracer("github.com/laney/modeloff/internal/store")
	attrs = append(attrs, attribute.String(observability.AttrOperation, operation))
	ctx, span := tracer.Start(ctx, operation)
	span.SetAttributes(attrs...)

	return ctx, span
}

func recordSQLiteSuccess(span trace.Span) {
	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
}

func recordSQLiteError(span trace.Span, err error) {
	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
	span.SetStatus(codes.Error, err.Error())
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
