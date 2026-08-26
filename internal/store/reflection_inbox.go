package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

const reflectionEventRetentionHeadroom = 2000

// ReflectionEventCandidate is one projected event awaiting reflection.
type ReflectionEventCandidate struct {
	Source      protocol.HistoryRef
	Message     protocol.IRCMessage
	Substantive bool
}

// ReflectionEvent is one ordered entry in an instance's private reflection
// stream.
type ReflectionEvent struct {
	Sequence    domain.ReflectionSequence
	InstanceID  domain.InstanceID
	Source      protocol.HistoryRef
	Message     protocol.IRCMessage
	Substantive bool
	CreatedAt   time.Time
}

// ReflectionInboxStatus describes the closed range available to a reflection
// scheduler.
type ReflectionInboxStatus struct {
	Checkpoint        domain.ReflectionSequence
	HighWaterMark     domain.ReflectionSequence
	PendingEvents     int
	SubstantiveEvents int
	ReflectedAt       *time.Time
}

// AppendReflectionEvents records projected events once. The insert
// conflicts on the instance, the history reference, the window and the
// rendered message together, so replaying a source the client has already
// filed allocates no second sequence.
func (s *SQLiteStore) AppendReflectionEvents(
	ctx context.Context,
	instanceID domain.InstanceID,
	candidates []ReflectionEventCandidate,
	createdAt time.Time,
) error {
	if len(candidates) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reflection event append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, candidate := range candidates {
		if err := appendReflectionEventTx(ctx, tx, instanceID, candidate, createdAt); err != nil {
			return err
		}
	}
	if err := trimReflectionEventsTx(ctx, tx, instanceID); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reflection event append: %w", err)
	}

	return nil
}

// trimReflectionEventsTx keeps the newest
// reflectionEventRetentionHeadroom events that the instance's own
// accepted experiences do not cite.
//
// The cited sequences are gathered once into their own CTE so the
// per-event test is a lookup into that set. Written as a correlated
// `NOT EXISTS` over persona_experience_sources it reads more directly
// but matches on `sequence` alone against a `(experience_id, sequence)`
// primary key, which SQLite can only answer by scanning the whole table
// once per candidate event.
func trimReflectionEventsTx(
	ctx context.Context,
	tx *sql.Tx,
	instanceID domain.InstanceID,
) error {
	_, err := tx.ExecContext(ctx, `
		WITH cited AS (
			SELECT source.sequence
			FROM persona_experiences AS experience
			JOIN persona_experience_sources AS source
			  ON source.experience_id = experience.id
			WHERE experience.instance_id = ?
		), ranked AS (
			SELECT event.sequence,
				ROW_NUMBER() OVER (ORDER BY event.sequence DESC) AS position
			FROM reflection_events AS event
			WHERE event.instance_id = ?
			  AND event.sequence NOT IN (SELECT sequence FROM cited)
		)
		DELETE FROM reflection_events WHERE sequence IN (
			SELECT sequence FROM ranked WHERE position > ?
		)
	`, instanceID, instanceID, reflectionEventRetentionHeadroom)
	if err != nil {
		return fmt.Errorf("trim reflection events: %w", err)
	}

	return nil
}

func appendReflectionEventTx(
	ctx context.Context,
	tx *sql.Tx,
	instanceID domain.InstanceID,
	candidate ReflectionEventCandidate,
	createdAt time.Time,
) error {
	if candidate.Source.ID <= 0 {
		return fmt.Errorf("append reflection event: invalid source id %d", candidate.Source.ID)
	}
	if candidate.Source.Kind != protocol.HistorySourceEvent &&
		candidate.Source.Kind != protocol.HistorySourceChannelScrollback {
		return fmt.Errorf("append reflection event: invalid source kind %d", candidate.Source.Kind)
	}
	if candidate.Source.Window == nil {
		return fmt.Errorf("append reflection event: missing window")
	}

	message, err := json.Marshal(candidate.Message)
	if err != nil {
		return fmt.Errorf("marshal reflection event: %w", err)
	}
	windowKind, windowKey := windowColumns(candidate.Source.Window)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO reflection_events
			(instance_id, source_kind, source_id, window_kind, window_key,
			 message, substantive, event_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (
			instance_id, source_kind, source_id, window_kind, window_key, message
		) DO NOTHING
	`,
		instanceID, candidate.Source.Kind, candidate.Source.ID, windowKind, windowKey,
		string(message), candidate.Substantive,
		candidate.Message.At.Format(time.RFC3339Nano),
		createdAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("append reflection event: %w", err)
	}

	return nil
}

// ReflectionEvents reads one closed candidate range in sequence order.
func (s *SQLiteStore) ReflectionEvents(
	ctx context.Context,
	instanceID domain.InstanceID,
	after, through domain.ReflectionSequence,
	limit int,
) ([]ReflectionEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sequence, source_kind, source_id, window_kind, window_key,
		       message, substantive, created_at
		FROM reflection_events
		WHERE instance_id = ? AND sequence > ? AND sequence <= ?
		ORDER BY sequence
		LIMIT ?
	`, instanceID, after, through, limit)
	if err != nil {
		return nil, fmt.Errorf("read reflection events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	events := []ReflectionEvent{}
	for rows.Next() {
		var event ReflectionEvent
		var sourceKind protocol.HistorySourceKind
		var sourceID int64
		var windowKind int
		var windowKey string
		var message string
		var createdAt string
		if err := rows.Scan(
			&event.Sequence, &sourceKind, &sourceID, &windowKind, &windowKey,
			&message, &event.Substantive, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan reflection event: %w", err)
		}
		window, err := windowFromColumns(windowKind, windowKey)
		if err != nil {
			return nil, fmt.Errorf("read reflection event %d: %w", event.Sequence, err)
		}
		if err := json.Unmarshal([]byte(message), &event.Message); err != nil {
			return nil, fmt.Errorf("decode reflection event %d: %w", event.Sequence, err)
		}
		event.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse reflection event %d creation time: %w", event.Sequence, err)
		}
		event.InstanceID = instanceID
		event.Source = protocol.HistoryRef{Kind: sourceKind, ID: sourceID, Window: window}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read reflection events: %w", err)
	}

	return events, nil
}

// ReflectionInboxStatus returns the candidate range after the active persona
// checkpoint.
func (s *SQLiteStore) ReflectionInboxStatus(
	ctx context.Context,
	instanceID domain.InstanceID,
) (ReflectionInboxStatus, error) {
	var status ReflectionInboxStatus
	var reflectedAt sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT checkpoint, reflected_at
		FROM persona_lineages
		WHERE instance_id = ?
	`, instanceID).Scan(&status.Checkpoint, &reflectedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ReflectionInboxStatus{}, ErrNoPersonaLineage
	}
	if err != nil {
		return ReflectionInboxStatus{}, fmt.Errorf("read reflection checkpoint: %w", err)
	}
	if reflectedAt.Valid {
		at, err := time.Parse(time.RFC3339Nano, reflectedAt.String)
		if err != nil {
			return ReflectionInboxStatus{}, fmt.Errorf("parse reflection time: %w", err)
		}
		status.ReflectedAt = &at
	}

	err = s.db.QueryRowContext(ctx, `
		SELECT coalesce(max(sequence), 0),
		       count(*) FILTER (WHERE sequence > ?),
		       count(*) FILTER (WHERE sequence > ? AND substantive)
		FROM reflection_events
		WHERE instance_id = ?
	`, status.Checkpoint, status.Checkpoint, instanceID).Scan(
		&status.HighWaterMark, &status.PendingEvents, &status.SubstantiveEvents,
	)
	if err != nil {
		return ReflectionInboxStatus{}, fmt.Errorf("read reflection inbox status: %w", err)
	}

	return status, nil
}
