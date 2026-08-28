package store

import (
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

// memoryRetentionEffect is which memories survived a retention pass.
type memoryRetentionEffect struct {
	Keys []string
}

// TestSQLiteStore_pruneMemories_keeps_the_newest_by_instant pins that
// retention ranks memories by the instant they record and not by the text
// that records it.
//
// Every memory here is written within one second of the others, so the
// fractional part decides the order. [time.RFC3339Nano] trims trailing
// zeros, which puts a memory written on the second ahead of every memory
// written after it, and orders the fractions among themselves by digit
// rather than by magnitude.
func TestSQLiteStore_pruneMemories_keeps_the_newest_by_instant(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	const actor = domain.InstanceID("inst-botty")

	require.NoError(t, s.SaveInstance(ctx,
		domain.NewModelInstance(actor, "botty", "test/model", "", nil)))

	keys := make([]string, memoryRetentionHeadroom+1)
	for i := range keys {
		keys[i] = fmt.Sprintf("fact_%04d", i)
		require.NoError(t, s.WriteMemory(
			ctx, actor, keys[i], fmt.Sprintf("value %d", i),
			testTime.Add(time.Duration(i)*time.Millisecond), false,
		))
	}

	require.NoError(t, s.pruneEvents(ctx))
	got, err := s.ReadMemories(ctx, actor)
	require.NoError(t, err)
	surviving := memoryKeys(got)
	slices.Sort(surviving)

	require.Equal(t,
		memoryRetentionEffect{Keys: keys[1:]},
		memoryRetentionEffect{Keys: surviving},
	)
}

// reflectionRunOrderEffect is the order an instance's runs are reported in,
// newest first, and the finish time the cooldown measures from.
type reflectionRunOrderEffect struct {
	Order       []domain.ReflectionRunID
	LastAttempt time.Time
}

// TestSQLiteStore_reflection_runs_order_by_instant pins that both the
// listing and the cooldown read the instant a run finished.
//
// The four runs finish within one second of each other. Under
// [time.RFC3339Nano] the run that finished on the second sorts after every
// run that finished later, so it would be reported first and would be the
// maximum the cooldown measures from; and ".12" sorts before ".1" although
// it is the later instant.
func TestSQLiteStore_reflection_runs_order_by_instant(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	instance := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful", nil,
	)
	require.NoError(t, s.SaveInstance(ctx, instance))
	state, err := s.PersonaLineage(ctx, instance.ID())
	require.NoError(t, err)

	finishes := []time.Duration{
		0,
		100 * time.Millisecond,
		120 * time.Millisecond,
		200 * time.Millisecond,
	}
	for index, offset := range finishes {
		at := testTime.Add(offset)
		require.NoError(t, s.RecordReflectionRun(ctx, domain.ReflectionRun{
			ID:         domain.ReflectionRunID(fmt.Sprintf("run-%d", index)),
			InstanceID: instance.ID(), BaseRevisionID: state.CurrentRevisionID,
			ResultRevisionID: state.CurrentRevisionID,
			ModelID:          "test/reflection", Outcome: domain.ReflectionFailed,
			RejectionReason: "upstream_failed", StartedAt: at, FinishedAt: at,
		}))
	}

	runs, err := s.ReflectionRuns(ctx, instance.ID(), len(finishes))
	require.NoError(t, err)
	order := make([]domain.ReflectionRunID, len(runs))
	for i, run := range runs {
		order[i] = run.ID
	}
	status, err := s.ReflectionInboxStatus(ctx, instance.ID())
	require.NoError(t, err)
	require.NotNil(t, status.LastAttemptAt)

	require.Equal(t, reflectionRunOrderEffect{
		Order:       []domain.ReflectionRunID{"run-3", "run-2", "run-1", "run-0"},
		LastAttempt: testTime.Add(200 * time.Millisecond),
	}, reflectionRunOrderEffect{
		Order: order, LastAttempt: status.LastAttemptAt.UTC(),
	})
}

// storedTimeEffect is what a column holds after the normalisation pass, and
// the order those rows come back in.
type storedTimeEffect struct {
	Stored []string
	Keys   []string
}

// TestApplyMigrations_normalises_stored_times pins that a database written
// before the store settled on [sortableTimeLayout] has its instants
// rewritten, so the rows already on disk order chronologically.
//
// The seeded values are what the store used to write: a trimmed fraction,
// and an offset kept from the machine that recorded it.
func TestApplyMigrations_normalises_stored_times(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	seedV1Database(t, db)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	for _, migration := range migrations {
		if migration.Version > 17 {
			continue
		}
		require.NoError(t, migration.Apply(ctx, tx))
	}

	// London on this date is one hour ahead, so the offset spelling
	// records the earlier instant of the two.
	legacy := []struct {
		key string
		at  string
	}{
		{"offset", "2026-06-25T01:30:00+01:00"},
		{"trimmed", "2026-06-25T00:45:00.5Z"},
		{"whole", "2026-06-25T00:45:00Z"},
	}
	for _, row := range legacy {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO memories (instance_id, key, content, at, pinned)
			 VALUES ('inst-botty', ?, 'value', ?, 0)`, row.key, row.at)
		require.NoError(t, err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO state (key, value) VALUES ('schema_version', '17')`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	require.NoError(t, applyMigrations(ctx, db))

	rows, err := db.QueryContext(ctx,
		`SELECT key, at FROM memories ORDER BY at ASC`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	got := storedTimeEffect{}
	for rows.Next() {
		var key, at string
		require.NoError(t, rows.Scan(&key, &at))
		got.Keys = append(got.Keys, key)
		got.Stored = append(got.Stored, at)
	}
	require.NoError(t, rows.Err())

	// Under the spelling these rows were seeded with, ascending text order
	// is the exact reverse: ".5Z" before "Z", and both before "+01:00".
	require.Equal(t, storedTimeEffect{
		Keys: []string{"offset", "whole", "trimmed"},
		Stored: []string{
			"2026-06-25T00:30:00.000000000Z",
			"2026-06-25T00:45:00.000000000Z",
			"2026-06-25T00:45:00.500000000Z",
		},
	}, got)
}

// storedTimeColumnCoverage is the set of columns holding an instant, as the
// schema reports them against as the list the migration carries.
type storedTimeColumnCoverage struct {
	Columns []string
}

// TestStoredTimeColumns_covers_the_schema pins that the normalisation pass
// names every column of instants the schema had reached by v18.
//
// The list is written out rather than discovered, so that the migration
// covers the schema of its own version and no later one. This is what keeps
// the written list honest about the version it was written for.
func TestStoredTimeColumns_covers_the_schema(t *testing.T) {
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
		if migration.Version > 18 {
			continue
		}
		require.NoError(t, migration.Apply(ctx, tx))
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT tables.name || '.' || columns.name
		FROM sqlite_master AS tables
		JOIN pragma_table_info(tables.name) AS columns
		WHERE tables.type = 'table'
		  AND tables.name NOT LIKE 'sqlite_%'
		  AND columns.type = 'TEXT'
		  AND (columns.name = 'at' OR columns.name LIKE '%\_at' ESCAPE '\')
		ORDER BY tables.name, columns.name
	`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	inSchema := storedTimeColumnCoverage{}
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		inSchema.Columns = append(inSchema.Columns, name)
	}
	require.NoError(t, rows.Err())

	covered := storedTimeColumnCoverage{}
	for _, column := range storedTimeColumns {
		covered.Columns = append(covered.Columns,
			column.table+"."+column.column)
	}
	slices.Sort(covered.Columns)

	require.Equal(t, inSchema, covered)
}
