package store

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
)

// ListPersonaTemplates implements Store.
func (s *SQLiteStore) ListPersonaTemplates(ctx context.Context) ([]domain.PersonaTemplate, error) {
	var personas []domain.PersonaTemplate
	err := s.inSpan(ctx, "store.sqlite.list_personas", nil, func(ctx context.Context, _ trace.Span) error {
		got, err := queryRows(ctx, s.db,
			`SELECT id, description, origin FROM personas ORDER BY id`, nil,
			personaTemplateRow)
		if err != nil {
			return err
		}

		personas = got
		return nil
	})

	return personas, err
}

// GetPersonaTemplate implements Store.
func (s *SQLiteStore) GetPersonaTemplate(ctx context.Context, id string) (domain.PersonaTemplate, error) {
	var p domain.PersonaTemplate
	err := s.inSpan(ctx, "store.sqlite.get_persona",
		[]attribute.KeyValue{attribute.String("persona.id", id)},
		func(ctx context.Context, _ trace.Span) error {
			got, err := queryRow(ctx, s.db,
				`SELECT id, description, origin FROM personas WHERE id = ?`,
				[]any{id}, nil, personaTemplateRow)
			if err != nil {
				return fmt.Errorf("persona %q: %w", id, err)
			}

			p = got
			return nil
		})

	return p, err
}

// SavePersonaTemplate implements Store.
func (s *SQLiteStore) SavePersonaTemplate(ctx context.Context, p domain.PersonaTemplate) error {
	return s.inSpan(ctx, "store.sqlite.save_persona",
		[]attribute.KeyValue{attribute.String("persona.id", p.ID)},
		func(ctx context.Context, _ trace.Span) error {
			return execMutation(ctx, s.db,
				`INSERT INTO personas (id, description, origin) VALUES (?, ?, ?)
				 ON CONFLICT (id) DO UPDATE SET description = excluded.description, origin = excluded.origin`,
				p.ID, p.Description, p.Origin)
		})
}

// DeletePersonaTemplatesByOrigin implements Store.
func (s *SQLiteStore) DeletePersonaTemplatesByOrigin(ctx context.Context, origin domain.PersonaOrigin) error {
	return s.inSpan(ctx, "store.sqlite.delete_personas_by_origin",
		[]attribute.KeyValue{attribute.String("persona.origin", string(origin))},
		func(ctx context.Context, _ trace.Span) error {
			return execMutation(ctx, s.db, `DELETE FROM personas WHERE origin = ?`, origin)
		})
}

// personaTemplateRow decodes the (id, description, origin) shape used by
// every personas-table query.
func personaTemplateRow(r rowScanner) (domain.PersonaTemplate, error) {
	var p domain.PersonaTemplate
	return p, r.Scan(&p.ID, &p.Description, &p.Origin)
}

// ReplaceGeneratedPersonaTemplates implements Store. It atomically deletes all
// generated personas and inserts the given replacements in a single
// transaction.
func (s *SQLiteStore) ReplaceGeneratedPersonaTemplates(ctx context.Context, personas []domain.PersonaTemplate) error {
	return s.inSpan(ctx, "store.sqlite.replace_generated_personas", nil, func(ctx context.Context, _ trace.Span) error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin tx: %w", err)
		}

		defer func() { _ = tx.Rollback() }()

		if _, err := tx.ExecContext(ctx, `DELETE FROM personas WHERE origin = ?`, domain.PersonaGenerated); err != nil {
			return fmt.Errorf("delete generated: %w", err)
		}

		for _, p := range personas {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO personas (id, description, origin) VALUES (?, ?, ?)
				 ON CONFLICT (id) DO UPDATE SET description = excluded.description, origin = excluded.origin`,
				p.ID, p.Description, p.Origin); err != nil {
				return fmt.Errorf("insert persona %q: %w", p.ID, err)
			}
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit: %w", err)
		}

		return nil
	})
}
