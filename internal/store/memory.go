package store

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
)

// parseMemoryAt reads a memories.at column value back into a
// time.Time. A row written before the column existed carries the
// empty string, which fails RFC3339Nano parsing; that failure (and
// any other unparsable value) is not an error worth reporting back
// to the caller, since ordering by the zero time already gives the
// legacy-row behaviour ReadMemories wants.
func parseMemoryAt(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}

	return t
}

// ReadMemories implements Store.
func (s *SQLiteStore) ReadMemories(ctx context.Context, id domain.InstanceID) ([]MemoryEntry, error) {
	var entries []MemoryEntry
	err := s.inSpan(ctx, "store.sqlite.read_memories",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			got, err := queryRows(ctx, s.db,
				`SELECT key, content, at FROM memories WHERE instance_id = ? ORDER BY key`,
				[]any{string(id)},
				func(r rowScanner) (MemoryEntry, error) {
					var e MemoryEntry
					var at string
					if err := r.Scan(&e.Key, &e.Content, &at); err != nil {
						return e, err
					}

					e.At = parseMemoryAt(at)
					return e, nil
				})
			if err != nil {
				return err
			}

			entries = got
			return nil
		})

	return entries, err
}

// WriteMemory implements Store. at is the entry's write time, the
// caller's clock reading at the moment it decided to remember this
// fact; an overwrite of an existing key updates it too, so a memory
// touched again is fresh again.
func (s *SQLiteStore) WriteMemory(ctx context.Context, id domain.InstanceID, key, content string, at time.Time) error {
	return s.inSpan(ctx, "store.sqlite.write_memory",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			return execMutation(ctx, s.db,
				`INSERT INTO memories (instance_id, key, content, at) VALUES (?, ?, ?, ?)
				 ON CONFLICT (instance_id, key) DO UPDATE SET content = excluded.content, at = excluded.at`,
				string(id), key, content, at.Format(time.RFC3339Nano))
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
