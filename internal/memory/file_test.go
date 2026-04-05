package memory

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

func TestFileStore_ReadEmpty(t *testing.T) {
	store := NewFileStore(t.TempDir())

	got, err := store.Read(t.Context(), "alice")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestFileStore_WriteAndRead(t *testing.T) {
	ctx := t.Context()
	store := NewFileStore(t.TempDir())
	nick := domain.Nick("bob")

	entries := []Entry{
		{Key: "greeting", Content: "Hello, I like cats."},
		{Key: "preference", Content: "Prefers formal tone."},
	}

	for _, e := range entries {
		require.NoError(t, store.Write(ctx, nick, e))
	}

	got, err := store.Read(ctx, nick)
	require.NoError(t, err)
	require.Equal(t, entries, got)
}

func TestFileStore_WriteOverwritesExistingKey(t *testing.T) {
	ctx := t.Context()
	store := NewFileStore(t.TempDir())
	nick := domain.Nick("charlie")

	require.NoError(t, store.Write(ctx, nick, Entry{Key: "mood", Content: "happy"}))
	require.NoError(t, store.Write(ctx, nick, Entry{Key: "mood", Content: "excited"}))

	got, err := store.Read(ctx, nick)
	require.NoError(t, err)
	require.Equal(t, []Entry{{Key: "mood", Content: "excited"}}, got)
}

func TestFileStore_Delete(t *testing.T) {
	ctx := t.Context()
	store := NewFileStore(t.TempDir())
	nick := domain.Nick("dave")

	entries := []Entry{
		{Key: "first", Content: "one"},
		{Key: "second", Content: "two"},
		{Key: "third", Content: "three"},
	}

	for _, e := range entries {
		require.NoError(t, store.Write(ctx, nick, e))
	}

	require.NoError(t, store.Delete(ctx, nick, "second"))

	got, err := store.Read(ctx, nick)
	require.NoError(t, err)

	want := []Entry{
		{Key: "first", Content: "one"},
		{Key: "third", Content: "three"},
	}

	require.Equal(t, want, got)
}

func TestFileStore_DeleteNonexistent(t *testing.T) {
	ctx := t.Context()
	store := NewFileStore(t.TempDir())

	require.NoError(t, store.Delete(ctx, "eve", "nonexistent"))
}

func TestFileStore_IsolationBetweenNicks(t *testing.T) {
	ctx := t.Context()
	store := NewFileStore(t.TempDir())

	require.NoError(t, store.Write(ctx, "nick-a", Entry{Key: "k", Content: "from-a"}))
	require.NoError(t, store.Write(ctx, "nick-b", Entry{Key: "k", Content: "from-b"}))

	gotA, err := store.Read(ctx, "nick-a")
	require.NoError(t, err)
	require.Equal(t, []Entry{{Key: "k", Content: "from-a"}}, gotA)

	gotB, err := store.Read(ctx, "nick-b")
	require.NoError(t, err)
	require.Equal(t, []Entry{{Key: "k", Content: "from-b"}}, gotB)
}

func TestFileStore_Reset(t *testing.T) {
	ctx := t.Context()
	store := NewFileStore(t.TempDir())

	require.NoError(t, store.Write(ctx, "alice", Entry{Key: "k1", Content: "v1"}))
	require.NoError(t, store.Write(ctx, "bob", Entry{Key: "k2", Content: "v2"}))

	require.NoError(t, store.Reset(ctx))

	gotA, err := store.Read(ctx, "alice")
	require.NoError(t, err)
	require.Empty(t, gotA)

	gotB, err := store.Read(ctx, "bob")
	require.NoError(t, err)
	require.Empty(t, gotB)
}

func TestFileStore_Reset_empty(t *testing.T) {
	store := NewFileStore(t.TempDir())

	require.NoError(t, store.Reset(t.Context()))
}
