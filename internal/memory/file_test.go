package memory

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/observability/oteltest"
	"github.com/laney/modeloff/internal/store/storetest"
)

func TestStoreAdapter_ReadEmpty(t *testing.T) {
	store := NewStoreAdapter(storetest.NewMemoryStore(t))

	got, err := store.Read(t.Context(), "alice-id")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestStoreAdapter_WriteAndRead(t *testing.T) {
	ctx := t.Context()
	store := NewStoreAdapter(storetest.NewMemoryStore(t))
	id := domain.InstanceID("inst-bob")

	entries := []Entry{
		{Key: "greeting", Content: "Hello, I like cats."},
		{Key: "preference", Content: "Prefers formal tone."},
	}

	for _, e := range entries {
		require.NoError(t, store.Write(ctx, id, e))
	}

	got, err := store.Read(ctx, id)
	require.NoError(t, err)
	require.Equal(t, entries, got)
}

func TestStoreAdapter_Write_recordsSpan(t *testing.T) {
	recorder, provider := oteltest.NewSpanRecorder(t)
	store := NewStoreAdapter(storetest.NewMemoryStore(t)).WithTracerProvider(provider)

	require.NoError(t, store.Write(t.Context(), "inst-bob", Entry{Key: "greeting", Content: "hello"}))

	span := oteltest.FindSpan(t, recorder, "memory.file.write")
	require.Equal(t, "memory.file.write", oteltest.AttrValue(span.Attributes(), observability.AttrOperation))
	require.Equal(t, "inst-bob", oteltest.AttrValue(span.Attributes(), observability.AttrInstanceID))
	require.Equal(t, observability.ResultOK, oteltest.AttrValue(span.Attributes(), observability.AttrResult))
}

func TestStoreAdapter_WriteOverwritesExistingKey(t *testing.T) {
	ctx := t.Context()
	store := NewStoreAdapter(storetest.NewMemoryStore(t))
	id := domain.InstanceID("inst-charlie")

	require.NoError(t, store.Write(ctx, id, Entry{Key: "mood", Content: "happy"}))
	require.NoError(t, store.Write(ctx, id, Entry{Key: "mood", Content: "excited"}))

	got, err := store.Read(ctx, id)
	require.NoError(t, err)
	require.Equal(t, []Entry{{Key: "mood", Content: "excited"}}, got)
}

func TestStoreAdapter_Delete(t *testing.T) {
	ctx := t.Context()
	store := NewStoreAdapter(storetest.NewMemoryStore(t))
	id := domain.InstanceID("inst-dave")

	entries := []Entry{
		{Key: "first", Content: "one"},
		{Key: "second", Content: "two"},
		{Key: "third", Content: "three"},
	}

	for _, e := range entries {
		require.NoError(t, store.Write(ctx, id, e))
	}

	require.NoError(t, store.Delete(ctx, id, "second"))

	got, err := store.Read(ctx, id)
	require.NoError(t, err)

	want := []Entry{
		{Key: "first", Content: "one"},
		{Key: "third", Content: "three"},
	}

	require.Equal(t, want, got)
}

func TestStoreAdapter_DeleteNonexistent(t *testing.T) {
	ctx := t.Context()
	store := NewStoreAdapter(storetest.NewMemoryStore(t))

	require.NoError(t, store.Delete(ctx, "inst-eve", "nonexistent"))
}

func TestStoreAdapter_IsolationBetweenInstances(t *testing.T) {
	ctx := t.Context()
	store := NewStoreAdapter(storetest.NewMemoryStore(t))

	require.NoError(t, store.Write(ctx, "inst-a", Entry{Key: "k", Content: "from-a"}))
	require.NoError(t, store.Write(ctx, "inst-b", Entry{Key: "k", Content: "from-b"}))

	gotA, err := store.Read(ctx, "inst-a")
	require.NoError(t, err)
	require.Equal(t, []Entry{{Key: "k", Content: "from-a"}}, gotA)

	gotB, err := store.Read(ctx, "inst-b")
	require.NoError(t, err)
	require.Equal(t, []Entry{{Key: "k", Content: "from-b"}}, gotB)
}

func TestStoreAdapter_Reset(t *testing.T) {
	ctx := t.Context()
	store := NewStoreAdapter(storetest.NewMemoryStore(t))

	require.NoError(t, store.Write(ctx, "inst-alice", Entry{Key: "k1", Content: "v1"}))
	require.NoError(t, store.Write(ctx, "inst-bob", Entry{Key: "k2", Content: "v2"}))

	require.NoError(t, store.Reset(ctx))

	gotA, err := store.Read(ctx, "inst-alice")
	require.NoError(t, err)
	require.Empty(t, gotA)

	gotB, err := store.Read(ctx, "inst-bob")
	require.NoError(t, err)
	require.Empty(t, gotB)
}

func TestStoreAdapter_Reset_empty(t *testing.T) {
	store := NewStoreAdapter(storetest.NewMemoryStore(t))

	require.NoError(t, store.Reset(t.Context()))
}
