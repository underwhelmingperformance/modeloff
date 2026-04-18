package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
    instance_id TEXT PRIMARY KEY,
    nick        TEXT NOT NULL,
    data        TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_instances_nick
    ON instances (nick);

CREATE TABLE IF NOT EXISTS memories (
    instance_id TEXT NOT NULL,
    key         TEXT NOT NULL,
    content     TEXT NOT NULL,
    PRIMARY KEY (instance_id, key)
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

// SQLiteStore implements Store using a single SQLite database. It
// also owns the canonical `*domain.Instance` handle per InstanceID:
// the `instances` field caches every instance ever loaded or saved
// through this store, so callers see pointer-stable handles across
// calls. The registry is invalidated on `DeleteInstanceByID` and
// `Reset`.
type SQLiteStore struct {
	db *sql.DB

	instancesMu sync.RWMutex
	instances   map[domain.InstanceID]*domain.Instance
}

// SQLitePragmaDSN appends the connection-time PRAGMAs that the store
// requires (`busy_timeout`, `journal_mode`, `foreign_keys`) to the
// given filename or `file:` URI. The `ncruces/go-sqlite3` driver
// applies `_pragma=` parameters on every connection it opens, so any
// pool size sees the same configuration. Order matters per the driver
// docs: `busy_timeout` and the locking mode must come first.
func SQLitePragmaDSN(path string) string {
	dsn := path
	if !strings.HasPrefix(dsn, "file:") {
		dsn = "file:" + dsn
	}

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}

	return dsn + sep + "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)"
}

// NewDefaultSQLiteStore creates a SQLiteStore using the XDG data
// directory ($XDG_DATA_HOME/modeloff/modeloff.db).
func NewDefaultSQLiteStore(ctx context.Context) (*SQLiteStore, error) {
	dir := filepath.Join(xdg.DataHome, "modeloff")

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	db, err := sql.Open("sqlite3", SQLitePragmaDSN(filepath.Join(dir, "modeloff.db")))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	return NewSQLiteStore(ctx, db)
}

// NewSQLiteStore creates a store backed by the given database. The
// caller is responsible for opening the database with a DSN that
// configures the required connection-time PRAGMAs (`busy_timeout`,
// `journal_mode`, `foreign_keys`); `SQLitePragmaDSN` builds one. The
// schema is created if it does not already exist. The context is used
// for migration logging so that any surrounding trace is correlated
// with the startup-time log line.
func NewSQLiteStore(ctx context.Context, db *sql.DB) (*SQLiteStore, error) {
	// Read the resulting journal mode for operator visibility — an
	// on-disk database normally reports `wal` here, but a `:memory:`
	// database reports `memory` because there is no file to journal,
	// and a filesystem that cannot host WAL falls back to `delete`.
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		return nil, fmt.Errorf("read journal_mode: %w", err)
	}

	slog.Default().InfoContext(ctx, "sqlite journal mode",
		"component", "store.sqlite",
		"mode", journalMode,
	)

	// Pre-release schema v2 migration: the `instances` and `memories`
	// tables used to be keyed by nick; they are now keyed by
	// InstanceID. Nicks are display state that can drift during a
	// session, so identity-keyed storage is the correct long-term
	// shape. The project contract is that pre-release schema changes
	// drop the affected rows rather than migrate them (same pattern as
	// the `pending_quit` purge further down, and the membership rekey
	// in the channels table). If the legacy `instances.nick` primary
	// key is present, drop both tables so the fresh schema creation
	// below produces the v2 shape.
	if err := dropLegacyInstanceTables(ctx, db); err != nil {
		return nil, fmt.Errorf("drop legacy instance tables: %w", err)
	}

	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("create schema: %w", err)
	}

	// One-off migration: pending_quit was the deferred-shutdown
	// mechanism replaced by Session.Quit's synchronous path. Existing
	// rows are stale.
	result, err := db.Exec(`DELETE FROM state WHERE key = 'pending_quit'`)
	if err != nil {
		return nil, fmt.Errorf("purge legacy pending_quit row: %w", err)
	}

	if rows, err := result.RowsAffected(); err == nil && rows > 0 {
		slog.Default().InfoContext(ctx, "purged legacy pending_quit row", "component", "store.sqlite", "rows", rows)
	}

	return &SQLiteStore{
		db:        db,
		instances: make(map[domain.InstanceID]*domain.Instance),
	}, nil
}

// canonicaliseInstance returns the canonical `*domain.Instance` for
// the given id. On cache miss the freshly-loaded handle is inserted
// and returned. On cache hit the existing handle is returned
// untouched — the session is authoritative for the live nick,
// persona, and channels of every registered instance; the store row
// is a save-time snapshot and refreshing the cached handle from it
// would clobber unrelated in-flight mutations on the session side.
//
// Callers needing the on-disk row's display state must treat the
// returned handle's getters as authoritative and accept that the
// row may be staler.
func (s *SQLiteStore) canonicaliseInstance(fresh *domain.Instance) *domain.Instance {
	if fresh == nil {
		return nil
	}

	s.instancesMu.Lock()
	defer s.instancesMu.Unlock()

	if existing, ok := s.instances[fresh.ID()]; ok {
		return existing
	}

	s.instances[fresh.ID()] = fresh
	return fresh
}

// forgetInstance evicts an instance from the canonical registry.
// Subsequent loads that produce an Instance with the same id will
// return a fresh pointer — callers that held the old pointer see
// a stale handle, which is the correct semantic for a deleted
// instance.
func (s *SQLiteStore) forgetInstance(id domain.InstanceID) {
	s.instancesMu.Lock()
	delete(s.instances, id)
	s.instancesMu.Unlock()
}

// resolveInstance looks up the canonical handle for an id without
// touching the database. Returns nil if the id is not registered.
// Used by channel deserialisation to rewrite member-list stubs.
func (s *SQLiteStore) resolveInstance(id domain.InstanceID) *domain.Instance {
	s.instancesMu.RLock()
	defer s.instancesMu.RUnlock()

	return s.instances[id]
}

// dropLegacyInstanceTables detects the nick-keyed v1 shape of the
// `instances` table (no `instance_id` column) and, if found, drops
// both `instances` and `memories` so the fresh v2 schema can be
// created. The legacy rows are not migrated: this is a pre-release
// reset, not a data-preserving migration.
//
// Detection is v1→v2 specific; any future schema change requires its
// own detector.
func dropLegacyInstanceTables(ctx context.Context, db *sql.DB) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.migrate_v2")
	defer span.End()

	rows, err := db.QueryContext(ctx, `PRAGMA table_info(instances)`)
	if err != nil {
		recordSQLiteError(span, err)
		return fmt.Errorf("inspect instances schema: %w", err)
	}

	var (
		hasInstancesTable  bool
		hasInstanceIDField bool
	)

	for rows.Next() {
		hasInstancesTable = true

		var (
			cid     int
			name    string
			colType string
			notNull int
			dflt    sql.NullString
			pk      int
		)

		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			_ = rows.Close()
			recordSQLiteError(span, err)
			return fmt.Errorf("scan column info: %w", err)
		}

		if name == "instance_id" {
			hasInstanceIDField = true
		}
	}

	if err := rows.Err(); err != nil {
		_ = rows.Close()
		recordSQLiteError(span, err)
		return fmt.Errorf("iterate column info: %w", err)
	}

	if err := rows.Close(); err != nil {
		recordSQLiteError(span, err)
		return fmt.Errorf("close column info: %w", err)
	}

	detected := hasInstancesTable && !hasInstanceIDField
	span.SetAttributes(attribute.Bool("modeloff.migration.detected", detected))

	if !detected {
		recordSQLiteSuccess(span)
		return nil
	}

	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS instances`); err != nil {
		recordSQLiteError(span, err)
		return fmt.Errorf("drop legacy instances: %w", err)
	}

	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS memories`); err != nil {
		recordSQLiteError(span, err)
		return fmt.Errorf("drop legacy memories: %w", err)
	}

	slog.Default().WarnContext(ctx,
		"modeloff store schema v2 applied; legacy instances/memories tables dropped",
		"component", "store.sqlite",
		"reason", "legacy nick-keyed instances table detected",
	)

	recordSQLiteSuccess(span)
	return nil
}

// Close closes the underlying database connection.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// ListChannels implements Store. Returned channels have member-lists
// carrying canonical `*Instance` handles; stub references to deleted
// instances are dropped and logged.
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

	for i := range channels {
		if err := s.resolveChannelMembers(ctx, &channels[i]); err != nil {
			recordSQLiteError(span, err)
			return nil, err
		}
	}

	recordSQLiteSuccess(span)
	return channels, nil
}

// GetChannel implements Store. Returns a Channel whose member list
// carries canonical `*Instance` handles; stub references to deleted
// instances are dropped and logged.
func (s *SQLiteStore) GetChannel(ctx context.Context, name domain.ChannelName) (domain.Channel, error) {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.get_channel", attribute.String(observability.AttrChannel, string(name)))
	defer span.End()

	var data string

	err := s.db.QueryRowContext(ctx, `SELECT data FROM channels WHERE name = ?`, name).Scan(&data)
	if err != nil {
		kind := observability.ErrorKindStore
		if err == sql.ErrNoRows {
			kind = observability.ErrorKindNotFound
		}
		recordSQLiteErrorKind(span, err, kind)
		return domain.Channel{}, fmt.Errorf("channel %q: %w", name, err)
	}

	var ch domain.Channel
	if err := json.Unmarshal([]byte(data), &ch); err != nil {
		recordSQLiteError(span, err)
		return domain.Channel{}, err
	}

	if err := s.resolveChannelMembers(ctx, &ch); err != nil {
		recordSQLiteError(span, err)
		return domain.Channel{}, err
	}

	recordSQLiteSuccess(span)
	return ch, nil
}

// resolveChannelMembers rewrites the stub `*Instance` handles in
// the channel's member list (set by MemberList.UnmarshalJSON) to
// canonical pointers from the registry, loading any missing
// instances from SQLite in a single batch. Member rows that refer
// to an instance with no backing row are dropped from the list and
// logged — a leftover from a previous session where the instance
// was deleted but the channel's membership record still carried
// the id.
func (s *SQLiteStore) resolveChannelMembers(ctx context.Context, ch *domain.Channel) error {
	if ch.Members.Len() == 0 {
		return nil
	}

	// Gather the ids carried by stubs that aren't already in the
	// registry. The human user's instance (empty id) is ignored
	// because the session constructs it on its own at Connect time
	// and seeds the registry before loading channels.
	var missing []domain.InstanceID
	seen := make(map[domain.InstanceID]struct{})

	for _, m := range ch.Members.All() {
		id := m.Instance.ID()

		if _, ok := seen[id]; ok {
			continue
		}

		seen[id] = struct{}{}

		if id == "" {
			continue
		}

		if s.resolveInstance(id) != nil {
			continue
		}

		missing = append(missing, id)
	}

	if len(missing) > 0 {
		if err := s.loadInstancesByID(ctx, missing); err != nil {
			return fmt.Errorf("load channel members: %w", err)
		}
	}

	// Rewrite stubs; any id that still resolves to nil references a
	// deleted instance and is dropped.
	dropped := make([]domain.InstanceID, 0)
	ch.Members.ResolveInstances(func(id domain.InstanceID) *domain.Instance {
		canonical := s.resolveInstance(id)
		if canonical == nil {
			dropped = append(dropped, id)
		}

		return canonical
	})

	if len(dropped) > 0 {
		slog.Default().WarnContext(ctx,
			"channel members have no backing instance; dropped",
			"component", "store.sqlite",
			"channel", ch.Name,
			"dropped_ids", dropped,
			"count", len(dropped),
		)
	}

	return nil
}

// loadInstancesByID reads the given ids from the `instances` table
// in a single query and registers them in the canonical registry.
// Ids that don't resolve are silently ignored — the caller detects
// the miss via a second `resolveInstance` lookup.
func (s *SQLiteStore) loadInstancesByID(ctx context.Context, ids []domain.InstanceID) error {
	if len(ids) == 0 {
		return nil
	}

	// Pass the id list as a JSON array bound to a single parameter
	// and let SQLite expand it via `json_each`. This avoids building
	// an IN (?, ?, …) list at the string level while still binding
	// the id values through a prepared-statement parameter.
	idsJSON, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("marshal ids: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT data FROM instances
		WHERE instance_id IN (SELECT value FROM json_each(?))
	`, string(idsJSON))
	if err != nil {
		return fmt.Errorf("query instances: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return fmt.Errorf("scan instance row: %w", err)
		}

		fresh := &domain.Instance{}
		if err := json.Unmarshal([]byte(data), fresh); err != nil {
			return fmt.Errorf("unmarshal instance: %w", err)
		}

		s.canonicaliseInstance(fresh)
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate instance rows: %w", err)
	}

	return nil
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

// ListInstances implements Store. Returns canonical `*Instance`
// pointers from the registry; callers that called `GetInstanceByID`
// previously observe the same pointers.
func (s *SQLiteStore) ListInstances(ctx context.Context) ([]*domain.Instance, error) {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.list_instances")
	defer span.End()

	rows, err := s.db.QueryContext(ctx, `SELECT data FROM instances ORDER BY nick`)
	if err != nil {
		recordSQLiteError(span, err)
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var instances []*domain.Instance

	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			recordSQLiteError(span, err)
			return nil, err
		}

		fresh := &domain.Instance{}
		if err := json.Unmarshal([]byte(data), fresh); err != nil {
			recordSQLiteError(span, err)
			return nil, err
		}

		instances = append(instances, s.canonicaliseInstance(fresh))
	}

	if err := rows.Err(); err != nil {
		recordSQLiteError(span, err)
		return nil, err
	}

	recordSQLiteSuccess(span)
	return instances, nil
}

// GetInstanceByID implements Store. Returns the canonical
// `*Instance` pointer — two calls for the same id return the same
// handle.
func (s *SQLiteStore) GetInstanceByID(ctx context.Context, id domain.InstanceID) (*domain.Instance, error) {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.get_instance_by_id", attribute.String(observability.AttrInstanceID, string(id)))
	defer span.End()

	var data string

	err := s.db.QueryRowContext(ctx, `SELECT data FROM instances WHERE instance_id = ?`, string(id)).Scan(&data)
	if err != nil {
		kind := observability.ErrorKindStore
		if err == sql.ErrNoRows {
			kind = observability.ErrorKindNotFound
		}
		recordSQLiteErrorKind(span, err, kind)
		return nil, fmt.Errorf("instance %q: %w", id, err)
	}

	fresh := &domain.Instance{}
	if err := json.Unmarshal([]byte(data), fresh); err != nil {
		recordSQLiteError(span, err)
		return nil, err
	}

	recordSQLiteSuccess(span)
	return s.canonicaliseInstance(fresh), nil
}

// ResolveNick returns the canonical `*Instance` whose current
// display nick matches the argument. Identity is the stable anchor
// in this system; nicks are mutable display state. The command
// parser is the single intentional caller: it resolves user input
// into a handle once at the boundary, and every downstream call
// takes the handle.
//
// If multiple instances share the same display nick the store
// returns one arbitrary matching row — the `idx_instances_nick`
// index is non-unique because display nicks are expected to drift.
// Callers are responsible for preventing collisions upstream (the
// session refuses renames that would collide; see task #53).
func (s *SQLiteStore) ResolveNick(ctx context.Context, nick domain.Nick) (*domain.Instance, error) {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.resolve_nick", attribute.String(observability.AttrNick, string(nick)))
	defer span.End()

	var (
		id   string
		data string
	)

	err := s.db.QueryRowContext(ctx,
		`SELECT instance_id, data FROM instances WHERE nick = ?`, nick).Scan(&id, &data)
	if err == sql.ErrNoRows {
		recordSQLiteErrorKind(span, err, observability.ErrorKindNotFound)
		return nil, fmt.Errorf("resolve nick %q: %w", nick, ErrNoSuchNick)
	}

	if err != nil {
		recordSQLiteError(span, err)
		return nil, fmt.Errorf("resolve nick %q: %w", nick, err)
	}

	fresh := &domain.Instance{}
	if err := json.Unmarshal([]byte(data), fresh); err != nil {
		recordSQLiteError(span, err)
		return nil, err
	}

	recordSQLiteSuccess(span)
	return s.canonicaliseInstance(fresh), nil
}

// SaveInstance implements Store. The caller hands over the
// canonical handle; the store reads its current fields under the
// handle's read lock (via MarshalJSON) and writes them to the
// `instances` row. Registering the handle in the canonical map
// ensures a subsequent `GetInstanceByID` returns the same pointer.
func (s *SQLiteStore) SaveInstance(ctx context.Context, inst *domain.Instance) error {
	// Snapshot the nick once so the span attribute, the INSERT column
	// value, and the marshaled data blob all agree. The data blob is
	// already atomic with the marshal-time snapshot under the handle's
	// read lock; pairing the column value and span attribute with a
	// single nick read closes the divergence window.
	nick := inst.Nick()

	ctx, span := startSQLiteSpan(ctx, "store.sqlite.save_instance",
		attribute.String(observability.AttrInstanceID, string(inst.ID())),
		attribute.String(observability.AttrNick, string(nick)))
	defer span.End()

	data, err := json.Marshal(inst)
	if err != nil {
		recordSQLiteError(span, err)
		return err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO instances (instance_id, nick, data) VALUES (?, ?, ?)
		 ON CONFLICT (instance_id) DO UPDATE SET
		     nick = excluded.nick,
		     data = excluded.data`,
		string(inst.ID()), string(nick), string(data))

	if err != nil {
		recordSQLiteError(span, err)
		return err
	}

	// Register the saved handle as canonical if there isn't already
	// a registered handle for this id.
	s.instancesMu.Lock()
	if _, ok := s.instances[inst.ID()]; !ok {
		s.instances[inst.ID()] = inst
	}
	s.instancesMu.Unlock()

	recordSQLiteSuccess(span)
	return nil
}

// DeleteInstanceByID implements Store. Evicts the row from SQLite
// and the handle from the canonical registry.
func (s *SQLiteStore) DeleteInstanceByID(ctx context.Context, id domain.InstanceID) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.delete_instance_by_id", attribute.String(observability.AttrInstanceID, string(id)))
	defer span.End()

	_, err := s.db.ExecContext(ctx, `DELETE FROM instances WHERE instance_id = ?`, string(id))
	if err != nil {
		recordSQLiteError(span, err)
		return err
	}

	s.forgetInstance(id)

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
func (s *SQLiteStore) ReadMemories(ctx context.Context, id domain.InstanceID) ([]MemoryEntry, error) {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.read_memories", attribute.String(observability.AttrInstanceID, string(id)))
	defer span.End()

	rows, err := s.db.QueryContext(ctx,
		`SELECT key, content FROM memories WHERE instance_id = ? ORDER BY key`, string(id))
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
func (s *SQLiteStore) WriteMemory(ctx context.Context, id domain.InstanceID, key, content string) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.write_memory", attribute.String(observability.AttrInstanceID, string(id)))
	defer span.End()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO memories (instance_id, key, content) VALUES (?, ?, ?)
		 ON CONFLICT (instance_id, key) DO UPDATE SET content = excluded.content`,
		string(id), key, content)

	if err != nil {
		recordSQLiteError(span, err)
		return err
	}

	recordSQLiteSuccess(span)
	return nil
}

// DeleteMemory implements Store.
func (s *SQLiteStore) DeleteMemory(ctx context.Context, id domain.InstanceID, key string) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.delete_memory", attribute.String(observability.AttrInstanceID, string(id)))
	defer span.End()

	_, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE instance_id = ? AND key = ?`, string(id), key)
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
		kind := observability.ErrorKindStore
		if err == sql.ErrNoRows {
			kind = observability.ErrorKindNotFound
		}
		recordSQLiteErrorKind(span, err, kind)
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

// GetSessionActive implements Store.
func (s *SQLiteStore) GetSessionActive(ctx context.Context) (string, error) {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.get_session_active")
	defer span.End()

	value, err := getState[string](ctx, s.db, "session_active")
	if err != nil {
		recordSQLiteError(span, err)
		return "", err
	}

	recordSQLiteSuccess(span)
	return value, nil
}

// SetSessionActive implements Store.
func (s *SQLiteStore) SetSessionActive(ctx context.Context, value string) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.set_session_active")
	defer span.End()

	if err := setState(ctx, s.db, "session_active", value); err != nil {
		recordSQLiteError(span, err)
		return err
	}

	recordSQLiteSuccess(span)
	return nil
}

// ClearSessionActive implements Store.
func (s *SQLiteStore) ClearSessionActive(ctx context.Context) error {
	ctx, span := startSQLiteSpan(ctx, "store.sqlite.clear_session_active")
	defer span.End()

	_, err := s.db.ExecContext(ctx, `DELETE FROM state WHERE key = ?`, "session_active")
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		recordSQLiteError(span, err)
		return fmt.Errorf("begin tx: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

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
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			recordSQLiteError(span, err)
			return fmt.Errorf("reset: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		recordSQLiteError(span, err)
		return fmt.Errorf("commit: %w", err)
	}

	recordSQLiteSuccess(span)
	return nil
}

func startSQLiteSpan(ctx context.Context, operation string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	// TODO(#34): DI tracer; remove otel.Tracer global read.
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
	recordSQLiteErrorKind(span, err, observability.ErrorKindStore)
}

// recordSQLiteErrorKind records an error result with an explicit
// error kind. Use this at call sites where the failure is not a
// generic store error — currently only `sql.ErrNoRows`, which is
// tagged as `ErrorKindNotFound` so dashboards can separate
// missing-row outcomes from infrastructure failures.
func recordSQLiteErrorKind(span trace.Span, err error, kind string) {
	span.SetAttributes(
		attribute.String(observability.AttrResult, observability.ResultError),
		attribute.String(observability.AttrErrorKind, kind),
	)
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
