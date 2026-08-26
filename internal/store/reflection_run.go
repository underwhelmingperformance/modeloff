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

// RecordReflectionRun stores a bounded diagnostic outcome without changing the
// persona revision or checkpoint. Repeating the same run is idempotent.
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
