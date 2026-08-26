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
	// column) or a DM thread (matched via the source_instance_id
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

	// modelTurnRetentionHeadroom bounds the full rendered requests
	// retained for each actor. A turn duplicates its prompt and
	// transcript, so a per-window limit would still let an actor grow
	// the database by opening more windows. The byte bound retains the
	// newest turn even when that request alone exceeds it, because an
	// admitted turn must remain appendable until it finishes.
	modelTurnRetentionHeadroom = 500
	modelTurnRetentionBytes    = 64 << 20

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

// retentionPass is one bound the store enforces, named by the attribute
// and log key it reports under.
type retentionPass struct {
	name string
	run  func(context.Context) (int64, error)
}

// retentionPasses is every per-table bound, in the order the pass runs
// them. A new bounded table is one entry here.
func (s *SQLiteStore) retentionPasses() []retentionPass {
	return []retentionPass{
		{"channel", s.pruneAllChannelEvents},
		{"channel_scrollback", s.pruneChannelScrollback},
		{"instance_replies", s.pruneInstanceReplies},
		{"dm", s.pruneAllDMEvents},
		{"model_turns", s.pruneModelTurns},
	}
}

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
		orphans, err := s.pruneOrphans(ctx)
		if err != nil {
			return err
		}

		trimmed := make(map[string]int64, len(s.retentionPasses()))
		var total int64
		for _, pass := range s.retentionPasses() {
			removed, err := pass.run(ctx)
			if err != nil {
				return fmt.Errorf("prune %s: %w", pass.name, err)
			}
			trimmed[pass.name] = removed
			total += removed
		}

		attrs := []attribute.KeyValue{
			attribute.Int64("modeloff.retention.orphaned_removed", orphans.channelEvents),
			attribute.Int64("modeloff.retention.dm_orphaned_removed", orphans.dmEvents),
			attribute.Int64("modeloff.retention.channel_scrollback_orphaned_removed", orphans.channelScrollback),
			attribute.Int64("modeloff.retention.instance_replies_orphaned_removed", orphans.instanceReplies),
			attribute.Int64("modeloff.retention.model_turns_orphaned_removed", orphans.modelTurns),
			attribute.Int64("modeloff.retention.context_summaries_orphaned_removed", orphans.contextSummaries),
		}
		fields := []any{
			"component", "store.sqlite",
			"orphaned_removed", orphans.channelEvents,
			"dm_orphaned_removed", orphans.dmEvents,
			"channel_scrollback_orphaned_removed", orphans.channelScrollback,
			"instance_replies_orphaned_removed", orphans.instanceReplies,
			"model_turns_orphaned_removed", orphans.modelTurns,
			"context_summaries_orphaned_removed", orphans.contextSummaries,
		}
		for _, pass := range s.retentionPasses() {
			attrs = append(attrs, attribute.Int64(
				"modeloff.retention."+pass.name+"_trimmed", trimmed[pass.name],
			))
			fields = append(fields, pass.name+"_trimmed", trimmed[pass.name])
		}
		span.SetAttributes(attrs...)

		if orphans.total()+total > 0 {
			slog.Default().InfoContext(ctx, "event retention pass", fields...)
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

// pruneAllChannelEvents trims every channel's event log, which is bounded
// per channel rather than per instance.
func (s *SQLiteStore) pruneAllChannelEvents(ctx context.Context) (int64, error) {
	names, err := queryRows(ctx, s.db, `SELECT name FROM channels ORDER BY name`, nil,
		scalarColumn[domain.ChannelName]())
	if err != nil {
		return 0, fmt.Errorf("list channels: %w", err)
	}

	var trimmed int64
	for _, name := range names {
		removed, err := s.pruneChannelEvents(ctx, name)
		if err != nil {
			return 0, fmt.Errorf("channel %q: %w", name, err)
		}
		trimmed += removed
	}

	return trimmed, nil
}

// pruneAllDMEvents trims every direct-message thread, which is bounded per
// pair of correspondents.
func (s *SQLiteStore) pruneAllDMEvents(ctx context.Context) (int64, error) {
	pairs, err := s.dmPairs(ctx)
	if err != nil {
		return 0, fmt.Errorf("list dm pairs: %w", err)
	}

	var trimmed int64
	for _, pair := range pairs {
		removed, err := s.pruneDMEvents(ctx, pair)
		if err != nil {
			return 0, fmt.Errorf("thread between %q and %q: %w", pair.self, pair.peer, err)
		}
		trimmed += removed
	}

	return trimmed, nil
}

type orphanRetention struct {
	channelEvents     int64
	dmEvents          int64
	channelScrollback int64
	instanceReplies   int64
	modelTurns        int64
	contextSummaries  int64
}

func (r orphanRetention) total() int64 {
	return r.channelEvents + r.dmEvents + r.channelScrollback +
		r.instanceReplies + r.modelTurns + r.contextSummaries
}

func (s *SQLiteStore) pruneOrphans(ctx context.Context) (orphanRetention, error) {
	var removed orphanRetention
	var err error

	removed.channelEvents, err = s.pruneOrphanChannelEvents(ctx)
	if err != nil {
		return orphanRetention{}, fmt.Errorf("prune orphaned channel events: %w", err)
	}

	removed.dmEvents, err = s.pruneOrphanDMEvents(ctx)
	if err != nil {
		return orphanRetention{}, fmt.Errorf("prune orphaned dm events: %w", err)
	}

	removed.channelScrollback, err = s.pruneOrphanChannelScrollback(ctx)
	if err != nil {
		return orphanRetention{}, fmt.Errorf("prune orphaned channel scrollback: %w", err)
	}

	removed.instanceReplies, err = s.pruneOrphanInstanceReplies(ctx)
	if err != nil {
		return orphanRetention{}, fmt.Errorf("prune orphaned instance replies: %w", err)
	}

	removed.modelTurns, err = s.pruneOrphanModelTurns(ctx)
	if err != nil {
		return orphanRetention{}, fmt.Errorf("prune orphaned model turns: %w", err)
	}

	removed.contextSummaries, err = s.pruneOrphanContextSummaries(ctx)
	if err != nil {
		return orphanRetention{}, fmt.Errorf("prune orphaned context summaries: %w", err)
	}

	return removed, nil
}

// contextSummaryOrphanPredicate matches a summary or source row naming
// an actor with no instances row, or a channel with no channels row,
// under the column names both tables share. A departed actor's rows are removed by the departure
// transaction; these are the ones a run that ended without one left
// behind. Neither table has a foreign key to channels or to instances,
// so nothing else reaches them.
const contextSummaryOrphanPredicate = `
	row.instance_id = ''
	OR row.instance_id NOT IN (SELECT instance_id FROM instances)
	OR (row.window_kind = 2 AND row.window_key != ''
		AND row.window_key NOT IN (SELECT instance_id FROM instances))
	OR (row.window_kind = 1 AND NOT EXISTS (
		SELECT 1 FROM channels AS channel
		WHERE channel.name = row.window_key COLLATE NOCASE
			AND (
				EXISTS (
					SELECT 1 FROM json_each(channel.data, '$.Members') AS member
					WHERE json_extract(member.value, '$.instance_id') = row.instance_id
				)
				OR EXISTS (
					SELECT 1 FROM json_each(channel.data, '$.Invitations') AS invitation
					WHERE invitation.value = row.instance_id
				)
			)
	))`

// pruneOrphanContextSummaries removes the summaries first so no
// surviving first_source_id or last_source_id references a source row
// the second statement deletes.
func (s *SQLiteStore) pruneOrphanContextSummaries(ctx context.Context) (int64, error) {
	summaries, err := deleteEventsBatched(ctx, s.db, `DELETE FROM context_summaries WHERE id IN (
		SELECT row.id FROM context_summaries AS row
		WHERE `+contextSummaryOrphanPredicate+`
		ORDER BY row.id LIMIT ?
	)`, nil)
	if err != nil {
		return summaries, err
	}

	sources, err := deleteEventsBatched(ctx, s.db, `DELETE FROM context_summary_sources WHERE id IN (
		SELECT row.id FROM context_summary_sources AS row
		WHERE `+contextSummaryOrphanPredicate+`
		ORDER BY row.id LIMIT ?
	)`, nil)

	return summaries + sources, err
}

func (s *SQLiteStore) pruneOrphanChannelScrollback(ctx context.Context) (int64, error) {
	query := `DELETE FROM channel_scrollback WHERE id IN (
		SELECT id FROM channel_scrollback
		WHERE instance_id != ''
			AND instance_id NOT IN (SELECT instance_id FROM instances)
		ORDER BY id LIMIT ?
	)`

	return deleteEventsBatched(ctx, s.db, query, nil)
}

func (s *SQLiteStore) pruneOrphanInstanceReplies(ctx context.Context) (int64, error) {
	query := `DELETE FROM instance_replies WHERE id IN (
		SELECT id FROM instance_replies
		WHERE (instance_id != ''
				AND instance_id NOT IN (SELECT instance_id FROM instances))
			OR (window_kind = 2 AND window_key != ''
				AND window_key NOT IN (SELECT instance_id FROM instances))
		ORDER BY id LIMIT ?
	)`

	return deleteEventsBatched(ctx, s.db, query, nil)
}

func (s *SQLiteStore) pruneOrphanModelTurns(ctx context.Context) (int64, error) {
	query := `DELETE FROM model_turns WHERE id IN (
		SELECT turn.id FROM model_turns AS turn
		WHERE turn.instance_id = ''
			OR turn.instance_id NOT IN (SELECT instance_id FROM instances)
			OR (turn.window_kind = 2 AND turn.window_key != ''
				AND turn.window_key NOT IN (SELECT instance_id FROM instances))
			OR (turn.window_kind = 1 AND NOT EXISTS (
				SELECT 1 FROM channels AS channel
				WHERE channel.name = turn.window_key COLLATE NOCASE
					AND (
						EXISTS (
							SELECT 1 FROM json_each(channel.data, '$.Members') AS member
							WHERE json_extract(member.value, '$.instance_id') = turn.instance_id
						)
						OR EXISTS (
							SELECT 1 FROM json_each(channel.data, '$.Invitations') AS invitation
							WHERE invitation.value = turn.instance_id
						)
					)
			))
		ORDER BY turn.id LIMIT ?
	)`

	return deleteEventsBatched(ctx, s.db, query, nil)
}

func (s *SQLiteStore) pruneModelTurns(ctx context.Context) (int64, error) {
	return s.pruneModelTurnsTo(
		ctx, modelTurnRetentionHeadroom, modelTurnRetentionBytes,
	)
}

func (s *SQLiteStore) pruneModelTurnsTo(
	ctx context.Context,
	maxTurns int,
	maxBytes int64,
) (int64, error) {
	query := `DELETE FROM model_turns WHERE id IN (
		WITH turn_sizes AS (
			SELECT turn.id, turn.instance_id,
				COALESCE(SUM(length(CAST(entry.data AS BLOB))), 0) AS bytes
			FROM model_turns AS turn
			LEFT JOIN model_turn_entries AS entry ON entry.turn_id = turn.id
			GROUP BY turn.id
		), ranked AS (
			SELECT id,
				ROW_NUMBER() OVER (
					PARTITION BY instance_id ORDER BY id DESC
				) AS position,
				SUM(bytes) OVER (
					PARTITION BY instance_id ORDER BY id DESC
				) AS retained_bytes
			FROM turn_sizes
		)
		SELECT id FROM ranked
		WHERE position > ? OR (position > 1 AND retained_bytes > ?)
		ORDER BY id LIMIT ?
	)`

	return deleteEventsBatched(ctx, s.db, query, []any{
		maxTurns,
		maxBytes,
	})
}

func (s *SQLiteStore) pruneChannelScrollback(ctx context.Context) (int64, error) {
	query := `DELETE FROM channel_scrollback WHERE id IN (
		SELECT id FROM (
			SELECT id, row_number() OVER (
				PARTITION BY instance_id, channel ORDER BY id DESC
			) AS position
			FROM channel_scrollback
		) WHERE position > ? LIMIT ?
	)`

	return deleteEventsBatched(ctx, s.db, query, []any{eventRetentionHeadroom})
}

func (s *SQLiteStore) pruneInstanceReplies(ctx context.Context) (int64, error) {
	query := `DELETE FROM instance_replies WHERE id IN (
		SELECT id FROM (
			SELECT id, row_number() OVER (
				PARTITION BY instance_id, window_kind, window_key ORDER BY id DESC
			) AS position
			FROM instance_replies
		) WHERE position > ? LIMIT ?
	)`

	return deleteEventsBatched(ctx, s.db, query, []any{eventRetentionHeadroom})
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

// pruneOrphanDMEvents removes DM-shaped event rows when either
// non-user correspondent no longer has a row in instances.
// DeleteInstanceByID has no foreign key through which to cascade to
// these rows, and instance ids are never reused, so nothing will
// address that thread again.
//
// A DM message row carries no channel prefix in its channel column
// (see domain.InferChannelKind). The direction the user sent has the
// peer's InstanceID there and an empty source_instance_id; the direction
// the peer sent has an empty channel and the peer's id in
// source_instance_id. A model-to-model row has both columns populated,
// so both must still resolve. Empty is the user sentinel and remains
// eligible. Every other persisted event type is logged under a real
// channel the actor was in, which starts with a channel prefix and
// never matches this predicate.
func (s *SQLiteStore) pruneOrphanDMEvents(ctx context.Context) (int64, error) {
	prefixes := []rune(domain.ChannelPrefixes)
	placeholders := make([]string, len(prefixes))
	args := make([]any, len(prefixes))

	for i, r := range prefixes {
		placeholders[i] = "?"
		args[i] = string(r)
	}

	query := `DELETE FROM events WHERE id IN (
		SELECT id FROM events WHERE substr(channel, 1, 1) NOT IN (` + strings.Join(placeholders, ",") + `)
			AND ((channel != '' AND channel NOT IN (SELECT instance_id FROM instances))
				OR (source_instance_id != '' AND source_instance_id NOT IN (SELECT instance_id FROM instances)))
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
// InstanceID and the generated `source_instance_id` column carries the
// sender's. The pair therefore comes off the row itself, which is
// what makes a thread between two models visible here; the instances
// table names each client but says nothing about who talked to whom.
// Channel activity is excluded by the same channel-prefix test
// pruneOrphanDMEvents uses, since its `channel` column names a real
// channel and its `source_instance_id` is only the sender.
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
			min(channel, source_instance_id) AS self,
			max(channel, source_instance_id) AS peer
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
// `(channel = peer, source_instance_id = self)` for a line self sent, or
// `(channel = self, source_instance_id = peer)` for one peer sent back.
func (s *SQLiteStore) pruneDMEvents(ctx context.Context, pair dmCorrespondents) (int64, error) {
	const thread = `(channel = ? AND source_instance_id = ?) OR (channel = ? AND source_instance_id = ?)`

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
