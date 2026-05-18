package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SchemaVersion is the current on-disk schema version. Bumped by
// exactly one whenever the SQL in `schema` (sqlite.go) changes;
// the corresponding [migration] entry brings a v(N-1) database
// forward to vN.
//
// A fresh database adopts SchemaVersion atomically as part of the
// `schema` exec: the seed `INSERT OR IGNORE` into `state` lands
// in the same statement batch as the table creations. Existing
// databases keep whatever version they last recorded; the
// `INSERT OR IGNORE` is a no-op.
const SchemaVersion = 1

// migration is one forward-only step that brings the database
// from v(Version-1) to vVersion. Apply runs inside the
// transaction [applyMigrations] opens, so a mid-chain failure
// rolls every applied step back.
type migration struct {
	Version int
	Apply   func(ctx context.Context, tx *sql.Tx) error
}

// migrations is the ordered registry of forward-only steps.
// Empty: v1 is the first cut and nothing predates it. Future
// schema changes append entries with strictly increasing
// `Version`.
var migrations = []migration{}

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
// `INSERT OR IGNORE` in `schema` seeds the row to [SchemaVersion]
// on first exec. A 0 from a populated database indicates a state
// row that pre-dates the seed and is handled by
// [applyMigrations]'s "no migration to reach" branch.
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
