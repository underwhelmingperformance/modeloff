package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/laney/modeloff/internal/domain"
)

// ErrReflectionRunConflict reports a repeated run ID with different metadata.
var ErrReflectionRunConflict = errors.New("reflection run conflicts with recorded outcome")

// ErrNoReflectionRun reports that a reflection run does not exist.
var ErrNoReflectionRun = errors.New("no reflection run")

// RecordReflectionRun stores a bounded diagnostic outcome without changing
// the persona revision or checkpoint.
//
// Repeating the same run is idempotent, and repeating a run id with
// different metadata is refused with [ErrReflectionRunConflict], both for
// as long as the row survives retention. A repeat arriving after
// `reflectionRunRetentionHeadroom` later attempts records a new run.
func (s *SQLiteStore) RecordReflectionRun(
	ctx context.Context,
	run domain.ReflectionRun,
) error {
	run.StartedAt = run.StartedAt.UTC()
	run.FinishedAt = run.FinishedAt.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reflection run: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, found, err := reflectionRunTx(ctx, tx, run.ID)
	if err != nil {
		return err
	}
	if found {
		if !sameReflectionRun(existing, run) {
			return ErrReflectionRunConflict
		}

		return nil
	}
	if err := requirePersonaRevisionOwnershipTx(
		ctx, tx, run.InstanceID, run.BaseRevisionID,
	); err != nil {
		return err
	}
	if err := requirePersonaRevisionOwnershipTx(
		ctx, tx, run.InstanceID, run.ResultRevisionID,
	); err != nil {
		return err
	}
	if err := insertReflectionRunTx(ctx, tx, run); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reflection run: %w", err)
	}

	return nil
}

// ReflectionRun returns one recorded diagnostic or accepted run.
func (s *SQLiteStore) ReflectionRun(
	ctx context.Context,
	runID domain.ReflectionRunID,
) (domain.ReflectionRun, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.ReflectionRun{}, fmt.Errorf("begin reflection run read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	run, found, err := reflectionRunTx(ctx, tx, runID)
	if err != nil {
		return domain.ReflectionRun{}, err
	}
	if !found {
		return domain.ReflectionRun{}, ErrNoReflectionRun
	}

	return run, nil
}

// ReflectionRuns returns the most recently finished reflection diagnostics for
// one instance. A non-positive limit returns an empty slice.
func (s *SQLiteStore) ReflectionRuns(
	ctx context.Context,
	instanceID domain.InstanceID,
	limit int,
) ([]domain.ReflectionRun, error) {
	return reflectionRunsTx(ctx, s.db, instanceID, limit)
}

// reflectionRunsTx reads an instance's newest runs through whichever of
// the database and a transaction the caller holds, so an inspection can
// take them in the same read as the persona state.
func reflectionRunsTx(
	ctx context.Context,
	queryer rowsQueryer,
	instanceID domain.InstanceID,
	limit int,
) ([]domain.ReflectionRun, error) {
	if limit <= 0 {
		return []domain.ReflectionRun{}, nil
	}

	rows, err := queryer.QueryContext(ctx, `
		SELECT id, instance_id, base_revision_id, prior_checkpoint,
		       high_water_mark, result_revision_id, model_id, outcome,
		       rejection_reason, proposed_experiences, accepted_experiences,
		       proposed_amendments, accepted_amendments, started_at, finished_at
		FROM reflection_runs
		WHERE instance_id = ?
		ORDER BY finished_at DESC, id DESC
		LIMIT ?
	`, instanceID, limit)
	if err != nil {
		return nil, fmt.Errorf("read reflection runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	runs := make([]domain.ReflectionRun, 0, limit)
	for rows.Next() {
		run, err := scanReflectionRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read reflection runs: %w", err)
	}

	return runs, nil
}

func sameReflectionRun(a, b domain.ReflectionRun) bool {
	return a.ID == b.ID &&
		a.InstanceID == b.InstanceID &&
		a.BaseRevisionID == b.BaseRevisionID &&
		a.PriorCheckpoint == b.PriorCheckpoint &&
		a.HighWaterMark == b.HighWaterMark &&
		a.ResultRevisionID == b.ResultRevisionID &&
		a.ModelID == b.ModelID &&
		a.Outcome == b.Outcome &&
		a.RejectionReason == b.RejectionReason &&
		a.ProposedExperiences == b.ProposedExperiences &&
		a.AcceptedExperiences == b.AcceptedExperiences &&
		a.ProposedAmendments == b.ProposedAmendments &&
		a.AcceptedAmendments == b.AcceptedAmendments &&
		a.StartedAt.Equal(b.StartedAt) &&
		a.FinishedAt.Equal(b.FinishedAt)
}

func requirePersonaRevisionOwnershipTx(
	ctx context.Context,
	tx *sql.Tx,
	instanceID domain.InstanceID,
	revisionID domain.PersonaRevisionID,
) error {
	revision, err := personaRevisionTx(ctx, tx, revisionID)
	if err != nil {
		return err
	}
	if revision.InstanceID != instanceID {
		return fmt.Errorf(
			"persona revision %d belongs to another instance", revisionID,
		)
	}

	return nil
}
