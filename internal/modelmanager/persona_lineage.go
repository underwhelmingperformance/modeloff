package modelmanager

import (
	"context"

	"github.com/laney/modeloff/internal/domain"
)

const (
	recentReflectionRunLimit     = 10
	recentPersonaTransitionLimit = 20
)

// InspectPersona returns one instance's active persona lineage and bounded
// reflection diagnostics.
func (m *Manager) InspectPersona(
	ctx context.Context,
	nick domain.Nick,
) (domain.PersonaInspection, error) {
	instance, err := m.personaInstance(ctx, nick)
	if err != nil {
		return domain.PersonaInspection{}, err
	}

	return m.inspectPersonaInstance(ctx, instance)
}

// ResetPersona selects an instance's immutable revision zero.
func (m *Manager) ResetPersona(
	ctx context.Context,
	nick domain.Nick,
) (domain.PersonaInspection, error) {
	instance, err := m.personaInstance(ctx, nick)
	if err != nil {
		return domain.PersonaInspection{}, err
	}
	snapshot, err := m.store.PersonaSnapshot(ctx, instance.ID())
	if err != nil {
		return domain.PersonaInspection{}, err
	}
	if _, err := m.store.ResetPersona(
		ctx, instance.ID(), snapshot.Lineage.CurrentRevisionID, m.now(),
	); err != nil {
		return domain.PersonaInspection{}, err
	}

	return m.inspectPersonaInstance(ctx, instance)
}

// RollbackPersona selects an ancestor of an instance's active revision.
func (m *Manager) RollbackPersona(
	ctx context.Context,
	nick domain.Nick,
	target domain.PersonaRevisionID,
) (domain.PersonaInspection, error) {
	instance, err := m.personaInstance(ctx, nick)
	if err != nil {
		return domain.PersonaInspection{}, err
	}
	snapshot, err := m.store.PersonaSnapshot(ctx, instance.ID())
	if err != nil {
		return domain.PersonaInspection{}, err
	}
	if _, err := m.store.RollbackPersona(
		ctx, instance.ID(), snapshot.Lineage.CurrentRevisionID, target, m.now(),
	); err != nil {
		return domain.PersonaInspection{}, err
	}

	return m.inspectPersonaInstance(ctx, instance)
}

// SetInstancePersona gives an instance a replacement persona description,
// written as an operator-authored revision in the same lineage reflection
// writes into. The instance speaks under it from its next turn, and an
// operator can roll it back like any other revision.
//
// This is a different thing from `/config persona`, which defines a template
// in the pool a new instance's revision zero is copied from. A template and a
// live description are separate: editing one has never changed the other.
func (m *Manager) SetInstancePersona(
	ctx context.Context,
	nick domain.Nick,
	description string,
) (domain.PersonaInspection, error) {
	if reason := domain.ValidatePersona(description); reason != domain.PersonaAccepted {
		return domain.PersonaInspection{}, domain.ErroneousPersonaError{
			Reason: reason, At: m.now(),
		}
	}
	instance, err := m.personaInstance(ctx, nick)
	if err != nil {
		return domain.PersonaInspection{}, err
	}
	snapshot, err := m.store.PersonaSnapshot(ctx, instance.ID())
	if err != nil {
		return domain.PersonaInspection{}, err
	}
	if _, err := m.store.WritePersonaDescription(
		ctx, instance.ID(), snapshot.Lineage.CurrentRevisionID, description, m.now(),
	); err != nil {
		return domain.PersonaInspection{}, err
	}

	return m.inspectPersonaInstance(ctx, instance)
}

// personaInstance resolves a nick to the model instance holding it. Only a
// model instance has persona lineage, so a nick held by the user's connection
// record is refused with [domain.UnknownNickError] like a nick nobody holds.
func (m *Manager) personaInstance(
	ctx context.Context,
	nick domain.Nick,
) (*domain.Instance, error) {
	instances, err := m.store.ListInstances(ctx)
	if err != nil {
		return nil, err
	}
	for _, instance := range instances {
		if instance.IsModel() && domain.EqualNick(instance.Nick(), nick) {
			return instance, nil
		}
	}

	return nil, domain.UnknownNickError{Nick: nick, At: m.now()}
}

func (m *Manager) inspectPersonaInstance(
	ctx context.Context,
	instance *domain.Instance,
) (domain.PersonaInspection, error) {
	// One read, because reflection runs on the manager's own schedule: a
	// commit landing between separate reads would show one revision's
	// experiences beside the next revision's run and the transition
	// between them.
	inspection, err := m.store.PersonaInspection(
		ctx, instance.ID(), recentReflectionRunLimit, recentPersonaTransitionLimit,
	)
	if err != nil {
		return domain.PersonaInspection{}, err
	}
	// Counterpart nicks are display state read from the live instance
	// directory, which the inspection's own coherence does not depend on.
	counterparts, err := m.personaCounterparts(
		ctx, inspection.Persona.Experiences, inspection.Persona.Amendments,
	)
	if err != nil {
		return domain.PersonaInspection{}, err
	}

	return domain.PersonaInspection{
		Nick:         instance.Nick(),
		Lineage:      inspection.Persona.Lineage,
		Revision:     inspection.Persona.Revision,
		Parent:       inspection.Parent,
		Experiences:  inspection.Persona.Experiences,
		Amendments:   inspection.Persona.Amendments,
		Counterparts: counterparts,
		RecentRuns:   inspection.RecentRuns,
		Transitions:  inspection.Transitions,
	}, nil
}

// personaCounterparts resolves the actors a snapshot names, an experience's
// subject and an amendment's counterpart alike, to the nicks they currently
// hold. An actor that has quit has no instance row, so it has no entry here.
func (m *Manager) personaCounterparts(
	ctx context.Context,
	experiences []domain.Experience,
	amendments []domain.PersonaAmendment,
) ([]domain.PersonaCounterpart, error) {
	instances, err := m.store.ListInstances(ctx)
	if err != nil {
		return nil, err
	}
	nicks := make(map[domain.InstanceID]domain.Nick, len(instances))
	for _, instance := range instances {
		nicks[instance.ID()] = instance.Nick()
	}

	counterparts := make([]domain.PersonaCounterpart, 0)
	seen := make(map[domain.InstanceID]bool)
	record := func(id *domain.InstanceID) {
		if id == nil || seen[*id] {
			return
		}
		nick, ok := nicks[*id]
		if !ok {
			return
		}

		seen[*id] = true
		counterparts = append(counterparts, domain.PersonaCounterpart{
			InstanceID: *id,
			Nick:       nick,
		})
	}
	for _, experience := range experiences {
		record(experience.SubjectID)
	}
	for _, amendment := range amendments {
		record(amendment.Counterpart)
	}

	return counterparts, nil
}
