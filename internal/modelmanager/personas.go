package modelmanager

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
)

// EnsurePersonas populates the persona pool if it is empty. It
// calls the API to generate personas and saves each to the store.
func (m *Manager) EnsurePersonas(ctx context.Context) error {
	return m.inSpan(ctx, "modelmanager.ensure_personas", nil, func(ctx context.Context, _ trace.Span) error {
		existing, err := m.store.ListPersonas(ctx)
		if err != nil {
			return fmt.Errorf("list personas: %w", err)
		}

		if len(existing) > 0 {
			return nil
		}

		client, _ := m.snapshotAPI()
		if client == nil {
			return fmt.Errorf("generate personas: api client not configured")
		}

		personas, err := client.GeneratePersonas(ctx, m.SmallModel())
		if err != nil {
			return fmt.Errorf("generate personas: %w", err)
		}

		for _, p := range personas {
			if err := m.store.SavePersona(ctx, p); err != nil {
				return fmt.Errorf("save persona %q: %w", p.ID, err)
			}
		}

		return nil
	})
}

// RandomPersona picks a random persona from the store pool, excluding
// any persona description already held by a connected model instance
// so a run of invites does not hand out the same persona twice while
// an unused one is available. Once every persona in the pool is held,
// the draw falls back to the full pool and hands out a duplicate.
func (m *Manager) RandomPersona(ctx context.Context) (domain.Persona, error) {
	var chosen domain.Persona

	err := m.inSpan(ctx, "modelmanager.random_persona", nil, func(ctx context.Context, _ trace.Span) error {
		personas, err := m.store.ListPersonas(ctx)
		if err != nil {
			return fmt.Errorf("list personas: %w", err)
		}

		if len(personas) == 0 {
			return fmt.Errorf("no personas available")
		}

		held, err := m.heldPersonaDescriptions(ctx)
		if err != nil {
			return fmt.Errorf("list instances: %w", err)
		}

		pool := personas
		if unheld := excludeHeld(personas, held); len(unheld) > 0 {
			pool = unheld
		}

		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(pool))))
		if err != nil {
			return fmt.Errorf("random selection: %w", err)
		}

		chosen = pool[n.Int64()]
		return nil
	})

	return chosen, err
}

// heldPersonaDescriptions returns the persona descriptions assigned
// to every instance row [Store.ListInstances] currently reports.
// QUIT and KILL delete an instance's row on a clean disconnect, so
// this is normally just the connected instances. The session's
// cleanup after a failed ADDMODEL is best-effort, though: if it
// cannot delete the row it leaves behind, that row's persona stays
// excluded from the draw until something removes it.
func (m *Manager) heldPersonaDescriptions(ctx context.Context) (map[string]bool, error) {
	instances, err := m.store.ListInstances(ctx)
	if err != nil {
		return nil, err
	}

	held := make(map[string]bool, len(instances))
	for _, inst := range instances {
		if p := inst.Persona(); p != "" {
			held[p] = true
		}
	}

	return held, nil
}

// excludeHeld returns the personas whose description is not in held.
func excludeHeld(personas []domain.Persona, held map[string]bool) []domain.Persona {
	unheld := make([]domain.Persona, 0, len(personas))
	for _, p := range personas {
		if !held[p.Description] {
			unheld = append(unheld, p)
		}
	}

	return unheld
}

// RegeneratePersonas generates a fresh set of personas via the
// API, then replaces all generated personas in the store. The API
// call happens first so that the existing pool is preserved if
// generation fails. User-defined personas are never touched.
func (m *Manager) RegeneratePersonas(ctx context.Context) ([]domain.Persona, error) {
	var personas []domain.Persona

	err := m.inSpan(ctx, "modelmanager.regenerate_personas", nil, func(ctx context.Context, _ trace.Span) error {
		client, _ := m.snapshotAPI()
		if client == nil {
			return fmt.Errorf("generate personas: api client not configured")
		}

		generated, err := client.GeneratePersonas(ctx, m.SmallModel())
		if err != nil {
			return fmt.Errorf("generate personas: %w", err)
		}

		if err := m.store.ReplaceGeneratedPersonas(ctx, generated); err != nil {
			return fmt.Errorf("replace generated personas: %w", err)
		}

		personas = generated
		return nil
	})

	return personas, err
}

// SetPersona saves a user-defined persona to the store.
func (m *Manager) SetPersona(ctx context.Context, id string, description string) error {
	return m.inSpan(ctx, "modelmanager.set_persona", []attribute.KeyValue{
		attribute.String("persona.id", id),
	}, func(ctx context.Context, _ trace.Span) error {
		p := domain.Persona{
			ID:          id,
			Description: description,
			Origin:      domain.PersonaUser,
		}

		return m.store.SavePersona(ctx, p)
	})
}

// ListPersonas returns all personas from the store.
func (m *Manager) ListPersonas(ctx context.Context) ([]domain.Persona, error) {
	var personas []domain.Persona

	err := m.inSpan(ctx, "modelmanager.list_personas", nil, func(ctx context.Context, _ trace.Span) error {
		listed, err := m.store.ListPersonas(ctx)
		if err != nil {
			return err
		}
		personas = listed
		return nil
	})

	return personas, err
}

// ResetPersonas removes all user-defined personas from the store,
// leaving only generated ones. It returns the number of personas
// that were removed.
func (m *Manager) ResetPersonas(ctx context.Context) (int, error) {
	var count int

	err := m.inSpan(ctx, "modelmanager.reset_personas", nil, func(ctx context.Context, _ trace.Span) error {
		personas, err := m.store.ListPersonas(ctx)
		if err != nil {
			return fmt.Errorf("list personas: %w", err)
		}

		for _, p := range personas {
			if p.Origin == domain.PersonaUser {
				count++
			}
		}

		if err := m.store.DeletePersonasByOrigin(ctx, domain.PersonaUser); err != nil {
			return err
		}

		return nil
	})

	return count, err
}
