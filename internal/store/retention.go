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
// at all, then every live channel and every pair of DM
// correspondents' thread is trimmed down to eventRetentionHeadroom
// rows.
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

		dmOrphaned, err := s.pruneOrphanDMEvents(ctx)
		if err != nil {
			return fmt.Errorf("prune orphaned dm events: %w", err)
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

		pairs, err := s.dmPairs(ctx)
		if err != nil {
			return fmt.Errorf("list dm pairs: %w", err)
		}

		var dmTrimmed int64
		for _, pair := range pairs {
			n, err := s.pruneDMEvents(ctx, pair)
			if err != nil {
				return fmt.Errorf("prune dm thread between %q and %q: %w", pair.self, pair.peer, err)
			}
			dmTrimmed += n
		}

		span.SetAttributes(
			attribute.Int64("modeloff.retention.orphaned_removed", orphaned),
			attribute.Int64("modeloff.retention.dm_orphaned_removed", dmOrphaned),
			attribute.Int64("modeloff.retention.channel_trimmed", channelTrimmed),
			attribute.Int64("modeloff.retention.dm_trimmed", dmTrimmed),
		)

		if orphaned+dmOrphaned+channelTrimmed+dmTrimmed > 0 {
			slog.Default().InfoContext(ctx, "event retention pass",
				"component", "store.sqlite",
				"orphaned_removed", orphaned,
				"dm_orphaned_removed", dmOrphaned,
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

// pruneOrphanDMEvents removes DM-shaped event rows whose peer
// instance no longer has a row in instances. DeleteInstanceByID
// evicts the peer's own row but has no foreign key to cascade
// through to these rows (they are keyed by instance id inside the
// channel column, not a real reference), and instance ids are never
// reused, so nothing will ever address that thread again.
//
// A DM message row carries no channel prefix in its channel column
// (see domain.InferChannelKind): the direction the user sent has the
// peer's InstanceID there and an empty dm_instance_id, and the
// direction the peer sent has an empty channel and the peer's id in
// the generated dm_instance_id column instead (see DMEventsBefore's
// doc comment). Exactly one of the two names the peer for a given
// row, so the CASE picks whichever is non-empty. Every other
// persisted event type is logged under a real channel the actor was
// in, which starts with a channel prefix and so never matches this
// predicate, including the peer's own quit/nick_change rows
// DMEventsBefore replays into the thread, which pruneChannelEvents
// already governs (see pruneDMEvents's doc comment).
func (s *SQLiteStore) pruneOrphanDMEvents(ctx context.Context) (int64, error) {
	prefixes := []rune(domain.ChannelPrefixes)
	placeholders := make([]string, len(prefixes))
	args := make([]any, len(prefixes))

	for i, r := range prefixes {
		placeholders[i] = "?"
		args[i] = string(r)
	}

	const peer = `CASE WHEN channel != '' THEN channel ELSE dm_instance_id END`

	query := `DELETE FROM events WHERE id IN (
		SELECT id FROM events WHERE substr(channel, 1, 1) NOT IN (` + strings.Join(placeholders, ",") + `)
			AND ` + peer + ` != ''
			AND ` + peer + ` NOT IN (SELECT instance_id FROM instances)
			AND ` + cursorExclusion + `
		ORDER BY id LIMIT ?
	)`

	removed, err := deleteEventsBatched(ctx, s.db, query, args)
	if err != nil {
		return removed, err
	}

	if removed > 0 {
		slog.Default().InfoContext(ctx, "pruned orphaned dm event rows",
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

// dmCorrespondents is one unordered pair of clients with a DM thread
// between them, normalised so `self` is the BINARY-smaller of the two
// ids. Normalising is what makes the pair one conversation: without
// it the two directions of a thread enumerate as two pairs and each
// gets trimmed against the headroom on its own.
type dmCorrespondents struct {
	self domain.InstanceID
	peer domain.InstanceID
}

// dmPairs enumerates the distinct pairs of correspondents present in
// DM-shaped event rows.
//
// A DM row names both of them: `channel` carries the recipient's
// InstanceID and the generated `dm_instance_id` column carries the
// sender's. The pair therefore comes off the row itself, which is
// what makes a thread between two models visible here; the instances
// table names each client but says nothing about who talked to whom.
// Channel activity is excluded by the same channel-prefix test
// pruneOrphanDMEvents uses, since its `channel` column names a real
// channel and its `dm_instance_id` is only the sender.
//
// A row whose two ids are equal, which `/msg` against your own nick
// writes, enumerates as a pair with itself, and the thread predicate
// below then reads as the same half twice, which trims it correctly.
func (s *SQLiteStore) dmPairs(ctx context.Context) ([]dmCorrespondents, error) {
	prefixes := []rune(domain.ChannelPrefixes)
	placeholders := make([]string, len(prefixes))
	args := make([]any, len(prefixes))

	for i, r := range prefixes {
		placeholders[i] = "?"
		args[i] = string(r)
	}

	query := `SELECT DISTINCT
			min(channel, dm_instance_id) AS self,
			max(channel, dm_instance_id) AS peer
		FROM events
		WHERE substr(channel, 1, 1) NOT IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY self, peer`

	return queryRows(ctx, s.db, query, args, func(r rowScanner) (dmCorrespondents, error) {
		var pair dmCorrespondents

		err := r.Scan(&pair.self, &pair.peer)

		return pair, err
	})
}

// pruneDMEvents trims the message rows of one pair's DM thread down
// to eventRetentionHeadroom, matching the shape DMEventsBefore reads:
// `(channel = peer, dm_instance_id = self)` for a line self sent, or
// `(channel = self, dm_instance_id = peer)` for one peer sent back.
//
// The pair's actor-scoped events (quit, nick_change) that
// DMEventsBefore also replays into the thread are not counted here.
// Their `channel` column names the real channel the actor was in, not
// the DM, so they are governed by that channel's own
// pruneChannelEvents pass; eventRetentionHeadroom is generously past
// what either read needs, so leaving them out of the DM count does
// not risk trimming a thread down to fewer than the 500 events a
// model's attach-time load actually asks for.
func (s *SQLiteStore) pruneDMEvents(ctx context.Context, pair dmCorrespondents) (int64, error) {
	const thread = `(channel = ? AND dm_instance_id = ?) OR (channel = ? AND dm_instance_id = ?)`

	query := `DELETE FROM events WHERE id IN (
		SELECT id FROM events WHERE (` + thread + `) AND id NOT IN (
			SELECT id FROM events WHERE ` + thread + ` ORDER BY id DESC LIMIT ?
		) AND ` + cursorExclusion + `
		ORDER BY id LIMIT ?
	)`

	args := []any{
		pair.peer, pair.self, pair.self, pair.peer, // outer predicate
		pair.peer, pair.self, pair.self, pair.peer, // inner top-N subquery
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
