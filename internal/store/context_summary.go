package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// contextSummarySourceRetention bounds how many projected source rows
// survive per actor and window.
//
// summarizedRecentPrefix (internal/modelclient) matches the tail of a
// window's concatenated sources against that turn's recent messages,
// which are the window's history ring and its reply ring, each capped
// at modelHistorySize (internal/modelclient, 500). A source older than
// the newest 1000 can therefore never take part in a match. Raising
// modelHistorySize means raising this too.
const contextSummarySourceRetention = 2 * 500

// ContextSummaryID identifies one stored summary segment.
type ContextSummaryID int64

// ContextSummary is one compacted part of an actor's window context.
//
// Sources holds the messages the summary was written from, as the
// transcript held them. The projection that renders a message for one
// recipient is applied when a turn builds its request, so what is stored
// here is what the actor's own ring carried.
type ContextSummary struct {
	ID         ContextSummaryID
	InstanceID domain.InstanceID
	Window     protocol.WindowTarget
	Summary    string
	Sources    []protocol.IRCMessage
	CreatedAt  time.Time
}

// ContextSummaryUpdate appends projected sources and creates a
// summary over them. Supersedes folds existing segments into the new
// segment without copying their source rows.
type ContextSummaryUpdate struct {
	InstanceID domain.InstanceID
	Window     protocol.WindowTarget
	Summary    string
	Sources    []protocol.IRCMessage
	Supersedes []ContextSummaryID
	CreatedAt  time.Time
}

type contextSummaryRow struct {
	id          ContextSummaryID
	instanceID  domain.InstanceID
	windowKind  int
	windowKey   string
	summary     string
	firstSource int64
	lastSource  int64
	createdAt   time.Time
}

// CommitContextSummary creates or replaces summary segments in one
// transaction. Existing projected source rows remain in place when
// their summaries are superseded.
func (s *SQLiteStore) CommitContextSummary(
	ctx context.Context,
	update ContextSummaryUpdate,
) (ContextSummary, error) {
	kind, key := windowColumns(update.Window)
	supersedes := slices.Clone(update.Supersedes)
	slices.Sort(supersedes)
	supersedes = slices.Compact(supersedes)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ContextSummary{}, fmt.Errorf("begin context summary transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	firstSource, lastSource, err := contextSummaryRange(
		ctx, tx, update.InstanceID, kind, key, supersedes,
	)
	if err != nil {
		return ContextSummary{}, err
	}

	for _, source := range update.Sources {
		data, err := json.Marshal(source)
		if err != nil {
			return ContextSummary{}, fmt.Errorf("marshal context summary source: %w", err)
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO context_summary_sources
				(instance_id, window_kind, window_key, data, at)
			VALUES (?, ?, ?, ?, ?)
		`, update.InstanceID, kind, key, string(data), formatTime(source.At))
		if err != nil {
			return ContextSummary{}, fmt.Errorf("insert context summary source: %w", err)
		}
		sourceID, err := result.LastInsertId()
		if err != nil {
			return ContextSummary{}, fmt.Errorf("read context summary source id: %w", err)
		}
		if firstSource == 0 {
			firstSource = sourceID
		}
		lastSource = sourceID
	}
	if firstSource == 0 {
		return ContextSummary{}, fmt.Errorf("commit context summary: no sources")
	}

	createdAt := formatTime(update.CreatedAt)
	result, err := tx.ExecContext(ctx, `
		INSERT INTO context_summaries
			(instance_id, window_kind, window_key, summary, first_source_id, last_source_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, update.InstanceID, kind, key, update.Summary, firstSource, lastSource, createdAt)
	if err != nil {
		return ContextSummary{}, fmt.Errorf("insert context summary: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ContextSummary{}, fmt.Errorf("read context summary id: %w", err)
	}

	for _, superseded := range supersedes {
		if _, err := tx.ExecContext(ctx, `DELETE FROM context_summaries WHERE id = ?`, superseded); err != nil {
			return ContextSummary{}, fmt.Errorf("delete superseded context summary: %w", err)
		}
	}
	if err := trimContextSummarySourcesTx(ctx, tx, update.InstanceID, kind, key); err != nil {
		return ContextSummary{}, err
	}
	if err := tx.Commit(); err != nil {
		return ContextSummary{}, fmt.Errorf("commit context summary: %w", err)
	}

	return s.contextSummaryByID(ctx, ContextSummaryID(id))
}

// trimContextSummarySourcesTx keeps the newest
// contextSummarySourceRetention source rows for one actor window and
// raises each surviving summary's first_source_id to the oldest row
// that remains.
//
// context_summaries.first_source_id and last_source_id are foreign keys
// into context_summary_sources with no ON DELETE clause, so a summary's
// own endpoints have to survive. The oldest summary's last_source_id is
// therefore the floor on what this can remove.
func trimContextSummarySourcesTx(
	ctx context.Context,
	tx *sql.Tx,
	instanceID domain.InstanceID,
	windowKind int,
	windowKey string,
) error {
	var oldestRetained int64
	if err := tx.QueryRowContext(ctx, `
		WITH ranked AS (
			SELECT id, ROW_NUMBER() OVER (ORDER BY id DESC) AS position
			FROM context_summary_sources
			WHERE instance_id = ? AND window_kind = ? AND window_key = ?
		)
		SELECT COALESCE(MIN(id), 0) FROM ranked WHERE position <= ?
	`, instanceID, windowKind, windowKey,
		contextSummarySourceRetention).Scan(&oldestRetained); err != nil {
		return fmt.Errorf("read context summary source cutoff: %w", err)
	}

	var floor int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MIN(last_source_id), ?) FROM context_summaries
		WHERE instance_id = ? AND window_kind = ? AND window_key = ?
	`, oldestRetained, instanceID, windowKind, windowKey).Scan(&floor); err != nil {
		return fmt.Errorf("read context summary source floor: %w", err)
	}
	oldestRetained = min(oldestRetained, floor)

	if _, err := tx.ExecContext(ctx, `
		UPDATE context_summaries SET first_source_id = ?
		WHERE instance_id = ? AND window_kind = ? AND window_key = ?
		  AND first_source_id < ?
	`, oldestRetained, instanceID, windowKind, windowKey, oldestRetained); err != nil {
		return fmt.Errorf("clamp context summary source range: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM context_summary_sources
		WHERE instance_id = ? AND window_kind = ? AND window_key = ? AND id < ?
	`, instanceID, windowKind, windowKey, oldestRetained); err != nil {
		return fmt.Errorf("trim context summary sources: %w", err)
	}

	return nil
}

func contextSummaryRange(
	ctx context.Context,
	tx *sql.Tx,
	instanceID domain.InstanceID,
	windowKind int,
	windowKey string,
	supersedes []ContextSummaryID,
) (int64, int64, error) {
	// A summary must name every segment active for the window, an empty
	// list included. Skipping the check when nothing is superseded would
	// leave two summaries active for one window, each covering a range the
	// other does not, and a turn would read a fork.
	var active int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM context_summaries
		WHERE instance_id = ? AND window_kind = ? AND window_key = ?
	`, instanceID, windowKind, windowKey).Scan(&active); err != nil {
		return 0, 0, fmt.Errorf("count active context summaries: %w", err)
	}
	if active != len(supersedes) {
		return 0, 0, fmt.Errorf(
			"supersede context summaries: got %d of %d active segments",
			len(supersedes), active,
		)
	}

	var firstSource, lastSource int64
	for _, id := range supersedes {
		var rowInstance domain.InstanceID
		var rowKind int
		var rowKey string
		var first, last int64
		err := tx.QueryRowContext(ctx, `
			SELECT instance_id, window_kind, window_key, first_source_id, last_source_id
			FROM context_summaries WHERE id = ?
		`, id).Scan(&rowInstance, &rowKind, &rowKey, &first, &last)
		if err != nil {
			return 0, 0, fmt.Errorf("read superseded context summary %d: %w", id, err)
		}
		if rowInstance != instanceID || rowKind != windowKind || rowKey != windowKey {
			return 0, 0, fmt.Errorf("superseded context summary %d belongs to another window", id)
		}
		if firstSource == 0 || first < firstSource {
			firstSource = first
		}
		if last > lastSource {
			lastSource = last
		}
	}

	return firstSource, lastSource, nil
}

// ContextSummaries returns an actor window's active summary segments
// and their exact projected sources in creation order.
func (s *SQLiteStore) ContextSummaries(
	ctx context.Context,
	instanceID domain.InstanceID,
	window protocol.WindowTarget,
) ([]ContextSummary, error) {
	kind, key := windowColumns(window)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, instance_id, window_kind, window_key, summary,
		       first_source_id, last_source_id, created_at
		FROM context_summaries
		WHERE instance_id = ? AND window_kind = ? AND window_key = ?
		ORDER BY id
	`, instanceID, kind, key)
	if err != nil {
		return nil, fmt.Errorf("query context summaries: %w", err)
	}

	var summaryRows []contextSummaryRow
	for rows.Next() {
		var row contextSummaryRow
		var createdAt string
		if err := rows.Scan(
			&row.id, &row.instanceID, &row.windowKind, &row.windowKey,
			&row.summary, &row.firstSource, &row.lastSource, &createdAt,
		); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan context summary: %w", err)
		}
		row.createdAt, err = parseTime(createdAt)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("parse context summary time: %w", err)
		}
		summaryRows = append(summaryRows, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("read context summaries: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close context summaries: %w", err)
	}

	summaries := make([]ContextSummary, 0, len(summaryRows))
	for _, row := range summaryRows {
		summary, err := s.contextSummary(ctx, row)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, summary)
	}

	return summaries, nil
}

func (s *SQLiteStore) contextSummaryByID(ctx context.Context, id ContextSummaryID) (ContextSummary, error) {
	var row contextSummaryRow
	var createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, instance_id, window_kind, window_key, summary,
		       first_source_id, last_source_id, created_at
		FROM context_summaries WHERE id = ?
	`, id).Scan(
		&row.id, &row.instanceID, &row.windowKind, &row.windowKey,
		&row.summary, &row.firstSource, &row.lastSource, &createdAt,
	)
	if err != nil {
		return ContextSummary{}, fmt.Errorf("read context summary: %w", err)
	}
	row.createdAt, err = parseTime(createdAt)
	if err != nil {
		return ContextSummary{}, fmt.Errorf("parse context summary time: %w", err)
	}

	return s.contextSummary(ctx, row)
}

func (s *SQLiteStore) contextSummary(ctx context.Context, row contextSummaryRow) (ContextSummary, error) {
	window, err := windowFromColumns(row.windowKind, row.windowKey)
	if err != nil {
		return ContextSummary{}, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT data FROM context_summary_sources
		WHERE instance_id = ? AND window_kind = ? AND window_key = ?
		  AND id BETWEEN ? AND ?
		ORDER BY id
	`, row.instanceID, row.windowKind, row.windowKey, row.firstSource, row.lastSource)
	if err != nil {
		return ContextSummary{}, fmt.Errorf("query context summary sources: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sources []protocol.IRCMessage
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return ContextSummary{}, fmt.Errorf("scan context summary source: %w", err)
		}
		var source protocol.IRCMessage
		if err := json.Unmarshal([]byte(data), &source); err != nil {
			return ContextSummary{}, fmt.Errorf("decode context summary source: %w", err)
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return ContextSummary{}, fmt.Errorf("read context summary sources: %w", err)
	}

	return ContextSummary{
		ID: row.id, InstanceID: row.instanceID, Window: window,
		Summary: row.summary, Sources: sources, CreatedAt: row.createdAt,
	}, nil
}

// DeleteContextSummariesForWindow removes every summary and source
// copy for one actor window.
func (s *SQLiteStore) DeleteContextSummariesForWindow(
	ctx context.Context,
	instanceID domain.InstanceID,
	window protocol.WindowTarget,
) error {
	kind, key := windowColumns(window)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin context summary deletion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := deleteContextSummariesForWindowTx(ctx, tx, instanceID, kind, key); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit context summary deletion: %w", err)
	}

	return nil
}

func deleteContextSummariesForWindowTx(
	ctx context.Context,
	tx *sql.Tx,
	instanceID domain.InstanceID,
	windowKind int,
	windowKey string,
) error {
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM context_summaries
		WHERE instance_id = ? AND window_kind = ? AND window_key = ?
	`, instanceID, windowKind, windowKey); err != nil {
		return fmt.Errorf("delete context summaries: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM context_summary_sources
		WHERE instance_id = ? AND window_kind = ? AND window_key = ?
	`, instanceID, windowKind, windowKey); err != nil {
		return fmt.Errorf("delete context summary sources: %w", err)
	}

	return nil
}

func deleteContextSummariesForActorTx(
	ctx context.Context,
	tx *sql.Tx,
	instanceID domain.InstanceID,
) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM context_summaries WHERE instance_id = ?`, instanceID); err != nil {
		return fmt.Errorf("delete actor context summaries: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM context_summary_sources WHERE instance_id = ?`, instanceID); err != nil {
		return fmt.Errorf("delete actor context summary sources: %w", err)
	}

	return nil
}
