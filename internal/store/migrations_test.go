package store

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

// v1DatabaseSchema is byte-identical to the pre-v2 `schema` const
// (the on-disk shape of every database written before dm_instance_id,
// idx_events_dm_thread, and dm_windows existed), frozen here as its
// own literal so the test keeps exercising a genuine v1 database even
// if `schema`'s own definition changes later. `schema` today only
// describes v1, so the two happen to agree now, but pinning the
// migration test to this literal means it never silently drifts into
// testing an already-upgraded database under the "v1" label.
const v1DatabaseSchema = `
CREATE TABLE channels (
    name TEXT PRIMARY KEY,
    data TEXT NOT NULL
);

CREATE TABLE events (
    id      INTEGER PRIMARY KEY,
    channel TEXT NOT NULL,
    type    TEXT NOT NULL,
    data    TEXT NOT NULL,
    at      TEXT NOT NULL
);

CREATE INDEX idx_events_channel_id
    ON events (channel, id);

CREATE TABLE instance_replies (
    id          INTEGER PRIMARY KEY,
    instance_id TEXT NOT NULL,
    type        TEXT NOT NULL,
    data        TEXT NOT NULL,
    at          TEXT NOT NULL
);

CREATE INDEX idx_instance_replies_instance_id
    ON instance_replies (instance_id, id);

CREATE TABLE instances (
    instance_id TEXT PRIMARY KEY,
    nick        TEXT NOT NULL,
    data        TEXT NOT NULL
);

CREATE INDEX idx_instances_nick
    ON instances (nick);

CREATE TABLE memories (
    instance_id TEXT NOT NULL,
    key         TEXT NOT NULL,
    content     TEXT NOT NULL,
    PRIMARY KEY (instance_id, key)
);

CREATE TABLE personas (
    id          TEXT PRIMARY KEY,
    description TEXT NOT NULL,
    origin      TEXT NOT NULL
);

CREATE TABLE state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE autojoin (
    name TEXT PRIMARY KEY
);

CREATE TABLE last_read (
    channel  TEXT PRIMARY KEY REFERENCES channels(name) ON DELETE CASCADE,
    event_id INTEGER NOT NULL REFERENCES events(id)
);

INSERT OR IGNORE INTO state (key, value) VALUES ('schema_version', '1');
`

// seedV1Database execs v1DatabaseSchema against db and inserts one
// DM message row shaped exactly as a v1 database would already hold
// it: a botty→user message, stored under the empty-string channel
// (the user's side of the routing key — see Message.RoutingKey),
// sender identified only through the JSON body's instance_id, since
// dm_instance_id does not exist yet at v1.
func seedV1Database(t *testing.T, db *sql.DB) {
	t.Helper()

	ctx := t.Context()

	_, err := db.ExecContext(ctx, v1DatabaseSchema)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx,
		`INSERT INTO events (channel, type, data, at) VALUES (?, ?, ?, ?)`,
		"", "message",
		`{"type":"message","data":{"channel":"","from":"botty","instance_id":"inst-botty","body":"hi","at":"2025-01-15T10:30:00Z"}}`,
		"2025-01-15T10:30:00Z",
	)
	require.NoError(t, err)
}

func TestApplyMigrations_fresh_database_records_current_version(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	db.SetMaxOpenConns(1)

	_, err = db.ExecContext(ctx, schema)
	require.NoError(t, err)

	require.NoError(t, applyMigrations(ctx, db))

	got, err := readSchemaVersion(ctx, db)
	require.NoError(t, err)
	require.Equal(t, SchemaVersion, got)
}

func TestApplyMigrations_at_current_is_noop(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	db.SetMaxOpenConns(1)

	_, err = db.ExecContext(ctx, schema)
	require.NoError(t, err)

	require.NoError(t, applyMigrations(ctx, db))
	require.NoError(t, applyMigrations(ctx, db))

	got, err := readSchemaVersion(ctx, db)
	require.NoError(t, err)
	require.Equal(t, SchemaVersion, got)
}

func TestApplyMigrations_newer_database_fails_loud(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	db.SetMaxOpenConns(1)

	_, err = db.ExecContext(ctx, schema)
	require.NoError(t, err)

	// Pretend the database was last touched by a future build.
	_, err = db.ExecContext(ctx,
		`INSERT OR REPLACE INTO state (key, value) VALUES ('schema_version', ?)`,
		"999",
	)
	require.NoError(t, err)

	err = applyMigrations(ctx, db)
	require.ErrorContains(t, err, "downgrades aren't supported")
}

func TestApplyMigrations_older_database_without_migration_fails_loud(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	db.SetMaxOpenConns(1)

	_, err = db.ExecContext(ctx, schema)
	require.NoError(t, err)

	// Pretend the database was last touched by a pre-versioning
	// build that left no `schema_version` row, then never seeded
	// by the current `schema` exec (the INSERT OR IGNORE that
	// seeds the row on fresh databases). Reads it as v0; no
	// registered migration brings v0 forward, so it's an
	// unrunnable older database.
	_, err = db.ExecContext(ctx, `DELETE FROM state WHERE key = 'schema_version'`)
	require.NoError(t, err)

	err = applyMigrations(ctx, db)
	require.ErrorContains(t, err, "no migration to reach")
}

// TestApplyMigrations_v1_to_v2_adds_dm_thread_support recreates the
// exact v1 shape — no `dm_instance_id` generated column, no
// `dm_windows` table — the way a database written before this
// change actually looks on disk, then checks the v2 migration
// upgrades it in place without touching pre-existing rows.
func TestApplyMigrations_v1_to_v2_adds_dm_thread_support(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	db.SetMaxOpenConns(1)

	seedV1Database(t, db)

	require.NoError(t, applyMigrations(ctx, db))

	got, err := readSchemaVersion(ctx, db)
	require.NoError(t, err)
	require.Equal(t, SchemaVersion, got)

	// The generated column derives the sender's instance id from the
	// pre-existing row's JSON body without a rewrite of the row.
	var dmInstanceID string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT dm_instance_id FROM events WHERE channel = ''`,
	).Scan(&dmInstanceID))
	require.Equal(t, "inst-botty", dmInstanceID)

	// dm_windows exists and accepts a row referencing an instance
	// created under the pre-existing `instances` table.
	_, err = db.ExecContext(ctx,
		`INSERT INTO instances (instance_id, nick, data) VALUES ('inst-botty', 'botty', '{}')`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO dm_windows (instance_id) VALUES ('inst-botty')`)
	require.NoError(t, err)
}

// TestApplyMigrations_v1_to_v3_adds_casemapped_indexes pins the v3
// step: the nick and channel-name lookups compare under NOCASE, and
// a NOCASE comparison cannot use the BINARY index either column
// already carries, so each gains an index on the folded form.
func TestApplyMigrations_v1_to_v3_adds_casemapped_indexes(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	db.SetMaxOpenConns(1)

	seedV1Database(t, db)

	require.NoError(t, applyMigrations(ctx, db))

	rows, err := db.QueryContext(ctx,
		`SELECT name FROM sqlite_master
		 WHERE type = 'index' AND name IN ('idx_instances_nick_nocase', 'idx_channels_name_nocase')
		 ORDER BY name`)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rows.Close() })

	var indexes []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		indexes = append(indexes, name)
	}
	require.NoError(t, rows.Err())

	require.Equal(t, []string{"idx_channels_name_nocase", "idx_instances_nick_nocase"}, indexes)

	// A database written before the casemapping existed may hold two
	// rows whose names differ only in case. Neither index is UNIQUE,
	// so the migration runs against such a database and upgrades it.
	_, err = db.ExecContext(ctx,
		`INSERT INTO channels (name, data) VALUES ('#Dev', '{}'), ('#dev', '{}')`)
	require.NoError(t, err)
}

// TestApplyMigrations_v1_to_v4_adds_dm_last_read pins the v4 step:
// dm_last_read exists after the migration and accepts a row keyed to
// an instance created under the pre-existing `instances` table, the
// same shape TestApplyMigrations_v1_to_v2_adds_dm_thread_support
// checks for dm_windows.
func TestApplyMigrations_v1_to_v4_adds_dm_last_read(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite3", SQLitePragmaDSN(":memory:"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	db.SetMaxOpenConns(1)

	seedV1Database(t, db)

	require.NoError(t, applyMigrations(ctx, db))

	got, err := readSchemaVersion(ctx, db)
	require.NoError(t, err)
	require.Equal(t, SchemaVersion, got)

	_, err = db.ExecContext(ctx,
		`INSERT INTO instances (instance_id, nick, data) VALUES ('inst-botty', 'botty', '{}')`)
	require.NoError(t, err)

	// seedV1Database already inserted event id 1, which
	// dm_last_read.event_id can reference.
	_, err = db.ExecContext(ctx,
		`INSERT INTO dm_last_read (instance_id, event_id) VALUES ('inst-botty', 1)`)
	require.NoError(t, err)

	// dm_last_read.instance_id enforces the same referential
	// integrity dm_windows does: a cursor against an instance the
	// database has never seen is refused.
	_, err = db.ExecContext(ctx,
		`INSERT INTO dm_last_read (instance_id, event_id) VALUES ('inst-ghost', 1)`)
	require.Error(t, err)
}

// TestNewSQLiteStore_opens_existing_v1_database is the regression
// test for the schema/migration ordering bug this package shipped
// once already: NewSQLiteStore execs `schema` unconditionally before
// running migrations, so if `schema` ever describes more than the v1
// shape, opening a genuine pre-existing v1 database fails outright
// (`schema`'s `CREATE INDEX` on a v2+ column errors "no such column"
// against a table `CREATE TABLE IF NOT EXISTS` left untouched at v1)
// — before applyMigrations ever gets a chance to add that column.
// Unlike TestApplyMigrations_v1_to_v2_adds_dm_thread_support, which
// calls applyMigrations directly and so cannot see this class of
// bug, this test goes through the real NewSQLiteStore entry point —
// the same path NewDefaultSQLiteStore uses against a user's actual
// on-disk database.
func TestNewSQLiteStore_opens_existing_v1_database(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite3", SQLitePragmaDSN(":memory:"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	db.SetMaxOpenConns(1)

	seedV1Database(t, db)

	s, err := NewSQLiteStore(ctx, db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	got, err := readSchemaVersion(ctx, db)
	require.NoError(t, err)
	require.Equal(t, SchemaVersion, got)

	var indexName string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_events_dm_thread'`,
	).Scan(&indexName))
	require.Equal(t, "idx_events_dm_thread", indexName)

	// The pre-existing DM message row survived the upgrade and its
	// generated column resolves. The assertion reads it back through
	// the store's own DMEventsBefore, exercising the real query path
	// a live DM read depends on.
	events, err := s.DMEventsBefore(ctx, "", "inst-botty", nil, 10)
	require.NoError(t, err)
	require.Equal(t, []domain.StoredEvent{
		{
			ID: 1,
			Event: domain.Message{
				Target:     "",
				From:       "botty",
				InstanceID: "inst-botty",
				Body:       "hi",
				At:         time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC),
			},
		},
	}, events)
}

// seedV3Database brings db to a genuine v3 shape by running the v2
// and v3 migration steps directly against a v1 database, stopping
// short of v4, and recording the version as 3. This is what a real
// pre-existing v3 database looks like on disk: the shape
// TestSQLiteStore_Reset (and every other test in this package)
// exercised before dm_last_read existed, and the shape
// NewSQLiteStore has to upgrade cleanly from.
func seedV3Database(t *testing.T, db *sql.DB) {
	t.Helper()

	ctx := t.Context()

	seedV1Database(t, db)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })

	for _, m := range migrations {
		if m.Version > 3 {
			continue
		}
		require.NoError(t, m.Apply(ctx, tx))
	}

	_, err = tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO state (key, value) VALUES ('schema_version', '3')`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

// TestNewSQLiteStore_opens_existing_v3_database is the regression
// test for the DM read-cursor blocker: before dm_last_read existed,
// a DM's read cursor could never be recorded, because
// last_read.channel references channels(name) and a DM window is
// never a row there. It goes through the real NewSQLiteStore entry
// point against a genuine v3 database, the same path
// NewDefaultSQLiteStore uses against a user's actual on-disk
// database, and then exercises the fix through the store's public
// API rather than by inspecting the schema directly.
func TestNewSQLiteStore_opens_existing_v3_database(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite3", SQLitePragmaDSN(":memory:"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	db.SetMaxOpenConns(1)

	seedV3Database(t, db)

	// A pre-existing instance row, the way a real v3 database already
	// holds one for any model that has ever joined.
	_, err = db.ExecContext(ctx,
		`INSERT INTO instances (instance_id, nick, data) VALUES ('inst-botty', 'botty', '{}')`)
	require.NoError(t, err)

	s, err := NewSQLiteStore(ctx, db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	got, err := readSchemaVersion(ctx, db)
	require.NoError(t, err)
	require.Equal(t, SchemaVersion, got)

	// seedV1Database already inserted event id 1; SetDMLastRead can
	// reference it. This is the write that used to fail on
	// last_read's foreign key.
	require.NoError(t, s.SetDMLastRead(ctx, "inst-botty", 1))

	dmLastRead, err := s.GetDMLastRead(ctx, "inst-botty")
	require.NoError(t, err)
	require.Equal(t, int64(1), dmLastRead)
}

func TestReadSchemaVersion_absent_row_returns_zero(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	db.SetMaxOpenConns(1)

	_, err = db.ExecContext(ctx,
		`CREATE TABLE state (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	)
	require.NoError(t, err)

	got, err := readSchemaVersion(ctx, db)
	require.NoError(t, err)
	require.Equal(t, 0, got)
}
