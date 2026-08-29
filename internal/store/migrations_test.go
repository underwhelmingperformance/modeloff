package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
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

type personaBackfillState struct {
	InstanceID     domain.InstanceID
	Baseline       string
	Description    string
	TemplateID     *string
	TemplateOrigin *string
	TemplateHash   *string
	ParentID       *int64
	Checkpoint     int64
	CreatedAt      string
}

func TestApplyMigrations_backfills_revision_zero_for_model_instances(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	seedV1Database(t, db)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	for _, migration := range migrations {
		if migration.Version > 13 {
			continue
		}
		require.NoError(t, migration.Apply(ctx, tx))
	}
	modelData, err := json.Marshal(domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful and curious", nil,
	))
	require.NoError(t, err)
	userData, err := json.Marshal(domain.NewUserInstance("testuser"))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO instances (instance_id, nick, data) VALUES
			('inst-botty', 'botty', ?),
			('', 'testuser', ?)
	`, string(modelData), string(userData))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO state (key, value) VALUES ('schema_version', '13')`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	require.NoError(t, applyMigrations(ctx, db))
	var got personaBackfillState
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT state.instance_id, state.baseline, revision.description,
		       state.template_id, state.template_origin, state.template_hash,
		       revision.parent_id,
		       state.checkpoint, state.created_at
		FROM persona_lineages AS state
		JOIN persona_revisions AS revision
		  ON revision.id = state.current_revision_id
	`).Scan(
		&got.InstanceID,
		&got.Baseline,
		&got.Description,
		&got.TemplateID,
		&got.TemplateOrigin,
		&got.TemplateHash,
		&got.ParentID,
		&got.Checkpoint,
		&got.CreatedAt,
	))
	// Reading the baseline alone would pass against a migration that
	// backfilled revision zero with an empty description.
	require.Equal(t, personaBackfillState{
		InstanceID:  "inst-botty",
		Baseline:    "careful and curious",
		Description: "careful and curious",
		CreatedAt:   formatTime(time.Time{}),
	}, got)
	var states int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM persona_lineages`).Scan(&states))
	require.Equal(t, 1, states)
}

func TestApplyMigrations_scopes_private_replies_by_issuing_window(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite3", SQLitePragmaDSN(":memory:"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	seedV1Database(t, db)

	_, err = db.ExecContext(ctx,
		`INSERT INTO instances (instance_id, nick, data) VALUES ('inst-botty', 'botty', '{}')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO instance_replies (instance_id, type, data, at) VALUES
		 ('inst-botty', 'list_reply', '{"type":"list_reply","data":{"channel":"#other","members":1,"at":"2025-01-15T10:30:00Z"}}', '2025-01-15T10:30:00Z'),
		 ('inst-botty', 'whois', '{"type":"whois","data":{"channel":"#dev","nick":"alice","at":"2025-01-15T10:30:01Z"}}', '2025-01-15T10:30:01Z'),
		 ('inst-botty', 'whois', '{"type":"whois","data":{"channel":"","nick":"the-user","at":"2025-01-15T10:30:02Z"}}', '2025-01-15T10:30:02Z')`)
	require.NoError(t, err)

	require.NoError(t, applyMigrations(ctx, db))

	s := &SQLiteStore{db: db, tracerProvider: otel.GetTracerProvider()}
	replies, err := s.InstanceRepliesBefore(ctx, "inst-botty", nil, 10)
	require.NoError(t, err)
	require.Equal(t, []InstanceReplyRecord{{
		ID:     2,
		Window: protocol.ChannelWindowTarget("#dev"),
		Event: domain.Whois{
			Nick: "alice",
			At:   time.Date(2025, 1, 15, 10, 30, 1, 0, time.UTC),
		},
	}}, replies)
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

// TestNewSQLiteStore_reopen_does_not_restore_a_v1_table pins that the
// `schema` constant runs for a fresh database and not again. `personas`
// is the case that exists: it is a v1 table the migration chain takes
// away, so a second open that re-ran `schema` would put it back.
func TestNewSQLiteStore_reopen_does_not_restore_a_v1_table(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite3", SQLitePragmaDSN(":memory:"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	_, err = NewSQLiteStore(ctx, db)
	require.NoError(t, err)
	_, err = NewSQLiteStore(ctx, db)
	require.NoError(t, err)

	var present bool
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'personas'
		)`).Scan(&present))

	require.False(t, present)
}

func TestApplyMigrations_v9_to_v10_adds_the_model_turn_journal(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite3", SQLitePragmaDSN(":memory:"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	_, err = db.ExecContext(ctx, schema)
	require.NoError(t, err)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	for _, migration := range migrations {
		if migration.Version >= 10 {
			break
		}
		require.NoError(t, migration.Apply(ctx, tx))
	}
	_, err = tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO state (key, value) VALUES ('schema_version', '9')`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	require.NoError(t, applyMigrations(ctx, db))

	s := &SQLiteStore{db: db, tracerProvider: otel.GetTracerProvider()}
	input := ModelTurnEntry{Kind: ModelTurnInput, Data: []byte(`{"input":"hello"}`), At: testTime}
	turnID, err := s.BeginModelTurn(ctx, ModelTurn{
		InstanceID: "inst-botty",
		Window:     protocol.ChannelWindowTarget("#dev"),
		ModelID:    "test/model",
		StartedAt:  testTime,
	}, input)
	require.NoError(t, err)

	got, err := s.ModelTurnEntries(ctx, turnID)
	require.NoError(t, err)
	require.Equal(t, []ModelTurnEntry{input}, got)

	version, err := readSchemaVersion(ctx, db)
	require.NoError(t, err)
	require.Equal(t, SchemaVersion, version)
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
	var versionErr *schemaTooNewError
	require.ErrorAs(t, err, &versionErr)
	require.Equal(t, &schemaTooNewError{Found: 999, Supported: SchemaVersion}, versionErr)
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
	var migrationErr *missingMigrationError
	require.ErrorAs(t, err, &migrationErr)
	require.Equal(t, &missingMigrationError{From: 0, To: SchemaVersion}, migrationErr)
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

	// The DM message row seedV1Database writes belongs to "inst-botty",
	// created under the pre-existing `instances` table: retention's
	// orphaned-DM pass (pruneOrphanDMEvents), which NewSQLiteStore runs
	// on open, treats a DM row naming no instance row as an orphan left
	// behind by a deleted peer, and this fixture's peer is very much
	// still around.
	_, err = db.ExecContext(ctx,
		`INSERT INTO instances (instance_id, nick, data) VALUES ('inst-botty', 'botty', '{}')`)
	require.NoError(t, err)

	s, err := NewSQLiteStore(ctx, db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	got, err := readSchemaVersion(ctx, db)
	require.NoError(t, err)
	require.Equal(t, SchemaVersion, got)

	var indexName string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_events_source_thread'`,
	).Scan(&indexName))
	require.Equal(t, "idx_events_source_thread", indexName)

	var oldIndexCount int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_events_dm_thread'`,
	).Scan(&oldIndexCount))
	require.Zero(t, oldIndexCount)

	// The pre-existing DM message row survived the upgrade and its
	// generated column resolves. The assertion reads it back through
	// the store's own DMEventsBefore, exercising the real query path
	// a live DM read depends on.
	events, err := s.DMEventsBefore(ctx, "", "inst-botty", nil, 10)
	require.NoError(t, err)
	require.Equal(t, []domain.StoredEvent{
		{
			ID: 1,
			Event: domain.Message{Source: domain.ClientSource(

				"inst-botty", "botty"), Target: "", Body: "hi", At: time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)},
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

func seedV5Database(t *testing.T, db *sql.DB) {
	t.Helper()

	ctx := t.Context()
	seedV1Database(t, db)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })

	for _, migration := range migrations {
		if migration.Version > 5 {
			continue
		}
		require.NoError(t, migration.Apply(ctx, tx))
	}

	_, err = tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO state (key, value) VALUES ('schema_version', '5')`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

func TestApplyMigrations_v5_to_v6_keeps_existing_instances_active(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	db.SetMaxOpenConns(1)
	seedV5Database(t, db)

	_, err = db.ExecContext(ctx,
		`INSERT INTO instances (instance_id, nick, data) VALUES (?, ?, ?)`,
		"inst-botty", "botty", `{}`)
	require.NoError(t, err)
	require.NoError(t, applyMigrations(ctx, db))

	var pendingDeletion int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT pending_deletion FROM instances WHERE instance_id = ?`,
		"inst-botty",
	).Scan(&pendingDeletion))
	_, err = db.ExecContext(ctx,
		`INSERT INTO pending_memory_deletions (instance_id) VALUES (?)`,
		"inst-gone")
	require.NoError(t, err)
	var pendingMemoryDeletion string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT instance_id FROM pending_memory_deletions`,
	).Scan(&pendingMemoryDeletion))

	version, err := readSchemaVersion(ctx, db)
	require.NoError(t, err)
	require.Equal(t, struct {
		version               int
		pendingDeletion       int
		pendingMemoryDeletion string
	}{
		version:               SchemaVersion,
		pendingDeletion:       0,
		pendingMemoryDeletion: "inst-gone",
	}, struct {
		version               int
		pendingDeletion       int
		pendingMemoryDeletion string
	}{
		version:               version,
		pendingDeletion:       pendingDeletion,
		pendingMemoryDeletion: pendingMemoryDeletion,
	})
}

func TestApplyMigrations_v12_to_v13_keeps_existing_memories_unpinned(t *testing.T) {
	type migrationState struct {
		Version int
		Pinned  bool
	}

	ctx := t.Context()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	seedV1Database(t, db)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	for _, migration := range migrations {
		if migration.Version > 12 {
			continue
		}
		require.NoError(t, migration.Apply(ctx, tx))
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO memories (instance_id, key, content, at) VALUES (?, ?, ?, ?)`,
		"inst-botty", "fact", "likes tea", "2026-08-26T12:00:00Z",
	)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO state (key, value) VALUES ('schema_version', '12')`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	require.NoError(t, applyMigrations(ctx, db))
	version, err := readSchemaVersion(ctx, db)
	require.NoError(t, err)
	var pinned bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT pinned FROM memories WHERE instance_id = ? AND key = ?`,
		"inst-botty", "fact",
	).Scan(&pinned))
	require.Equal(t, migrationState{Version: SchemaVersion}, migrationState{
		Version: version,
		Pinned:  pinned,
	})
}

func TestApplyMigrations_backfills_safe_current_membership_scrollback(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	seedV1Database(t, db)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	for _, migration := range migrations {
		if migration.Version > 5 {
			continue
		}
		require.NoError(t, migration.Apply(ctx, tx))
	}
	_, err = tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO state (key, value) VALUES ('schema_version', '5')`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	members := domain.NewMemberList()
	members.AddIdentity("inst-botty", "botty")
	channel, err := json.Marshal(channelRow{
		Name: "#general", Kind: domain.KindChannel, Members: members,
	})
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO channels (name, data) VALUES ('#general', ?)`, channel)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO events (channel, type, data, at) VALUES
			('#general', 'join', ?, '2025-01-15T10:29:00Z'),
			('#general', 'message', ?, '2025-01-15T10:30:00Z'),
			('#general', 'message', ?, '2025-01-15T10:31:00Z')
	`,
		`{"type":"join","data":{"channel":"#general","nick":"botty","instance_id":"inst-botty","at":"2025-01-15T10:29:00Z"}}`,
		`{"type":"message","data":{"channel":"#general","from":"alice","body":"old","at":"2025-01-15T10:30:00Z"}}`,
		`{"type":"message","data":{"channel":"#general","from":"botty","instance_id":"inst-botty","body":"reply","at":"2025-01-15T10:31:00Z"}}`,
	)
	require.NoError(t, err)

	require.NoError(t, applyMigrations(ctx, db))

	rows, err := db.QueryContext(ctx,
		`SELECT data FROM channel_scrollback ORDER BY id`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var got []domain.PersistableEvent
	for rows.Next() {
		var data string
		require.NoError(t, rows.Scan(&data))
		event, err := domain.UnmarshalPersistableEvent([]byte(data))
		require.NoError(t, err)
		got = append(got, event)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []domain.PersistableEvent{
		domain.Message{
			Source: domain.AnonymousSource(), Target: "#general", Body: "old",
			At: time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC),
		},
		domain.Message{
			Source: domain.ClientSource("inst-botty", "botty"),
			Target: "#general", Body: "reply",
			At: time.Date(2025, 1, 15, 10, 31, 0, 0, time.UTC),
		},
	}, got)
}

func TestApplyMigrations_does_not_backfill_a_closed_membership_interval(t *testing.T) {
	tests := []struct {
		name          string
		departureType string
		departureData string
		want          []domain.PersistableEvent
	}{
		{
			name:          "part",
			departureType: "part",
			departureData: `{"type":"part","data":{"channel":"#general","nick":"botty","instance_id":"inst-botty","at":"2025-01-15T10:31:00Z"}}`,
			want:          []domain.PersistableEvent{},
		},
		{
			name:          "part without an old instance id",
			departureType: "part",
			departureData: `{"type":"part","data":{"channel":"#general","nick":"botty","at":"2025-01-15T10:31:00Z"}}`,
			want:          []domain.PersistableEvent{},
		},
		{
			name:          "kick",
			departureType: "model_kicked",
			departureData: `{"type":"model_kicked","data":{"channel":"#general","nick":"botty","by":"alice","by_instance_id":"inst-alice","at":"2025-01-15T10:31:00Z"}}`,
			want:          []domain.PersistableEvent{},
		},
		{
			name:          "quit",
			departureType: "quit",
			departureData: `{"type":"quit","data":{"nick":"botty","instance_id":"inst-botty","message":"gone","at":"2025-01-15T10:31:00Z"}}`,
			want:          []domain.PersistableEvent{},
		},
		{
			name:          "different identified actor with the same folded nick",
			departureType: "part",
			departureData: `{"type":"part","data":{"channel":"#general","nick":"Botty","instance_id":"inst-other","at":"2025-01-15T10:31:00Z"}}`,
			want: []domain.PersistableEvent{
				domain.Message{
					Source: domain.AnonymousSource(), Target: "#general", Body: "before",
					At: time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC),
				},
				domain.Message{
					Source: domain.AnonymousSource(), Target: "#general", Body: "after",
					At: time.Date(2025, 1, 15, 10, 32, 0, 0, time.UTC),
				},
			},
		},
		{
			name:          "identified user with the same folded nick",
			departureType: "part",
			departureData: `{"type":"part","data":{"channel":"#general","nick":"Botty","instance_id":"","at":"2025-01-15T10:31:00Z"}}`,
			want: []domain.PersistableEvent{
				domain.Message{
					Source: domain.AnonymousSource(), Target: "#general", Body: "before",
					At: time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC),
				},
				domain.Message{
					Source: domain.AnonymousSource(), Target: "#general", Body: "after",
					At: time.Date(2025, 1, 15, 10, 32, 0, 0, time.UTC),
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			db := seedV5ScrollbackDatabase(t)

			_, err := db.ExecContext(ctx, `
				INSERT INTO events (channel, type, data, at) VALUES
					('#general', 'join', ?, '2025-01-15T10:29:00Z'),
					('#general', 'message', ?, '2025-01-15T10:30:00Z'),
					('#general', ?, ?, '2025-01-15T10:31:00Z'),
					('#general', 'message', ?, '2025-01-15T10:32:00Z')
			`,
				`{"type":"join","data":{"channel":"#general","nick":"botty","instance_id":"inst-botty","at":"2025-01-15T10:29:00Z"}}`,
				`{"type":"message","data":{"channel":"#general","from":"alice","body":"before","at":"2025-01-15T10:30:00Z"}}`,
				tt.departureType,
				tt.departureData,
				`{"type":"message","data":{"channel":"#general","from":"alice","body":"after","at":"2025-01-15T10:32:00Z"}}`,
			)
			require.NoError(t, err)

			require.NoError(t, applyMigrations(ctx, db))

			rows, err := db.QueryContext(ctx,
				`SELECT data FROM channel_scrollback ORDER BY id`)
			require.NoError(t, err)
			defer func() { _ = rows.Close() }()

			got := []domain.PersistableEvent{}
			for rows.Next() {
				var data string
				require.NoError(t, rows.Scan(&data))
				event, err := domain.UnmarshalPersistableEvent([]byte(data))
				require.NoError(t, err)
				got = append(got, event)
			}
			require.NoError(t, rows.Err())
			require.Equal(t, tt.want, got)
		})
	}
}

func seedV5ScrollbackDatabase(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	seedV1Database(t, db)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	for _, migration := range migrations {
		if migration.Version > 5 {
			continue
		}
		require.NoError(t, migration.Apply(ctx, tx))
	}
	_, err = tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO state (key, value) VALUES ('schema_version', '5')`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	members := domain.NewMemberList()
	members.AddIdentity("inst-botty", "botty")
	channel, err := json.Marshal(channelRow{
		Name: "#general", Kind: domain.KindChannel, Members: members,
	})
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO channels (name, data) VALUES ('#general', ?)`, channel)
	require.NoError(t, err)

	return db
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

// amendmentDepartureCase is one persona amendment as schema v19 left it.
type amendmentDepartureCase struct {
	Name           string
	ID             int64
	ConsolidatedAt string
}

// amendmentDepartureRow is what schema v20 leaves on that amendment.
type amendmentDepartureRow struct {
	DepartedAt sql.NullString
	Departure  sql.NullString
}

// TestApplyMigrations_v19_to_v20_names_the_recorded_departures covers the
// v20 backfill: a row carrying `consolidated_at` becomes a recorded
// consolidation, and a row that never left keeps both columns null.
func TestApplyMigrations_v19_to_v20_names_the_recorded_departures(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	seedV1Database(t, db)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	for _, migration := range migrations {
		if migration.Version > 19 {
			continue
		}
		require.NoError(t, migration.Apply(ctx, tx))
	}

	data, err := json.Marshal(domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful and curious", nil,
	))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx,
		`INSERT INTO instances (instance_id, nick, data) VALUES ('inst-botty', 'botty', ?)`,
		string(data))
	require.NoError(t, err)

	cases := []amendmentDepartureCase{
		{Name: "a tendency a description absorbed", ID: 1, ConsolidatedAt: "2026-08-01T10:00:00.000000000Z"},
		{Name: "a tendency still in the active set", ID: 2},
	}
	for _, testCase := range cases {
		consolidated := any(nil)
		if testCase.ConsolidatedAt != "" {
			consolidated = testCase.ConsolidatedAt
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO persona_amendments
				(id, instance_id, scope, tendency, confidence, created_at, consolidated_at)
			VALUES (?, 'inst-botty', 'global', 'Gives people a figure.', 'medium',
			        '2026-07-01T10:00:00.000000000Z', ?)
		`, testCase.ID, consolidated)
		require.NoError(t, err)
	}

	for _, migration := range migrations {
		if migration.Version != 20 {
			continue
		}
		require.NoError(t, migration.Apply(ctx, tx))
	}

	want := []amendmentDepartureRow{
		{
			DepartedAt: sql.NullString{String: "2026-08-01T10:00:00.000000000Z", Valid: true},
			Departure:  sql.NullString{String: "consolidated", Valid: true},
		},
		{},
	}

	got := make([]amendmentDepartureRow, 0, len(cases))
	for _, testCase := range cases {
		t.Run(testCase.Name, func(t *testing.T) {
			var row amendmentDepartureRow
			require.NoError(t, tx.QueryRowContext(ctx,
				`SELECT departed_at, departure FROM persona_amendments WHERE id = ?`,
				testCase.ID,
			).Scan(&row.DepartedAt, &row.Departure))
			got = append(got, row)
		})
	}

	require.Equal(t, want, got)
}

// storedEventRow is one row of a table holding a serialised
// [domain.PersistableEvent]: the discriminator in its own column and
// the envelope in `data`.
type storedEventRow struct {
	Type string
	Data string
}

// TestApplyMigrations_v20_to_v21_renames_the_persona_template_pool
// covers the three stored names the template rename left behind: the
// pool's table, the discriminator a template list is written under, and
// the field holding the templates inside it. A row written before the
// migration must come back through
// [domain.UnmarshalPersistableEvent], which knows only the new names.
func TestApplyMigrations_v20_to_v21_renames_the_persona_template_pool(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	seedV1Database(t, db)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	for _, migration := range migrations {
		if migration.Version > 20 {
			continue
		}
		require.NoError(t, migration.Apply(ctx, tx))
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO personas (id, description, origin) VALUES ('bard', 'A travelling storyteller', 'user')`)
	require.NoError(t, err)

	_, err = tx.ExecContext(ctx,
		`INSERT INTO channels (name, data) VALUES ('#dev', '{}')`)
	require.NoError(t, err)

	const listedAt = "2026-08-01T10:00:00Z"

	const storedList = `{"version":2,"type":"personas_list","data":{"personas":` +
		`[{"id":"bard","description":"A travelling storyteller","origin":"user"}],` +
		`"at":"2026-08-01T10:00:00Z"}}`

	for _, insert := range []string{
		`INSERT INTO events (channel, type, data, at) VALUES ('#dev', 'personas_list', ?, '2026-08-01T10:00:00Z')`,
		`INSERT INTO instance_replies (instance_id, type, data, at, window_key, window_kind)
		 VALUES ('', 'personas_list', ?, '2026-08-01T10:00:00Z', '#dev', 1)`,
		`INSERT INTO channel_scrollback (instance_id, channel, type, data, at)
		 VALUES ('', '#dev', 'personas_list', ?, '2026-08-01T10:00:00Z')`,
	} {
		_, err = tx.ExecContext(ctx, insert, storedList)
		require.NoError(t, err)
	}

	for _, migration := range migrations {
		if migration.Version != 21 {
			continue
		}
		require.NoError(t, migration.Apply(ctx, tx))
	}

	var template domain.PersonaTemplate
	require.NoError(t, tx.QueryRowContext(ctx,
		`SELECT id, description, origin FROM persona_templates`,
	).Scan(&template.ID, &template.Description, &template.Origin))

	rows := map[string]storedEventRow{}
	events := map[string]domain.PersistableEvent{}
	for _, table := range []string{"events", "instance_replies", "channel_scrollback"} {
		var row storedEventRow
		require.NoError(t, tx.QueryRowContext(ctx,
			`SELECT type, data FROM `+table+` WHERE at = ?`, listedAt,
		).Scan(&row.Type, &row.Data))
		rows[table] = storedEventRow{Type: row.Type}

		decoded, err := domain.UnmarshalPersistableEvent([]byte(row.Data))
		require.NoError(t, err, table)
		events[table] = decoded
	}

	wantList := domain.PersonaTemplatesList{
		Templates: []domain.PersonaTemplate{
			{ID: "bard", Description: "A travelling storyteller", Origin: domain.PersonaUser},
		},
		At: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
	}

	require.Equal(t, struct {
		Template domain.PersonaTemplate
		Rows     map[string]storedEventRow
		Events   map[string]domain.PersistableEvent
	}{
		Template: domain.PersonaTemplate{
			ID: "bard", Description: "A travelling storyteller", Origin: domain.PersonaUser,
		},
		Rows: map[string]storedEventRow{
			"events":             {Type: "persona_templates_list"},
			"instance_replies":   {Type: "persona_templates_list"},
			"channel_scrollback": {Type: "persona_templates_list"},
		},
		Events: map[string]domain.PersistableEvent{
			"events": wantList, "instance_replies": wantList, "channel_scrollback": wantList,
		},
	}, struct {
		Template domain.PersonaTemplate
		Rows     map[string]storedEventRow
		Events   map[string]domain.PersistableEvent
	}{Template: template, Rows: rows, Events: events})
}
