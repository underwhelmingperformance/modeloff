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

// EnsurePersonaTemplates populates the persona pool if it is empty. It
// calls the API to generate templates and saves each to the store.
func (m *Manager) EnsurePersonaTemplates(ctx context.Context) error {
	return m.inSpan(ctx, "modelmanager.ensure_persona_templates", nil, func(ctx context.Context, _ trace.Span) error {
		existing, err := m.store.ListPersonaTemplates(ctx)
		if err != nil {
			return fmt.Errorf("list persona templates: %w", err)
		}

		if len(existing) > 0 {
			return nil
		}

		client, _ := m.snapshotAPI()
		if client == nil {
			return fmt.Errorf("generate persona templates: api client not configured")
		}

		templates, err := client.GeneratePersonaTemplates(ctx, m.SmallModel())
		if err != nil {
			return fmt.Errorf("generate persona templates: %w", err)
		}

		for _, p := range templates {
			if err := m.store.SavePersonaTemplate(ctx, p); err != nil {
				return fmt.Errorf("save persona template %q: %w", p.ID, err)
			}
		}

		return nil
	})
}

// RandomPersonaTemplate picks a random template from the store pool,
// excluding any description a connected model instance already holds, so
// a run of invites does not hand out the same character twice while an
// unused template is available. Once every template in the pool is held,
// the draw falls back to the full pool and hands out a duplicate.
func (m *Manager) RandomPersonaTemplate(ctx context.Context) (domain.PersonaTemplate, error) {
	var chosen domain.PersonaTemplate

	err := m.inSpan(ctx, "modelmanager.random_persona_template", nil, func(ctx context.Context, _ trace.Span) error {
		templates, err := m.store.ListPersonaTemplates(ctx)
		if err != nil {
			return fmt.Errorf("list persona templates: %w", err)
		}

		if len(templates) == 0 {
			return fmt.Errorf("no persona templates available")
		}

		held, err := m.heldPersonaDescriptions(ctx)
		if err != nil {
			return fmt.Errorf("list instances: %w", err)
		}

		pool := templates
		if unheld := excludeHeld(templates, held); len(unheld) > 0 {
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

// excludeHeld returns the templates whose description is not in held.
func excludeHeld(templates []domain.PersonaTemplate, held map[string]bool) []domain.PersonaTemplate {
	unheld := make([]domain.PersonaTemplate, 0, len(templates))
	for _, p := range templates {
		if !held[p.Description] {
			unheld = append(unheld, p)
		}
	}

	return unheld
}

// RegeneratePersonaTemplates generates persona templates through the
// API, then replaces the generated templates in the store. The API call
// happens first so that the existing pool is preserved if generation
// fails. An operator's own templates are never touched.
func (m *Manager) RegeneratePersonaTemplates(ctx context.Context) ([]domain.PersonaTemplate, error) {
	var templates []domain.PersonaTemplate

	err := m.inSpan(ctx, "modelmanager.regenerate_persona_templates", nil, func(ctx context.Context, _ trace.Span) error {
		client, _ := m.snapshotAPI()
		if client == nil {
			return fmt.Errorf("generate persona templates: api client not configured")
		}

		generated, err := client.GeneratePersonaTemplates(ctx, m.SmallModel())
		if err != nil {
			return fmt.Errorf("generate persona templates: %w", err)
		}

		if err := m.store.ReplaceGeneratedPersonaTemplates(ctx, generated); err != nil {
			return fmt.Errorf("replace generated persona templates: %w", err)
		}

		templates = generated
		return nil
	})

	return templates, err
}

// SetPersonaTemplate saves a user-defined persona template to the store.
func (m *Manager) SetPersonaTemplate(ctx context.Context, id string, description string) error {
	if reason := domain.ValidatePersona(description); reason != domain.PersonaAccepted {
		return domain.ErroneousPersonaError{Reason: reason, At: m.now()}
	}

	return m.inSpan(ctx, "modelmanager.set_persona_template", []attribute.KeyValue{
		attribute.String("persona_template.id", id),
	}, func(ctx context.Context, _ trace.Span) error {
		p := domain.PersonaTemplate{
			ID:          id,
			Description: description,
			Origin:      domain.PersonaUser,
		}

		return m.store.SavePersonaTemplate(ctx, p)
	})
}

// ListPersonaTemplates returns every template in the store's pool.
func (m *Manager) ListPersonaTemplates(ctx context.Context) ([]domain.PersonaTemplate, error) {
	var templates []domain.PersonaTemplate

	err := m.inSpan(ctx, "modelmanager.list_persona_templates", nil, func(ctx context.Context, _ trace.Span) error {
		listed, err := m.store.ListPersonaTemplates(ctx)
		if err != nil {
			return err
		}
		templates = listed
		return nil
	})

	return templates, err
}

// ResetPersonaTemplates removes every operator-written template from
// the store, leaving only generated ones. It returns the number
// that were removed.
func (m *Manager) ResetPersonaTemplates(ctx context.Context) (int, error) {
	var count int

	err := m.inSpan(ctx, "modelmanager.reset_persona_templates", nil, func(ctx context.Context, _ trace.Span) error {
		templates, err := m.store.ListPersonaTemplates(ctx)
		if err != nil {
			return fmt.Errorf("list persona templates: %w", err)
		}

		for _, p := range templates {
			if p.Origin == domain.PersonaUser {
				count++
			}
		}

		if err := m.store.DeletePersonaTemplatesByOrigin(ctx, domain.PersonaUser); err != nil {
			return err
		}

		return nil
	})

	return count, err
}
