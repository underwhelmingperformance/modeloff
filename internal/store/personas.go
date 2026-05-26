package store

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
)

// ListPersonas implements Store.
func (s *SQLiteStore) ListPersonas(ctx context.Context) ([]domain.Persona, error) {
	var personas []domain.Persona
	err := s.inSpan(ctx, "store.sqlite.list_personas", nil, func(ctx context.Context, _ trace.Span) error {
		got, err := queryRows(ctx, s.db,
			`SELECT id, description, origin FROM personas ORDER BY id`, nil,
			personaRow)
		if err != nil {
			return err
		}

		personas = got
		return nil
	})

	return personas, err
}

// GetPersona implements Store.
func (s *SQLiteStore) GetPersona(ctx context.Context, id string) (domain.Persona, error) {
	var p domain.Persona
	err := s.inSpan(ctx, "store.sqlite.get_persona",
		[]attribute.KeyValue{attribute.String("persona.id", id)},
		func(ctx context.Context, _ trace.Span) error {
			got, err := queryRow(ctx, s.db,
				`SELECT id, description, origin FROM personas WHERE id = ?`,
				[]any{id}, nil, personaRow)
			if err != nil {
				return fmt.Errorf("persona %q: %w", id, err)
			}

			p = got
			return nil
		})

	return p, err
}

// SavePersona implements Store.
func (s *SQLiteStore) SavePersona(ctx context.Context, p domain.Persona) error {
	return s.inSpan(ctx, "store.sqlite.save_persona",
		[]attribute.KeyValue{attribute.String("persona.id", p.ID)},
		func(ctx context.Context, _ trace.Span) error {
			return execMutation(ctx, s.db,
				`INSERT INTO personas (id, description, origin) VALUES (?, ?, ?)
				 ON CONFLICT (id) DO UPDATE SET description = excluded.description, origin = excluded.origin`,
				p.ID, p.Description, p.Origin)
		})
}

// DeletePersonasByOrigin implements Store.
func (s *SQLiteStore) DeletePersonasByOrigin(ctx context.Context, origin domain.PersonaOrigin) error {
	return s.inSpan(ctx, "store.sqlite.delete_personas_by_origin",
		[]attribute.KeyValue{attribute.String("persona.origin", string(origin))},
		func(ctx context.Context, _ trace.Span) error {
			return execMutation(ctx, s.db, `DELETE FROM personas WHERE origin = ?`, origin)
		})
}

// personaRow decodes the (id, description, origin) shape used by
// every personas-table query.
func personaRow(r rowScanner) (domain.Persona, error) {
	var p domain.Persona
	return p, r.Scan(&p.ID, &p.Description, &p.Origin)
}

// ReplaceGeneratedPersonas implements Store. It atomically deletes all
// generated personas and inserts the given replacements in a single
// transaction.
func (s *SQLiteStore) ReplaceGeneratedPersonas(ctx context.Context, personas []domain.Persona) error {
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
