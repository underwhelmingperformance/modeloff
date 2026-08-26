package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/laney/modeloff/internal/domain"
)

// ErrNoPersonaLineage reports that an instance has no persona-lineage record.
var ErrNoPersonaLineage = errors.New("no persona lineage")

// ErrNoPersonaRevision reports a revision id no revision answers to. An
// instance with a lineage can still name one, through a stale id an
// earlier read handed the caller, so this is not the same condition as
// [ErrNoPersonaLineage] and the scheduler must not read it as one.
var ErrNoPersonaRevision = errors.New("no persona revision")

func ensurePersonaLineageTx(
	ctx context.Context,
	tx *sql.Tx,
	instanceID domain.InstanceID,
	baseline string,
) error {
	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM persona_lineages WHERE instance_id = ?)`,
		instanceID,
	).Scan(&exists); err != nil {
		return fmt.Errorf("check persona lineage: %w", err)
	}
	if exists {
		return nil
	}

	createdAt := time.Time{}.Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `
		INSERT INTO persona_revisions
			(instance_id, parent_id, description, created_at)
		VALUES (?, NULL, ?, ?)
	`, instanceID, baseline, createdAt)
	if err != nil {
		return fmt.Errorf("create persona revision zero: %w", err)
	}
	revisionID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("read persona revision zero id: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO persona_lineages
			(instance_id, baseline, current_revision_id, checkpoint, created_at)
		VALUES (?, ?, ?, 0, ?)
	`, instanceID, baseline, revisionID, createdAt); err != nil {
		return fmt.Errorf("create persona lineage: %w", err)
	}

	return nil
}

// PersonaLineage returns the immutable baseline and active revision for one
// model instance.
func (s *SQLiteStore) PersonaLineage(
	ctx context.Context,
	instanceID domain.InstanceID,
) (domain.PersonaLineage, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.PersonaLineage{}, fmt.Errorf("begin persona lineage read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	return personaLineageTx(ctx, tx, instanceID)
}

// PersonaRevision returns one immutable persona revision.
func (s *SQLiteStore) PersonaRevision(
	ctx context.Context,
	revisionID domain.PersonaRevisionID,
) (domain.PersonaRevision, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.PersonaRevision{}, fmt.Errorf("begin persona revision read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	return personaRevisionTx(ctx, tx, revisionID)
}
