package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
)

const (
	// eventRetentionHeadroom is how many of a conversation's most
	// recent events survive a retention pass, whether the
	// conversation is a channel (matched on the events.channel
	// column) or a DM thread (matched via the dm_instance_id
	// generated column, the same column DMEventsBefore reads).
	//
	// A model-client's attach-time load never asks the store for more
	// than modelHistorySize (internal/modelclient, 500) events, for a
	// channel or a DM thread alike, and never re-reads the store
	// after that. eventRetentionHeadroom keeps four times that: a
	// channel or thread does not shrink back down to bare 500
	// immediately after every retention pass. pruneEvents runs on
	// open, not on a timer, so this margin is what gives a session
	// room to grow before the next pass starts crowding the boundary
	// a model's own read sits at.
	eventRetentionHeadroom = 4 * 500

	// eventDeleteBatchSize bounds how many rows one DELETE statement
	// in a retention pass removes. A database that has never been
	// pruned before, or has run for a long time between opens, can be
	// holding a large backlog; removing it in bounded chunks instead
	// of one statement keeps any single DELETE's lock and undo-log
	// footprint small so the pass stays responsive.
	eventDeleteBatchSize = 500
)

// cursorExclusion is the predicate fragment every retention DELETE
// carries. `last_read.event_id` and `dm_last_read.event_id` are
// foreign keys into `events(id)` with no `ON DELETE` clause, so
// deleting a row either cursor still references fails the whole
// statement under `foreign_keys=on`. Retention must never attempt
// it: a cursor row that has fallen behind eventRetentionHeadroom
// survives as the one row it is, until a later MarkRead moves the
// cursor past it, at which point the next retention pass is free to
// remove it. UnreadCount's `id >= cursor` counting and MarkRead's own
// write are unaffected either way: neither depends on any row
// between the cursor and the newest event actually existing, only on
// the cursor's own row still being there to satisfy the foreign key.
const cursorExclusion = `id NOT IN (
	SELECT event_id FROM last_read
	UNION
	SELECT event_id FROM dm_last_read
)`

// pruneEvents runs the store's retention pass over the events table:
// pruneOrphanChannelEvents first removes rows no consumer can reach
// at all, then every live channel and every known DM counterpart's
// thread is trimmed down to eventRetentionHeadroom rows.
//
// It runs once, here, rather than on a recurring timer. A background
// pass would need its own goroutine, a cancellation path, and a slot
// in main.go's shutdown join order to avoid leaking one, machinery
// this single-user desktop app does not otherwise carry. A run at
// open already gets most of that benefit: flood control (`+f`, the
// per-connection penalty algorithm) already bounds how fast a
// channel's event count can climb, so an ordinary session's growth
// between two opens stays well under eventRetentionHeadroom. An
// exceptionally long-running session outgrows the headroom until its
// next restart, which is an acceptable trade against the added
// complexity of an owned background loop.
//
// A failure is logged by the caller (see NewSQLiteStore) rather than
// failing store construction: retention is disk hygiene, not a
// correctness requirement the rest of the store depends on.
func (s *SQLiteStore) pruneEvents(ctx context.Context) error {
	return s.inSpan(ctx, "store.sqlite.prune_events", nil, func(ctx context.Context, span trace.Span) error {
		orphaned, err := s.pruneOrphanChannelEvents(ctx)
		if err != nil {
			return fmt.Errorf("prune orphaned channel events: %w", err)
		}

		channelNames, err := queryRows(ctx, s.db, `SELECT name FROM channels ORDER BY name`, nil,
			scalarColumn[domain.ChannelName]())
		if err != nil {
			return fmt.Errorf("list channels: %w", err)
		}

		var channelTrimmed int64
		for _, name := range channelNames {
			n, err := s.pruneChannelEvents(ctx, name)
			if err != nil {
				return fmt.Errorf("prune channel %q events: %w", name, err)
			}
			channelTrimmed += n
		}

		instanceIDs, err := queryRows(ctx, s.db, `SELECT instance_id FROM instances ORDER BY instance_id`, nil,
			scalarColumn[domain.InstanceID]())
		if err != nil {
			return fmt.Errorf("list instances: %w", err)
		}

		var dmTrimmed int64
		for _, id := range instanceIDs {
			n, err := s.pruneDMEvents(ctx, id)
			if err != nil {
				return fmt.Errorf("prune dm thread %q events: %w", id, err)
			}
			dmTrimmed += n
		}

		span.SetAttributes(
			attribute.Int64("modeloff.retention.orphaned_removed", orphaned),
			attribute.Int64("modeloff.retention.channel_trimmed", channelTrimmed),
			attribute.Int64("modeloff.retention.dm_trimmed", dmTrimmed),
		)

		if orphaned+channelTrimmed+dmTrimmed > 0 {
			slog.Default().InfoContext(ctx, "event retention pass",
				"component", "store.sqlite",
				"orphaned_removed", orphaned,
				"channel_trimmed", channelTrimmed,
				"dm_trimmed", dmTrimmed,
			)
		}

		// A no-op against a database that predates auto_vacuum(incremental)
		// (see SQLitePragmaDSN): PRAGMA incremental_vacuum only reclaims
		// pages on a database actually running in incremental-vacuum
		// mode. On one that is, this is what returns the pages the
		// deletes above just freed to the OS.
		if _, err := s.db.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
			return fmt.Errorf("incremental vacuum: %w", err)
		}

		return nil
	})
}

// pruneOrphanChannelEvents removes event rows logged under a
// channel-shaped name (domain.ChannelPrefixes) that is not the
// canonical spelling of any channel currently in the channels table.
// Two situations produce such a row:
//
//   - the channel itself is gone: its last occupant parted, and
//     DeleteWindow removed every spelling of it (see DeleteWindow's
//     doc comment, which files this exact cleanup here). Nothing
//     will ever ask for these rows again.
//   - a database written before the server casemapped names holds a
//     case-pair (e.g. "#Dev" and "#dev") as two separate rows in
//     channels. GetWindow always answers such a pair with the same
//     one of the two (the BINARY-smaller spelling), and every write
//     from that point on lands under that spelling, so the other
//     row's own past events are BINARY-unreachable by any query the
//     store runs, even though the channel itself is still alive.
//
// Both are deleted rather than folded into the surviving spelling's
// history. Folding would splice them into the middle of a channel's
// chronological log, in an order a model never actually experienced
// them in; a model that replays its channel history honestly is
// better served by a gap than by out-of-order context it never saw
// arrive that way.
func (s *SQLiteStore) pruneOrphanChannelEvents(ctx context.Context) (int64, error) {
	prefixes := []rune(domain.ChannelPrefixes)
	placeholders := make([]string, len(prefixes))
	args := make([]any, len(prefixes))

	for i, r := range prefixes {
		placeholders[i] = "?"
		args[i] = string(r)
	}

	// The canonical spelling of every NOCASE-equivalence class
	// currently in channels is its BINARY-smallest name, the same
	// answer GetWindow's `ORDER BY name LIMIT 1` gives for a lookup
	// against that class.
	query := `DELETE FROM events WHERE id IN (
		SELECT id FROM events WHERE substr(channel, 1, 1) IN (` + strings.Join(placeholders, ",") + `)
			AND channel NOT IN (SELECT MIN(name) FROM channels GROUP BY name COLLATE NOCASE)
			AND ` + cursorExclusion + `
		ORDER BY id LIMIT ?
	)`

	removed, err := deleteEventsBatched(ctx, s.db, query, args)
	if err != nil {
		return removed, err
	}

	if removed > 0 {
		slog.Default().InfoContext(ctx, "pruned orphaned channel event rows",
			"component", "store.sqlite",
			"removed", removed,
		)
	}

	return removed, nil
}

// pruneChannelEvents trims channel ch's event log down to
// eventRetentionHeadroom rows, deleting the oldest excess in batches
// of eventDeleteBatchSize.
func (s *SQLiteStore) pruneChannelEvents(ctx context.Context, ch domain.ChannelName) (int64, error) {
	query := `DELETE FROM events WHERE id IN (
		SELECT id FROM events WHERE channel = ? AND id NOT IN (
			SELECT id FROM events WHERE channel = ? ORDER BY id DESC LIMIT ?
		) AND ` + cursorExclusion + `
		ORDER BY id LIMIT ?
	)`

	return deleteEventsBatched(ctx, s.db, query, []any{ch, ch, eventRetentionHeadroom})
}

// pruneDMEvents trims the message rows of the DM thread between the
// user and peer down to eventRetentionHeadroom, matching the shape
// DMEventsBefore reads: `(channel = peer, dm_instance_id = "")` for a
// line the user sent, or `(channel = "", dm_instance_id = peer)` for
// one peer sent back.
//
// The peer's actor-scoped events (quit, nick_change) that
// DMEventsBefore also replays into the thread are not counted here.
// Their `channel` column names the real channel the peer was in, not
// the DM, so they are governed by that channel's own
// pruneChannelEvents pass; eventRetentionHeadroom is generously past
// what either read needs, so leaving them out of the DM count does
// not risk trimming a thread down to fewer than the 500 events a
// model's attach-time load actually asks for.
func (s *SQLiteStore) pruneDMEvents(ctx context.Context, peer domain.InstanceID) (int64, error) {
	const self = domain.InstanceID("")
	const thread = `(channel = ? AND dm_instance_id = ?) OR (channel = ? AND dm_instance_id = ?)`

	query := `DELETE FROM events WHERE id IN (
		SELECT id FROM events WHERE (` + thread + `) AND id NOT IN (
			SELECT id FROM events WHERE ` + thread + ` ORDER BY id DESC LIMIT ?
		) AND ` + cursorExclusion + `
		ORDER BY id LIMIT ?
	)`

	args := []any{
		peer, self, self, peer, // outer predicate
		peer, self, self, peer, // inner top-N subquery
		eventRetentionHeadroom,
	}

	return deleteEventsBatched(ctx, s.db, query, args)
}

// deleteEventsBatched repeatedly runs query/args, which must be a
// complete DELETE statement ending in a single trailing placeholder
// for the batch size, stopping once a run affects fewer rows than
// eventDeleteBatchSize. Returns the total removed.
func deleteEventsBatched(ctx context.Context, db *sql.DB, query string, args []any) (int64, error) {
	batchArgs := make([]any, len(args)+1)
	copy(batchArgs, args)
	batchArgs[len(args)] = eventDeleteBatchSize

	var total int64
	for {
		result, err := db.ExecContext(ctx, query, batchArgs...)
		if err != nil {
			return total, err
		}

		n, err := result.RowsAffected()
		if err != nil {
			return total, err
		}

		total += n

		if n < eventDeleteBatchSize {
			return total, nil
		}

		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}
