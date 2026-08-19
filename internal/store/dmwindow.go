package store

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
)

// ListDMWindows implements Store. Returns the counterpart
// InstanceIDs of every DM window the user currently has open, in no
// particular order. This is client-owned data: the user-client is
// the sole reader and writer of the DM window set; the session
// dispatcher never reads or writes it.
func (s *SQLiteStore) ListDMWindows(ctx context.Context) ([]domain.InstanceID, error) {
	var ids []domain.InstanceID
	err := s.inSpan(ctx, "store.sqlite.list_dm_windows", nil, func(ctx context.Context, _ trace.Span) error {
		got, err := queryRows(ctx, s.db,
			`SELECT instance_id FROM dm_windows ORDER BY instance_id`, nil,
			scalarColumn[domain.InstanceID]())
		if err != nil {
			return err
		}

		ids = got
		return nil
	})

	return ids, err
}

// AddDMWindow records that the user has a DM window open with the
// given model instance. Idempotent: adding an already-open window is
// a no-op. This is client-owned data (see ListDMWindows).
func (s *SQLiteStore) AddDMWindow(ctx context.Context, id domain.InstanceID) error {
	return s.inSpan(ctx, "store.sqlite.add_dm_window",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			return execMutation(ctx, s.db,
				`INSERT OR IGNORE INTO dm_windows (instance_id) VALUES (?)`, string(id))
		})
}

// RemoveDMWindow removes the given instance from the user's open DM
// window set. Idempotent: removing a window that isn't open is a
// no-op. This is client-owned data (see ListDMWindows).
func (s *SQLiteStore) RemoveDMWindow(ctx context.Context, id domain.InstanceID) error {
	return s.inSpan(ctx, "store.sqlite.remove_dm_window",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			return execMutation(ctx, s.db, `DELETE FROM dm_windows WHERE instance_id = ?`, string(id))
		})
}
