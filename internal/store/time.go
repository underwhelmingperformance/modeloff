package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// sortableTimeLayout is how the store spells an instant. It is RFC 3339
// normalised twice over: to UTC, and to a fixed nine digits of fractional
// seconds. Both are what make the text sort chronologically.
//
// [time.RFC3339Nano] does neither. It trims trailing zeros, so ".1Z" sorts
// after ".12Z" although ".12Z" is the later instant, and it keeps whatever
// offset the value carried, so "01:30:00+01:00" sorts after the later
// "01:15:00Z". A query ordering by one of these columns, or taking a max
// over one, reads the text and not the instant.
const sortableTimeLayout = "2006-01-02T15:04:05.000000000Z"

// formatTime renders an instant for storage. Every time this store writes
// goes through it, so a column can be ordered by without asking which
// writer produced the row.
func formatTime(t time.Time) string {
	return t.UTC().Format(sortableTimeLayout)
}

// parseTime reads a stored instant back in UTC. It accepts any RFC 3339
// spelling, so a row written before the store settled on
// [sortableTimeLayout] reads back as the instant it recorded.
func parseTime(s string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, err
	}

	return parsed.UTC(), nil
}

// storedTimeColumn is one column of instants together with the statements
// that read and rewrite it.
//
// Both statements are constant. A migration describes the schema of its own
// version, so the columns it covers are the ones that existed when it was
// written; discovering them from the live schema would let it reach columns
// a later migration adds.
type storedTimeColumn struct {
	table  string
	column string
	read   string
	write  string
}

// normaliseStoredTimes rewrites every stored instant into
// [sortableTimeLayout].
//
// A database written before the store settled on that layout holds RFC 3339
// text with trimmed fractions and, for a memory, whatever offset the writing
// machine was on. Ordering by such a column reads the rows in the order of
// their spelling, so rows already on disk stay misordered until each one is
// rewritten.
func normaliseStoredTimes(ctx context.Context, tx *sql.Tx) error {
	for _, column := range storedTimeColumns {
		if err := normaliseTimeColumn(ctx, tx, column); err != nil {
			return err
		}
	}

	return nil
}

// storedRow is one row's identity and its stored spelling of an instant.
type storedRow struct {
	rowID int64
	value string
}

// normaliseTimeColumn rewrites one column's values. A value no RFC 3339
// parser accepts is left as it is: this pass restores the ordering of the
// instants the store wrote, and it is not the place to decide what an
// unreadable value meant.
func normaliseTimeColumn(
	ctx context.Context,
	tx *sql.Tx,
	column storedTimeColumn,
) error {
	rewrites, err := rewritableTimes(ctx, tx, column)
	if err != nil {
		return err
	}

	for _, row := range rewrites {
		if _, err := tx.ExecContext(ctx, column.write, row.value, row.rowID); err != nil {
			return fmt.Errorf(
				"rewrite %s.%s: %w", column.table, column.column, err,
			)
		}
	}

	return nil
}

// rewritableTimes reads one column and returns the rows whose spelling
// differs from the canonical one. The read finishes before any write, so the
// column's own cursor is closed while the updates run.
func rewritableTimes(
	ctx context.Context,
	tx *sql.Tx,
	column storedTimeColumn,
) ([]storedRow, error) {
	rows, err := tx.QueryContext(ctx, column.read)
	if err != nil {
		return nil, fmt.Errorf("read %s.%s: %w", column.table, column.column, err)
	}
	defer func() { _ = rows.Close() }()

	rewrites := []storedRow{}
	for rows.Next() {
		var row storedRow
		if err := rows.Scan(&row.rowID, &row.value); err != nil {
			return nil, fmt.Errorf(
				"read %s.%s: %w", column.table, column.column, err,
			)
		}
		parsed, err := parseTime(row.value)
		if err != nil {
			continue
		}
		if canonical := formatTime(parsed); canonical != row.value {
			rewrites = append(rewrites, storedRow{rowID: row.rowID, value: canonical})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s.%s: %w", column.table, column.column, err)
	}

	return rewrites, rows.Close()
}

// storedTimeColumns is every column holding an instant as of schema v18.
// TestStoredTimeColumns_covers_the_schema compares it with the database
// that migration builds.
var storedTimeColumns = []storedTimeColumn{
	{
		table: "channel_scrollback", column: "at",
		read:  `SELECT rowid, at FROM channel_scrollback WHERE at IS NOT NULL`,
		write: `UPDATE channel_scrollback SET at = ? WHERE rowid = ?`,
	},
	{
		table: "context_summaries", column: "created_at",
		read:  `SELECT rowid, created_at FROM context_summaries WHERE created_at IS NOT NULL`,
		write: `UPDATE context_summaries SET created_at = ? WHERE rowid = ?`,
	},
	{
		table: "context_summary_sources", column: "at",
		read:  `SELECT rowid, at FROM context_summary_sources WHERE at IS NOT NULL`,
		write: `UPDATE context_summary_sources SET at = ? WHERE rowid = ?`,
	},
	{
		table: "events", column: "at",
		read:  `SELECT rowid, at FROM events WHERE at IS NOT NULL`,
		write: `UPDATE events SET at = ? WHERE rowid = ?`,
	},
	{
		table: "instance_replies", column: "at",
		read:  `SELECT rowid, at FROM instance_replies WHERE at IS NOT NULL`,
		write: `UPDATE instance_replies SET at = ? WHERE rowid = ?`,
	},
	{
		table: "memories", column: "at",
		read:  `SELECT rowid, at FROM memories WHERE at IS NOT NULL`,
		write: `UPDATE memories SET at = ? WHERE rowid = ?`,
	},
	{
		table: "model_turn_entries", column: "at",
		read:  `SELECT rowid, at FROM model_turn_entries WHERE at IS NOT NULL`,
		write: `UPDATE model_turn_entries SET at = ? WHERE rowid = ?`,
	},
	{
		table: "model_turns", column: "started_at",
		read:  `SELECT rowid, started_at FROM model_turns WHERE started_at IS NOT NULL`,
		write: `UPDATE model_turns SET started_at = ? WHERE rowid = ?`,
	},
	{
		table: "persona_amendments", column: "consolidated_at",
		read:  `SELECT rowid, consolidated_at FROM persona_amendments WHERE consolidated_at IS NOT NULL`,
		write: `UPDATE persona_amendments SET consolidated_at = ? WHERE rowid = ?`,
	},
	{
		table: "persona_amendments", column: "created_at",
		read:  `SELECT rowid, created_at FROM persona_amendments WHERE created_at IS NOT NULL`,
		write: `UPDATE persona_amendments SET created_at = ? WHERE rowid = ?`,
	},
	{
		table: "persona_amendments", column: "expires_at",
		read:  `SELECT rowid, expires_at FROM persona_amendments WHERE expires_at IS NOT NULL`,
		write: `UPDATE persona_amendments SET expires_at = ? WHERE rowid = ?`,
	},
	{
		table: "persona_experiences", column: "created_at",
		read:  `SELECT rowid, created_at FROM persona_experiences WHERE created_at IS NOT NULL`,
		write: `UPDATE persona_experiences SET created_at = ? WHERE rowid = ?`,
	},
	{
		table: "persona_experiences", column: "occurred_at",
		read:  `SELECT rowid, occurred_at FROM persona_experiences WHERE occurred_at IS NOT NULL`,
		write: `UPDATE persona_experiences SET occurred_at = ? WHERE rowid = ?`,
	},
	{
		table: "persona_lineages", column: "created_at",
		read:  `SELECT rowid, created_at FROM persona_lineages WHERE created_at IS NOT NULL`,
		write: `UPDATE persona_lineages SET created_at = ? WHERE rowid = ?`,
	},
	{
		table: "persona_lineages", column: "reflected_at",
		read:  `SELECT rowid, reflected_at FROM persona_lineages WHERE reflected_at IS NOT NULL`,
		write: `UPDATE persona_lineages SET reflected_at = ? WHERE rowid = ?`,
	},
	{
		table: "persona_revisions", column: "created_at",
		read:  `SELECT rowid, created_at FROM persona_revisions WHERE created_at IS NOT NULL`,
		write: `UPDATE persona_revisions SET created_at = ? WHERE rowid = ?`,
	},
	{
		table: "persona_transitions", column: "at",
		read:  `SELECT rowid, at FROM persona_transitions WHERE at IS NOT NULL`,
		write: `UPDATE persona_transitions SET at = ? WHERE rowid = ?`,
	},
	{
		table: "reflection_events", column: "created_at",
		read:  `SELECT rowid, created_at FROM reflection_events WHERE created_at IS NOT NULL`,
		write: `UPDATE reflection_events SET created_at = ? WHERE rowid = ?`,
	},
	{
		table: "reflection_events", column: "event_at",
		read:  `SELECT rowid, event_at FROM reflection_events WHERE event_at IS NOT NULL`,
		write: `UPDATE reflection_events SET event_at = ? WHERE rowid = ?`,
	},
	{
		table: "reflection_runs", column: "finished_at",
		read:  `SELECT rowid, finished_at FROM reflection_runs WHERE finished_at IS NOT NULL`,
		write: `UPDATE reflection_runs SET finished_at = ? WHERE rowid = ?`,
	},
	{
		table: "reflection_runs", column: "started_at",
		read:  `SELECT rowid, started_at FROM reflection_runs WHERE started_at IS NOT NULL`,
		write: `UPDATE reflection_runs SET started_at = ? WHERE rowid = ?`,
	},
}
