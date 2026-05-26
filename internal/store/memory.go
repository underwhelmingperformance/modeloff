package store

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
)

// ReadMemories implements Store.
func (s *SQLiteStore) ReadMemories(ctx context.Context, id domain.InstanceID) ([]MemoryEntry, error) {
	var entries []MemoryEntry
	err := s.inSpan(ctx, "store.sqlite.read_memories",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			got, err := queryRows(ctx, s.db,
				`SELECT key, content FROM memories WHERE instance_id = ? ORDER BY key`,
				[]any{string(id)},
				func(r rowScanner) (MemoryEntry, error) {
					var e MemoryEntry
					return e, r.Scan(&e.Key, &e.Content)
				})
			if err != nil {
				return err
			}

			entries = got
			return nil
		})

	return entries, err
}

// WriteMemory implements Store.
func (s *SQLiteStore) WriteMemory(ctx context.Context, id domain.InstanceID, key, content string) error {
	return s.inSpan(ctx, "store.sqlite.write_memory",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			return execMutation(ctx, s.db,
				`INSERT INTO memories (instance_id, key, content) VALUES (?, ?, ?)
				 ON CONFLICT (instance_id, key) DO UPDATE SET content = excluded.content`,
				string(id), key, content)
		})
}

// DeleteMemory implements Store.
func (s *SQLiteStore) DeleteMemory(ctx context.Context, id domain.InstanceID, key string) error {
	return s.inSpan(ctx, "store.sqlite.delete_memory",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			return execMutation(ctx, s.db, `DELETE FROM memories WHERE instance_id = ? AND key = ?`, string(id), key)
		})
}

// ResetMemories implements Store.
func (s *SQLiteStore) ResetMemories(ctx context.Context) error {
	return s.inSpan(ctx, "store.sqlite.reset_memories", nil, func(ctx context.Context, _ trace.Span) error {
		return execMutation(ctx, s.db, `DELETE FROM memories`)
	})
}
