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
	var state domain.PersonaLineage
	var createdAt string
	var reflectedAt sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT instance_id, baseline, current_revision_id, checkpoint,
		       created_at, reflected_at
		FROM persona_lineages WHERE instance_id = ?
	`, instanceID).Scan(
		&state.InstanceID,
		&state.Baseline,
		&state.CurrentRevisionID,
		&state.Checkpoint,
		&createdAt,
		&reflectedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.PersonaLineage{}, ErrNoPersonaLineage
	}
	if err != nil {
		return domain.PersonaLineage{}, fmt.Errorf("read persona lineage: %w", err)
	}
	state.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return domain.PersonaLineage{}, fmt.Errorf("parse persona lineage creation time: %w", err)
	}
	if reflectedAt.Valid {
		at, err := time.Parse(time.RFC3339Nano, reflectedAt.String)
		if err != nil {
			return domain.PersonaLineage{}, fmt.Errorf("parse persona reflection time: %w", err)
		}
		state.ReflectedAt = &at
	}

	return state, nil
}

// PersonaRevision returns one immutable persona revision.
func (s *SQLiteStore) PersonaRevision(
	ctx context.Context,
	revisionID domain.PersonaRevisionID,
) (domain.PersonaRevision, error) {
	var revision domain.PersonaRevision
	var parentID sql.NullInt64
	var createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, instance_id, parent_id, description, created_at
		FROM persona_revisions WHERE id = ?
	`, revisionID).Scan(
		&revision.ID,
		&revision.InstanceID,
		&parentID,
		&revision.Description,
		&createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.PersonaRevision{}, ErrNoPersonaLineage
	}
	if err != nil {
		return domain.PersonaRevision{}, fmt.Errorf("read persona revision: %w", err)
	}
	if parentID.Valid {
		parent := domain.PersonaRevisionID(parentID.Int64)
		revision.ParentID = &parent
	}
	revision.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return domain.PersonaRevision{}, fmt.Errorf("parse persona revision creation time: %w", err)
	}
	revision.ExperienceIDs = []domain.ExperienceID{}
	revision.AmendmentIDs = []domain.PersonaAmendmentID{}

	return revision, nil
}
