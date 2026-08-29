package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/set"
)

// PersonaInspectionSnapshot is everything an operator's `/persona` read
// takes from the store, read as one coherent view.
//
// Parent is the revision the active one was derived from, and is nil for
// revision zero. Departed contains the amendments present in Parent and
// absent from the active revision, each carrying its departure when one
// was recorded. RecentRuns and Transitions are the newest of each, as
// many as the caller asked for.
type PersonaInspectionSnapshot struct {
	Persona     PersonaSnapshot
	Parent      *domain.PersonaRevision
	Departed    []domain.PersonaAmendment
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
	departed := []domain.PersonaAmendment{}
	if persona.Revision.ParentID != nil {
		revision, err := personaRevisionTx(ctx, tx, *persona.Revision.ParentID)
		if err != nil {
			return PersonaInspectionSnapshot{}, err
		}
		parent = &revision
		departed, err = personaAmendmentsTx(
			ctx, tx, departedAmendmentIDs(revision, persona.Revision),
		)
		if err != nil {
			return PersonaInspectionSnapshot{}, err
		}
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
		Persona: persona, Parent: parent, Departed: departed,
		RecentRuns: recentRuns, Transitions: recentTransitions,
	}, nil
}

// departedAmendmentIDs returns the amendment ids present in parent and
// absent from current.
func departedAmendmentIDs(
	parent, current domain.PersonaRevision,
) []domain.PersonaAmendmentID {
	active := set.New(current.AmendmentIDs...)
	departed := make([]domain.PersonaAmendmentID, 0, len(parent.AmendmentIDs))
	for _, id := range parent.AmendmentIDs {
		if !active.Has(id) {
			departed = append(departed, id)
		}
	}

	return departed
}
