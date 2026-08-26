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

// reflectionRowQueryer is the single-row query surface `*sql.DB` and
// `*sql.Tx` share, so one status read serves both the standalone call and
// the snapshot transaction.
type reflectionRowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

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

// ReflectionInboxStatus describes everything an instance has pending after
// its active persona checkpoint. HighWaterMark is the newest candidate
// sequence in the whole table, so it moves whenever a new event is
// recorded however large the backlog already is. LastAttemptAt is the
// finish time of the instance's most recent reflection run whatever its
// outcome, which is what paces attempts.
type ReflectionInboxStatus struct {
	Checkpoint        domain.ReflectionSequence
	HighWaterMark     domain.ReflectionSequence
	PendingEvents     int
	SubstantiveEvents int
	LastAttemptAt     *time.Time
}

// ReflectionRange is the bounded page of candidates one reflection attempt
// reads. Through is the last sequence in Events, which is as far as an
// accepted commit may advance the checkpoint. It is behind
// [ReflectionInboxStatus.HighWaterMark] whenever the backlog is longer
// than the requested limit.
type ReflectionRange struct {
	Checkpoint domain.ReflectionSequence
	Through    domain.ReflectionSequence
}

// PendingReflectionSnapshot is one coherent persona lineage and bounded event
// range for a reflection attempt.
type PendingReflectionSnapshot struct {
	Persona PersonaSnapshot
	Events  []ReflectionEvent
	Range   ReflectionRange
	Status  ReflectionInboxStatus
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
		event, err := scanReflectionEvent(rows, instanceID)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read reflection events: %w", err)
	}

	return events, nil
}

// PendingReflectionSnapshot reads the active persona revision and the next
// bounded reflection range in one read transaction.
func (s *SQLiteStore) PendingReflectionSnapshot(
	ctx context.Context,
	instanceID domain.InstanceID,
	limit int,
) (PendingReflectionSnapshot, error) {
	if limit <= 0 {
		return PendingReflectionSnapshot{}, fmt.Errorf(
			"read pending reflection snapshot: invalid event limit %d", limit,
		)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PendingReflectionSnapshot{}, fmt.Errorf(
			"begin pending reflection snapshot: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()

	state, err := personaLineageTx(ctx, tx, instanceID)
	if err != nil {
		return PendingReflectionSnapshot{}, err
	}
	revision, err := personaRevisionTx(ctx, tx, state.CurrentRevisionID)
	if err != nil {
		return PendingReflectionSnapshot{}, err
	}
	experiences, err := personaExperiencesTx(ctx, tx, revision.ExperienceIDs)
	if err != nil {
		return PendingReflectionSnapshot{}, err
	}
	amendments, err := personaAmendmentsTx(ctx, tx, revision.AmendmentIDs)
	if err != nil {
		return PendingReflectionSnapshot{}, err
	}
	events, err := reflectionEventsFrom(
		ctx, tx, instanceID, state.Checkpoint, limit,
	)
	if err != nil {
		return PendingReflectionSnapshot{}, err
	}
	status, err := readReflectionInboxStatus(ctx, tx, instanceID, state.Checkpoint)
	if err != nil {
		return PendingReflectionSnapshot{}, err
	}

	page := ReflectionRange{Checkpoint: state.Checkpoint, Through: state.Checkpoint}
	if len(events) > 0 {
		page.Through = events[len(events)-1].Sequence
	}

	return PendingReflectionSnapshot{
		Persona: PersonaSnapshot{
			Lineage: state, Revision: revision,
			Experiences: experiences, Amendments: amendments,
		},
		Events: events,
		Range:  page,
		Status: status,
	}, nil
}

func reflectionEventsFrom(
	ctx context.Context,
	db rowsQueryer,
	instanceID domain.InstanceID,
	after domain.ReflectionSequence,
	limit int,
) ([]ReflectionEvent, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT sequence, source_kind, source_id, window_kind, window_key,
		       message, substantive, created_at
		FROM reflection_events
		WHERE instance_id = ? AND sequence > ?
		ORDER BY sequence
		LIMIT ?
	`, instanceID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read pending reflection events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	events := []ReflectionEvent{}
	for rows.Next() {
		event, err := scanReflectionEvent(rows, instanceID)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pending reflection events: %w", err)
	}

	return events, nil
}

func scanReflectionEvent(
	row rowScanner,
	instanceID domain.InstanceID,
) (ReflectionEvent, error) {
	var event ReflectionEvent
	var sourceKind protocol.HistorySourceKind
	var sourceID int64
	var windowKind int
	var windowKey string
	var message string
	var createdAt string
	if err := row.Scan(
		&event.Sequence, &sourceKind, &sourceID, &windowKind, &windowKey,
		&message, &event.Substantive, &createdAt,
	); err != nil {
		return ReflectionEvent{}, fmt.Errorf("scan reflection event: %w", err)
	}
	window, err := windowFromColumns(windowKind, windowKey)
	if err != nil {
		return ReflectionEvent{}, fmt.Errorf(
			"read reflection event %d: %w", event.Sequence, err,
		)
	}
	if err := json.Unmarshal([]byte(message), &event.Message); err != nil {
		return ReflectionEvent{}, fmt.Errorf(
			"decode reflection event %d: %w", event.Sequence, err,
		)
	}
	event.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return ReflectionEvent{}, fmt.Errorf(
			"parse reflection event %d creation time: %w", event.Sequence, err,
		)
	}
	event.InstanceID = instanceID
	event.Source = protocol.HistoryRef{Kind: sourceKind, ID: sourceID, Window: window}

	return event, nil
}

// ReflectionInboxStatus returns the candidate range after the active persona
// checkpoint.
func (s *SQLiteStore) ReflectionInboxStatus(
	ctx context.Context,
	instanceID domain.InstanceID,
) (ReflectionInboxStatus, error) {
	var checkpoint domain.ReflectionSequence
	err := s.db.QueryRowContext(ctx, `
		SELECT checkpoint FROM persona_lineages WHERE instance_id = ?
	`, instanceID).Scan(&checkpoint)
	if errors.Is(err, sql.ErrNoRows) {
		return ReflectionInboxStatus{}, ErrNoPersonaLineage
	}
	if err != nil {
		return ReflectionInboxStatus{}, fmt.Errorf("read reflection checkpoint: %w", err)
	}

	return readReflectionInboxStatus(ctx, s.db, instanceID, checkpoint)
}

// readReflectionInboxStatus counts what is pending after checkpoint and
// reads the instance's most recent reflection attempt. Both the standalone
// status read and the snapshot read go through it, so a scheduler cannot be
// told one high-water mark here and a different one there.
func readReflectionInboxStatus(
	ctx context.Context,
	db reflectionRowQueryer,
	instanceID domain.InstanceID,
	checkpoint domain.ReflectionSequence,
) (ReflectionInboxStatus, error) {
	status := ReflectionInboxStatus{Checkpoint: checkpoint}
	err := db.QueryRowContext(ctx, `
		SELECT coalesce(max(sequence), 0),
		       count(*) FILTER (WHERE sequence > ?),
		       count(*) FILTER (WHERE sequence > ? AND substantive)
		FROM reflection_events
		WHERE instance_id = ?
	`, checkpoint, checkpoint, instanceID).Scan(
		&status.HighWaterMark, &status.PendingEvents, &status.SubstantiveEvents,
	)
	if err != nil {
		return ReflectionInboxStatus{}, fmt.Errorf("read reflection inbox status: %w", err)
	}

	var lastAttemptAt sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT max(finished_at) FROM reflection_runs WHERE instance_id = ?
	`, instanceID).Scan(&lastAttemptAt); err != nil {
		return ReflectionInboxStatus{}, fmt.Errorf("read last reflection attempt: %w", err)
	}
	if lastAttemptAt.Valid {
		at, err := time.Parse(time.RFC3339Nano, lastAttemptAt.String)
		if err != nil {
			return ReflectionInboxStatus{}, fmt.Errorf(
				"parse last reflection attempt time: %w", err,
			)
		}
		status.LastAttemptAt = &at
	}

	return status, nil
}
