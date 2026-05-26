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

// RandomPersona picks a random persona from the store pool.
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

		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(personas))))
		if err != nil {
			return fmt.Errorf("random selection: %w", err)
		}

		chosen = personas[n.Int64()]
		return nil
	})

	return chosen, err
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
