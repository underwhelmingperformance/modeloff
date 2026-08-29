package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/laney/modeloff/internal/domain"
)

// PersonaInspectionSnapshot is everything an operator's `/persona` read
// takes from the store, read as one coherent view.
//
// Parent is the revision the active one was derived from, and is nil for
// revision zero. RecentRuns and Transitions are the newest of each, as
// many as the caller asked for.
type PersonaInspectionSnapshot struct {
	Persona     PersonaSnapshot
	Parent      *domain.PersonaRevision
	RecentRuns  []domain.ReflectionRun
	Transitions []domain.PersonaTransition
}

// PersonaInspection reads one instance's persona snapshot, its parent
// revision, and its recent runs and transitions in a single
// transaction.
//
// Reflection runs on the manager's own schedule, so a reflection
// committing between separate reads is the ordinary case, and an
// inspection assembled from separate reads would show one revision's
// experiences beside the next revision's run and the transition between
// them.
func (s *SQLiteStore) PersonaInspection(
	ctx context.Context,
	instanceID domain.InstanceID,
	runs, transitions int,
) (PersonaInspectionSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PersonaInspectionSnapshot{}, fmt.Errorf("begin persona inspection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	persona, err := personaSnapshotTx(ctx, tx, instanceID)
	if err != nil {
		return PersonaInspectionSnapshot{}, err
	}

	var parent *domain.PersonaRevision
	if persona.Revision.ParentID != nil {
		revision, err := personaRevisionTx(ctx, tx, *persona.Revision.ParentID)
		if err != nil {
			return PersonaInspectionSnapshot{}, err
		}
		parent = &revision
	}

	recentRuns, err := reflectionRunsTx(ctx, tx, instanceID, runs)
	if err != nil {
		return PersonaInspectionSnapshot{}, err
	}
	recentTransitions, err := recentPersonaTransitionsTx(ctx, tx, instanceID, transitions)
	if err != nil {
		return PersonaInspectionSnapshot{}, err
	}

	return PersonaInspectionSnapshot{
		Persona: persona, Parent: parent,
		RecentRuns: recentRuns, Transitions: recentTransitions,
	}, nil
}
