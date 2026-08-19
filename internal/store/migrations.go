package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SchemaVersion is the current on-disk schema version. Bumped by
// exactly one whenever a schema change is needed; the corresponding
// [migration] entry brings a v(N-1) database forward to vN.
//
// `schema` (sqlite.go) only ever describes the v1 shape and seeds
// `state.schema_version` to '1' via `INSERT OR IGNORE` — it never
// grows the v2+ shape directly. NewSQLiteStore execs `schema` before
// running migrations, and `CREATE TABLE IF NOT EXISTS` /
// `CREATE INDEX IF NOT EXISTS` are no-ops against a table an earlier
// version already created; if `schema` also carried a v2+ column or
// index reference, that statement would run before the migration
// that is supposed to introduce it and fail against any database
// that predates this version. Every database — fresh or
// pre-existing — reaches the current shape through applyMigrations,
// the single path from v1 onward.
const SchemaVersion = 2

// migration is one forward-only step that brings the database
// from v(Version-1) to vVersion. Apply runs inside the
// transaction [applyMigrations] opens, so a mid-chain failure
// rolls every applied step back.
type migration struct {
	Version int
	Apply   func(ctx context.Context, tx *sql.Tx) error
}

// migrations is the ordered registry of forward-only steps. v1 is
// the first cut and nothing predates it. Future schema changes
// append entries with strictly increasing `Version`.
var migrations = []migration{
	{
		Version: 2,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			// dm_instance_id gives DMEventsBefore's thread lookup a
			// real column, so idx_events_dm_thread can serve it as an
			// index seek.
			if _, err := tx.ExecContext(ctx, `
				ALTER TABLE events ADD COLUMN dm_instance_id TEXT GENERATED ALWAYS AS
					(coalesce(json_extract(data, '$.data.instance_id'), '')) VIRTUAL
			`); err != nil {
				return fmt.Errorf("add events.dm_instance_id: %w", err)
			}

			if _, err := tx.ExecContext(ctx, `
				CREATE INDEX IF NOT EXISTS idx_events_dm_thread
					ON events (dm_instance_id, type, channel, id)
			`); err != nil {
				return fmt.Errorf("create idx_events_dm_thread: %w", err)
			}

			// dm_windows holds the user-client's set of open DM
			// windows, keyed by the counterpart model instance's id.
			// Client-owned data: the user-client reads and writes
			// this table directly; the session dispatcher never
			// touches it. Deleting the counterpart instance cascades
			// to drop its DM window entry too.
			if _, err := tx.ExecContext(ctx, `
				CREATE TABLE IF NOT EXISTS dm_windows (
					instance_id TEXT PRIMARY KEY REFERENCES instances(instance_id) ON DELETE CASCADE
				)
			`); err != nil {
				return fmt.Errorf("create dm_windows: %w", err)
			}

			return nil
		},
	},
}

// applyMigrations reconciles the recorded schema version against
// [SchemaVersion]. A current database is a no-op; an older
// database runs every registered step whose Version is strictly
// greater than the recorded one, transactionally; a database
// from a newer build fails-loud because downgrades aren't
// supported.
func applyMigrations(ctx context.Context, db *sql.DB) error {
	got, err := readSchemaVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	if got > SchemaVersion {
		return fmt.Errorf(
			"store schema is v%d but this build expects v%d; downgrades aren't supported",
			got, SchemaVersion,
		)
	}

	if got == SchemaVersion {
		return nil
	}

	missing := missingMigrations(got)
	if len(missing) == 0 {
		return fmt.Errorf(
			"store schema is v%d with no migration to reach v%d; delete the store file to start fresh",
			got, SchemaVersion,
		)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, m := range missing {
		if err := m.Apply(ctx, tx); err != nil {
			return fmt.Errorf("migrate to v%d: %w", m.Version, err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO state (key, value) VALUES ('schema_version', ?)`,
		fmt.Sprintf("%d", SchemaVersion),
	); err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}

	return nil
}

// missingMigrations returns the registered steps strictly greater
// than `from`. Returns nil when the gap from `from` to
// [SchemaVersion] is unbridgeable — every intermediate step must
// be registered for the chain to be runnable.
func missingMigrations(from int) []migration {
	var chain []migration
	expected := from + 1
	for _, m := range migrations {
		if m.Version <= from {
			continue
		}
		if m.Version != expected {
			return nil
		}
		chain = append(chain, m)
		expected++
	}

	if expected-1 != SchemaVersion {
		return nil
	}

	return chain
}

// readSchemaVersion returns the recorded version, or 0 when no
// row exists. A 0 result from an empty database is normal — the
// `INSERT OR IGNORE` in `schema` seeds the row to '1' on first
// exec, and applyMigrations brings it to [SchemaVersion] right
// after. A 0 from a populated database indicates a state row that
// pre-dates the seed and is handled by applyMigrations's "no
// migration to reach" branch.
func readSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	err := db.QueryRowContext(ctx,
		`SELECT CAST(value AS INTEGER) FROM state WHERE key = 'schema_version'`,
	).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}
