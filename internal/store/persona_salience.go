package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/laney/modeloff/internal/domain"
)

// backfillExperienceSalience gives every experience written before the
// columns existed a citation time and the ordering key that follows from
// it. An experience nothing has cited counts as cited when it was created,
// which is the moment the reflection that accepted it drew on the events
// behind it.
func backfillExperienceSalience(ctx context.Context, tx *sql.Tx) error {
	type row struct {
		id          domain.ExperienceID
		confidence  domain.Confidence
		lastCitedAt string
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT id, confidence, created_at FROM persona_experiences
	`)
	if err != nil {
		return fmt.Errorf("read persona experiences: %w", err)
	}
	defer func() { _ = rows.Close() }()

	pending := []row{}
	for rows.Next() {
		var next row
		if err := rows.Scan(&next.id, &next.confidence, &next.lastCitedAt); err != nil {
			return fmt.Errorf("read persona experiences: %w", err)
		}
		pending = append(pending, next)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read persona experiences: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("read persona experiences: %w", err)
	}

	for _, next := range pending {
		citedAt, err := parseTime(next.lastCitedAt)
		if err != nil {
			return fmt.Errorf("parse persona experience %d created time: %w", next.id, err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE persona_experiences
			SET last_cited_at = ?, salience_at = ?
			WHERE id = ?
		`,
			formatTime(citedAt),
			formatTime(domain.SalienceAt(next.confidence, citedAt)),
			next.id,
		); err != nil {
			return fmt.Errorf("backfill persona experience %d salience: %w", next.id, err)
		}
	}

	return nil
}

// experienceRetentionHeadroom bounds the experiences retained for each
// instance.
//
// Nothing else removes one. Consolidation marks a tendency and leaves the
// experiences behind it in place, and an experience carries no expiry,
// because the citations are the audit trail every accepted change rests
// on. This is the storage backstop over what nothing has ever drawn on,
// and it keeps far more than any one prompt or reflection request reads.
const experienceRetentionHeadroom = 500

// pruneExperiences trims each instance's experiences to
// [experienceRetentionHeadroom], keeping the most salient and never
// removing one an accepted description or tendency cites.
//
// Ordering by salience_at is ordering by decayed salience: see
// [domain.SalienceAt]. What falls off the end is the least live of the
// experiences nothing has ever built anything on.
func (s *SQLiteStore) pruneExperiences(ctx context.Context) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin experience retention: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	doomed, err := prunableExperiencesTx(ctx, tx)
	if err != nil {
		return 0, err
	}
	if len(doomed) == 0 {
		return 0, nil
	}

	for _, id := range doomed {
		// A revision names every experience active under it, and that link
		// has no cascade of its own, so it goes first.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM persona_revision_experiences WHERE experience_id = ?`, id,
		); err != nil {
			return 0, fmt.Errorf("unlink persona experience %d: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM persona_experiences WHERE id = ?`, id,
		); err != nil {
			return 0, fmt.Errorf("delete persona experience %d: %w", id, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit experience retention: %w", err)
	}

	return int64(len(doomed)), nil
}

// prunableExperiencesTx returns the experiences past the headroom, least
// salient first, excluding every one an accepted description or tendency
// cites.
func prunableExperiencesTx(
	ctx context.Context,
	tx *sql.Tx,
) ([]domain.ExperienceID, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM (
			SELECT id, row_number() OVER (
				PARTITION BY instance_id ORDER BY salience_at DESC, id DESC
			) AS position
			FROM persona_experiences
			WHERE id NOT IN (SELECT experience_id FROM persona_description_evidence)
			  AND id NOT IN (SELECT experience_id FROM persona_amendment_evidence)
		) WHERE position > ?
	`, experienceRetentionHeadroom)
	if err != nil {
		return nil, fmt.Errorf("read prunable persona experiences: %w", err)
	}
	defer func() { _ = rows.Close() }()

	doomed := []domain.ExperienceID{}
	for rows.Next() {
		var id domain.ExperienceID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read prunable persona experiences: %w", err)
		}
		doomed = append(doomed, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read prunable persona experiences: %w", err)
	}

	return doomed, rows.Close()
}

// refreshRecalledSalienceTx brings forward the salience of every one of
// this instance's experiences that an event the run read back is behind.
//
// Retrieval is what says an old experience still matters: an instance
// drawn to how a particular person treats it keeps those episodes live,
// and one that never looks again lets them sink. The write happens only
// here, inside the run's single commit, so a run that failed or was
// discarded strengthens nothing.
func refreshRecalledSalienceTx(
	ctx context.Context,
	tx *sql.Tx,
	acceptance PersonaReflectionAcceptance,
) error {
	if len(acceptance.RecalledSources) == 0 {
		return nil
	}

	for _, sequence := range acceptance.RecalledSources {
		if _, err := tx.ExecContext(ctx, `
			UPDATE persona_experiences
			SET last_cited_at = ?,
			    salience_at = CASE confidence
			        WHEN 'high' THEN ?
			        WHEN 'medium' THEN ?
			        ELSE ?
			    END
			WHERE instance_id = ?
			  AND id IN (
			      SELECT experience_id FROM persona_experience_sources
			      WHERE sequence = ?
			  )
		`,
			formatTime(acceptance.FinishedAt),
			formatTime(domain.SalienceAt(domain.ConfidenceHigh, acceptance.FinishedAt)),
			formatTime(domain.SalienceAt(domain.ConfidenceMedium, acceptance.FinishedAt)),
			formatTime(domain.SalienceAt(domain.ConfidenceLow, acceptance.FinishedAt)),
			acceptance.InstanceID,
			sequence,
		); err != nil {
			return fmt.Errorf(
				"refresh salience for reflection source %d: %w", sequence, err,
			)
		}
	}

	return nil
}

// personaTransitionRetentionHeadroom bounds the pointer-move history
// retained for each instance. Every reflection, reset, rollback and
// operator edit appends one, and `/persona` reads only the newest
// `recentPersonaTransitionLimit` of them.
const personaTransitionRetentionHeadroom = 500

// pruneTransitions trims each instance's pointer-move history, keeping the
// most recent. Nothing else removes a transition: the table is append-only
// and its rows outlive the revisions they name.
func (s *SQLiteStore) pruneTransitions(ctx context.Context) (int64, error) {
	query := `DELETE FROM persona_transitions WHERE id IN (
		SELECT id FROM (
			SELECT id, row_number() OVER (
				PARTITION BY instance_id ORDER BY id DESC
			) AS position
			FROM persona_transitions
		) WHERE position > ? LIMIT ?
	)`

	return deleteEventsBatched(
		ctx, s.db, query, []any{personaTransitionRetentionHeadroom},
	)
}

// ExperiencesByID reads the named experiences that belong to one
// instance, in the order given, skipping any the instance does not own
// or the store no longer holds.
//
// The semantic index over experiences is derived state: it answers with
// ids, and this is what turns them back into experiences. Skipping
// rather than failing is what keeps an index that has fallen behind the
// store from failing a reflection: an id retention has removed simply
// drops out of the answer.
func (s *SQLiteStore) ExperiencesByID(
	ctx context.Context,
	instanceID domain.InstanceID,
	ids []domain.ExperienceID,
) ([]domain.Experience, error) {
	if len(ids) == 0 {
		return []domain.Experience{}, nil
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin experience read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	experiences := make([]domain.Experience, 0, len(ids))
	for _, id := range ids {
		found, err := personaExperiencesTx(ctx, tx, []domain.ExperienceID{id})
		if err != nil {
			continue
		}
		for _, experience := range found {
			if experience.InstanceID == instanceID {
				experiences = append(experiences, experience)
			}
		}
	}

	return experiences, nil
}
