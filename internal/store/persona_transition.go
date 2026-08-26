package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/laney/modeloff/internal/domain"
)

// ResetPersona points an instance back to revision zero. It preserves the
// reflection checkpoint, so old event ranges do not become pending again.
func (s *SQLiteStore) ResetPersona(
	ctx context.Context,
	instanceID domain.InstanceID,
	expectedRevision domain.PersonaRevisionID,
	at time.Time,
) (domain.PersonaLineage, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.PersonaLineage{}, fmt.Errorf("begin persona reset: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var revisionZero domain.PersonaRevisionID
	if err := tx.QueryRowContext(ctx, `
		SELECT id FROM persona_revisions
		WHERE instance_id = ? AND parent_id IS NULL
		ORDER BY id LIMIT 1
	`, instanceID).Scan(&revisionZero); err != nil {
		return domain.PersonaLineage{}, fmt.Errorf("read persona revision zero: %w", err)
	}
	state, err := movePersonaRevisionTx(
		ctx, tx, instanceID, expectedRevision, revisionZero,
		domain.PersonaTransitionReset, at,
	)
	if err != nil {
		return domain.PersonaLineage{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.PersonaLineage{}, fmt.Errorf("commit persona reset: %w", err)
	}

	return state, nil
}

// RollbackPersona selects an ancestor of the current revision. It
// preserves the reflection checkpoint and rejects revisions from another
// instance or another branch.
func (s *SQLiteStore) RollbackPersona(
	ctx context.Context,
	instanceID domain.InstanceID,
	expectedRevision domain.PersonaRevisionID,
	targetRevision domain.PersonaRevisionID,
	at time.Time,
) (domain.PersonaLineage, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.PersonaLineage{}, fmt.Errorf("begin persona rollback: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ancestor, err := personaRevisionIsAncestorTx(
		ctx, tx, instanceID, expectedRevision, targetRevision,
	)
	if err != nil {
		return domain.PersonaLineage{}, err
	}
	if !ancestor {
		return domain.PersonaLineage{}, fmt.Errorf(
			"rollback persona: revision %d is not an ancestor of %d",
			targetRevision,
			expectedRevision,
		)
	}
	state, err := movePersonaRevisionTx(
		ctx, tx, instanceID, expectedRevision, targetRevision,
		domain.PersonaTransitionRollback, at,
	)
	if err != nil {
		return domain.PersonaLineage{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.PersonaLineage{}, fmt.Errorf("commit persona rollback: %w", err)
	}

	return state, nil
}

// WritePersonaDescription records an operator's replacement persona
// description as a revision in the instance's own lineage, so there is one
// history and one description in force. The new revision carries the active
// revision's experiences and tendencies forward unchanged, and cites no
// evidence: an operator writes the description directly, and citations belong
// to the text they were gathered for.
//
// The description already in force writes no revision, so repeating an edit
// does not lengthen the lineage. The revision the caller names has to be the
// active one, so an edit racing a reflection that changes the description
// loses to whichever landed first. A reflection that changes nothing else
// still advances the checkpoint, which this edit does not read, so the two
// can both land: the edit writes its revision and the checkpoint moves.
func (s *SQLiteStore) WritePersonaDescription(
	ctx context.Context,
	instanceID domain.InstanceID,
	expectedRevision domain.PersonaRevisionID,
	description string,
	at time.Time,
) (domain.PersonaLineage, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.PersonaLineage{}, fmt.Errorf("begin persona description: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	state, err := personaLineageTx(ctx, tx, instanceID)
	if err != nil {
		return domain.PersonaLineage{}, err
	}
	if state.CurrentRevisionID != expectedRevision {
		return domain.PersonaLineage{}, ErrPersonaLineageChanged
	}
	parent, err := personaRevisionTx(ctx, tx, expectedRevision)
	if err != nil {
		return domain.PersonaLineage{}, err
	}
	if parent.Description == description {
		return state, nil
	}
	revisionID, err := insertDescribedRevisionTx(ctx, tx, parent, description, at)
	if err != nil {
		return domain.PersonaLineage{}, err
	}
	state, err = movePersonaRevisionTx(
		ctx, tx, instanceID, expectedRevision, revisionID,
		domain.PersonaTransitionOperator, at,
	)
	if err != nil {
		return domain.PersonaLineage{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.PersonaLineage{}, fmt.Errorf("commit persona description: %w", err)
	}

	return state, nil
}

func insertDescribedRevisionTx(
	ctx context.Context,
	tx *sql.Tx,
	parent domain.PersonaRevision,
	description string,
	at time.Time,
) (domain.PersonaRevisionID, error) {
	result, err := tx.ExecContext(ctx, `
		INSERT INTO persona_revisions
			(instance_id, parent_id, description, created_at)
		VALUES (?, ?, ?, ?)
	`, parent.InstanceID, parent.ID, description, at.Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("insert persona revision: %w", err)
	}
	revisionID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read persona revision id: %w", err)
	}
	if err := insertRevisionLinksTx(ctx, tx, "revision experience", `
		INSERT INTO persona_revision_experiences (revision_id, experience_id)
		VALUES (?, ?)
	`, revisionID, parent.ExperienceIDs); err != nil {
		return 0, err
	}
	if err := insertRevisionLinksTx(ctx, tx, "revision amendment", `
		INSERT INTO persona_revision_amendments (revision_id, amendment_id)
		VALUES (?, ?)
	`, revisionID, parent.AmendmentIDs); err != nil {
		return 0, err
	}

	return domain.PersonaRevisionID(revisionID), nil
}

func movePersonaRevisionTx(
	ctx context.Context,
	tx *sql.Tx,
	instanceID domain.InstanceID,
	expectedRevision domain.PersonaRevisionID,
	targetRevision domain.PersonaRevisionID,
	kind domain.PersonaTransitionKind,
	at time.Time,
) (domain.PersonaLineage, error) {
	result, err := tx.ExecContext(ctx, `
		UPDATE persona_lineages SET current_revision_id = ?
		WHERE instance_id = ? AND current_revision_id = ?
	`, targetRevision, instanceID, expectedRevision)
	if err != nil {
		return domain.PersonaLineage{}, fmt.Errorf("move persona revision: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return domain.PersonaLineage{}, fmt.Errorf("read persona revision update: %w", err)
	}
	if updated != 1 {
		return domain.PersonaLineage{}, ErrPersonaLineageChanged
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO persona_transitions
			(instance_id, from_revision_id, to_revision_id, kind, at)
		VALUES (?, ?, ?, ?, ?)
	`, instanceID, expectedRevision, targetRevision, kind,
		at.Format(time.RFC3339Nano)); err != nil {
		return domain.PersonaLineage{}, fmt.Errorf("insert persona transition: %w", err)
	}

	return personaLineageTx(ctx, tx, instanceID)
}

func personaRevisionIsAncestorTx(
	ctx context.Context,
	tx *sql.Tx,
	instanceID domain.InstanceID,
	revisionID domain.PersonaRevisionID,
	ancestorID domain.PersonaRevisionID,
) (bool, error) {
	var found bool
	err := tx.QueryRowContext(ctx, `
		WITH RECURSIVE ancestors(id, instance_id, parent_id) AS (
			SELECT id, instance_id, parent_id FROM persona_revisions WHERE id = ?
			UNION ALL
			SELECT revision.id, revision.instance_id, revision.parent_id
			FROM persona_revisions AS revision
			JOIN ancestors ON revision.id = ancestors.parent_id
		)
		SELECT EXISTS(
			SELECT 1 FROM ancestors WHERE id = ? AND instance_id = ?
		)
	`, revisionID, ancestorID, instanceID).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("check persona revision ancestry: %w", err)
	}

	return found, nil
}

// PersonaTransitions returns the append-only revision pointer history for one
// instance.
func (s *SQLiteStore) PersonaTransitions(
	ctx context.Context,
	instanceID domain.InstanceID,
) ([]domain.PersonaTransition, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, instance_id, from_revision_id, to_revision_id, kind, at
		FROM persona_transitions WHERE instance_id = ? ORDER BY id
	`, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read persona transitions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	transitions := []domain.PersonaTransition{}
	for rows.Next() {
		transition, err := scanPersonaTransition(rows)
		if err != nil {
			return nil, err
		}
		transitions = append(transitions, transition)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read persona transitions: %w", err)
	}

	return transitions, nil
}

type personaTransitionScanner interface {
	Scan(dest ...any) error
}

func scanPersonaTransition(
	scanner personaTransitionScanner,
) (domain.PersonaTransition, error) {
	var transition domain.PersonaTransition
	var at string
	if err := scanner.Scan(
		&transition.ID,
		&transition.InstanceID,
		&transition.FromRevisionID,
		&transition.ToRevisionID,
		&transition.Kind,
		&at,
	); err != nil {
		return domain.PersonaTransition{}, fmt.Errorf("scan persona transition: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return domain.PersonaTransition{}, fmt.Errorf("parse persona transition time: %w", err)
	}
	transition.At = parsed

	return transition, nil
}

// RecentPersonaTransitions returns the bounded newest suffix of an instance's
// revision-pointer history in chronological order.
func (s *SQLiteStore) RecentPersonaTransitions(
	ctx context.Context,
	instanceID domain.InstanceID,
	limit int,
) ([]domain.PersonaTransition, error) {
	return recentPersonaTransitionsTx(ctx, s.db, instanceID, limit)
}

// recentPersonaTransitionsTx reads through whichever of the database and
// a transaction the caller holds, so an inspection can take the
// transitions in the same read as the persona state.
func recentPersonaTransitionsTx(
	ctx context.Context,
	queryer rowsQueryer,
	instanceID domain.InstanceID,
	limit int,
) ([]domain.PersonaTransition, error) {
	if limit <= 0 {
		return []domain.PersonaTransition{}, nil
	}

	rows, err := queryer.QueryContext(ctx, `
		SELECT id, instance_id, from_revision_id, to_revision_id, kind, at
		FROM persona_transitions
		WHERE instance_id = ?
		ORDER BY id DESC
		LIMIT ?
	`, instanceID, limit)
	if err != nil {
		return nil, fmt.Errorf("read recent persona transitions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	transitions := make([]domain.PersonaTransition, 0, limit)
	for rows.Next() {
		transition, err := scanPersonaTransition(rows)
		if err != nil {
			return nil, err
		}
		transitions = append(transitions, transition)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read recent persona transitions: %w", err)
	}
	slices.Reverse(transitions)

	return transitions, nil
}
