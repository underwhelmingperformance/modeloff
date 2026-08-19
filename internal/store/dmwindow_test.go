package store

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

func TestSQLiteStore_ListDMWindows_empty(t *testing.T) {
	got, err := newTestStore(t).ListDMWindows(t.Context())
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSQLiteStore_AddDMWindow_and_List(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SaveInstance(ctx, domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)))
	require.NoError(t, s.SaveInstance(ctx, domain.NewModelInstance("inst-helper", "helper", "test/model", "", nil)))

	require.NoError(t, s.AddDMWindow(ctx, "inst-botty"))
	require.NoError(t, s.AddDMWindow(ctx, "inst-helper"))

	got, err := s.ListDMWindows(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.InstanceID{"inst-botty", "inst-helper"}, got)
}

func TestSQLiteStore_AddDMWindow_idempotent(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SaveInstance(ctx, domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)))

	require.NoError(t, s.AddDMWindow(ctx, "inst-botty"))
	require.NoError(t, s.AddDMWindow(ctx, "inst-botty"))

	got, err := s.ListDMWindows(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.InstanceID{"inst-botty"}, got)
}

func TestSQLiteStore_AddDMWindow_requires_existing_instance(t *testing.T) {
	err := newTestStore(t).AddDMWindow(t.Context(), "inst-ghost")
	require.Error(t, err)
}

func TestSQLiteStore_RemoveDMWindow(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SaveInstance(ctx, domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)))
	require.NoError(t, s.AddDMWindow(ctx, "inst-botty"))

	require.NoError(t, s.RemoveDMWindow(ctx, "inst-botty"))

	got, err := s.ListDMWindows(ctx)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSQLiteStore_RemoveDMWindow_idempotent(t *testing.T) {
	require.NoError(t, newTestStore(t).RemoveDMWindow(t.Context(), "inst-never-opened"))
}

// TestSQLiteStore_DeleteInstanceByID_cascades_dm_window pins that
// deleting a model instance drops its open DM window too, via the
// `dm_windows.instance_id` foreign key's `ON DELETE CASCADE` — a
// window left pointing at a deleted instance would fail to resolve
// its counterpart on the next load.
func TestSQLiteStore_DeleteInstanceByID_cascades_dm_window(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SaveInstance(ctx, domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)))
	require.NoError(t, s.AddDMWindow(ctx, "inst-botty"))

	require.NoError(t, s.DeleteInstanceByID(ctx, "inst-botty"))

	got, err := s.ListDMWindows(ctx)
	require.NoError(t, err)
	require.Empty(t, got)
}
