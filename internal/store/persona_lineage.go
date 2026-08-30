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
	createdAt time.Time,
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

	createdAtText := formatTime(createdAt)
	result, err := tx.ExecContext(ctx, `
		INSERT INTO persona_revisions
			(instance_id, parent_id, description, created_at)
		VALUES (?, NULL, ?, ?)
	`, instanceID, baseline, createdAtText)
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
	`, instanceID, baseline, revisionID, createdAtText); err != nil {
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

// PersonaCounts returns the active persona revision of one model
// instance and how many experiences and tendencies it rests on.
//
// An instance with no persona lineage, which includes the user's
// connection record, returns the zero value and no error: `/whois`
// answers for every connected client, and having no persona lineage is
// a normal answer rather than a failure.
func (s *SQLiteStore) PersonaCounts(
	ctx context.Context,
	instanceID domain.InstanceID,
) (domain.PersonaCounts, error) {
	var counts domain.PersonaCounts
	err := s.db.QueryRowContext(ctx, `
		SELECT
			s.current_revision_id,
			(SELECT COUNT(*) FROM persona_revision_experiences
			 WHERE revision_id = s.current_revision_id),
			(SELECT COUNT(*) FROM persona_revision_amendments
			 WHERE revision_id = s.current_revision_id)
		FROM persona_lineages s
		WHERE s.instance_id = ?
	`, instanceID).Scan(&counts.Revision, &counts.Experiences, &counts.Tendencies)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.PersonaCounts{}, nil
	}
	if err != nil {
		return domain.PersonaCounts{}, fmt.Errorf("read persona counts: %w", err)
	}

	return counts, nil
}

// PersonaRevision returns one persona revision. Its description and
// parent are fixed; which experiences it names follows the retention
// backstop, which unlinks the ones it removes.
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
