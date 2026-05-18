package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

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
