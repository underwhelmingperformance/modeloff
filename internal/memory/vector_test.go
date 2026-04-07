package memory

import (
	"context"
	"fmt"
	"math"
	"testing"

	chromem "github.com/philippgille/chromem-go"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/store/storetest"
)

// fakeEmbedder produces deterministic embeddings by assigning each
// topic keyword a unit vector in a different dimension. This makes
// cosine similarity between different topics exactly 0, and between
// the same topic exactly 1.
func fakeEmbedder(dims int, topics map[string]int) chromem.EmbeddingFunc {
	return func(_ context.Context, text string) ([]float32, error) {
		vec := make([]float32, dims)

		for keyword, dim := range topics {
			if containsWord(text, keyword) {
				vec[dim] = 1.0
				return vec, nil
			}
		}

		// Default: spread across all dimensions equally.
		val := float32(1.0 / math.Sqrt(float64(dims)))
		for i := range vec {
			vec[i] = val
		}

		return vec, nil
	}
}

func containsWord(text, word string) bool {
	for i := 0; i <= len(text)-len(word); i++ {
		if text[i:i+len(word)] == word {
			return true
		}
	}

	return false
}

func newTestIndexedStore(t *testing.T, embedder chromem.EmbeddingFunc) *IndexedStore {
	t.Helper()

	backing := NewStoreAdapter(storetest.NewMemoryStore(t))
	db := chromem.NewDB()

	return NewIndexedStoreFromDB(backing, db, embedder)
}

// trivialEmbedder returns a constant vector — sufficient for
// non-search CRUD tests where similarity ranking is irrelevant.
func trivialEmbedder() chromem.EmbeddingFunc {
	return func(_ context.Context, _ string) ([]float32, error) {
		return []float32{1.0, 0.0, 0.0}, nil
	}
}

func failingEmbedder() chromem.EmbeddingFunc {
	return func(_ context.Context, _ string) ([]float32, error) {
		return nil, fmt.Errorf("embedding service unavailable")
	}
}

// --- Layer 1: Store interface compliance ---

func TestIndexedStore_ReadEmpty(t *testing.T) {
	store := newTestIndexedStore(t, trivialEmbedder())

	got, err := store.Read(t.Context(), "alice")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestIndexedStore_WriteAndRead(t *testing.T) {
	ctx := t.Context()
	store := newTestIndexedStore(t, trivialEmbedder())
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

func TestIndexedStore_WriteOverwritesExistingKey(t *testing.T) {
	ctx := t.Context()
	store := newTestIndexedStore(t, trivialEmbedder())
	nick := domain.Nick("charlie")

	require.NoError(t, store.Write(ctx, nick, Entry{Key: "mood", Content: "happy"}))
	require.NoError(t, store.Write(ctx, nick, Entry{Key: "mood", Content: "excited"}))

	got, err := store.Read(ctx, nick)
	require.NoError(t, err)
	require.Equal(t, []Entry{{Key: "mood", Content: "excited"}}, got)
}

func TestIndexedStore_Delete(t *testing.T) {
	ctx := t.Context()
	store := newTestIndexedStore(t, trivialEmbedder())
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
	require.Equal(t, []Entry{
		{Key: "first", Content: "one"},
		{Key: "third", Content: "three"},
	}, got)
}

func TestIndexedStore_DeleteNonexistent(t *testing.T) {
	store := newTestIndexedStore(t, trivialEmbedder())

	require.NoError(t, store.Delete(t.Context(), "eve", "nonexistent"))
}

func TestIndexedStore_IsolationBetweenNicks(t *testing.T) {
	ctx := t.Context()
	store := newTestIndexedStore(t, trivialEmbedder())

	require.NoError(t, store.Write(ctx, "nick-a", Entry{Key: "k", Content: "from-a"}))
	require.NoError(t, store.Write(ctx, "nick-b", Entry{Key: "k", Content: "from-b"}))

	gotA, err := store.Read(ctx, "nick-a")
	require.NoError(t, err)
	require.Equal(t, []Entry{{Key: "k", Content: "from-a"}}, gotA)

	gotB, err := store.Read(ctx, "nick-b")
	require.NoError(t, err)
	require.Equal(t, []Entry{{Key: "k", Content: "from-b"}}, gotB)
}

func TestIndexedStore_Reset(t *testing.T) {
	ctx := t.Context()
	topics := map[string]int{"cats": 0, "dogs": 1}
	store := newTestIndexedStore(t, fakeEmbedder(2, topics))

	require.NoError(t, store.Write(ctx, "alice", Entry{Key: "k1", Content: "cats are great"}))
	require.NoError(t, store.Write(ctx, "bob", Entry{Key: "k2", Content: "dogs are loyal"}))

	require.NoError(t, store.Reset(ctx))

	gotA, err := store.Read(ctx, "alice")
	require.NoError(t, err)
	require.Empty(t, gotA)

	gotB, err := store.Read(ctx, "bob")
	require.NoError(t, err)
	require.Empty(t, gotB)

	searchA, err := store.Search(ctx, "alice", "cats", 5)
	require.NoError(t, err)
	require.Empty(t, searchA)

	searchB, err := store.Search(ctx, "bob", "dogs", 5)
	require.NoError(t, err)
	require.Empty(t, searchB)
}

func TestIndexedStore_Reset_empty(t *testing.T) {
	store := newTestIndexedStore(t, trivialEmbedder())

	require.NoError(t, store.Reset(t.Context()))
}

// --- Semantic search tests ---

func TestIndexedStore_Search_returns_relevant_entries(t *testing.T) {
	ctx := t.Context()
	topics := map[string]int{"cats": 0, "dogs": 1, "fish": 2}
	store := newTestIndexedStore(t, fakeEmbedder(3, topics))
	nick := domain.Nick("searcher")

	require.NoError(t, store.Write(ctx, nick, Entry{Key: "cat_fact", Content: "cats sleep 16 hours a day"}))
	require.NoError(t, store.Write(ctx, nick, Entry{Key: "dog_fact", Content: "dogs are loyal companions"}))
	require.NoError(t, store.Write(ctx, nick, Entry{Key: "fish_fact", Content: "fish breathe through gills"}))

	results, err := store.Search(ctx, nick, "cats are great", 3)
	require.NoError(t, err)

	// The top result is deterministic (exact topic match). The
	// remaining results tie at similarity 0, so use ElementsMatch.
	require.Equal(t, SearchResult{
		Entry:      Entry{Key: "cat_fact", Content: "cats sleep 16 hours a day"},
		Similarity: 1.0,
	}, results[0])
	require.ElementsMatch(t, []SearchResult{
		{Entry: Entry{Key: "dog_fact", Content: "dogs are loyal companions"}, Similarity: 0},
		{Entry: Entry{Key: "fish_fact", Content: "fish breathe through gills"}, Similarity: 0},
	}, results[1:])
}

func TestIndexedStore_Search_respects_limit(t *testing.T) {
	ctx := t.Context()
	store := newTestIndexedStore(t, fakeEmbedder(3, map[string]int{"a": 0, "b": 1, "c": 2}))
	nick := domain.Nick("limited")

	require.NoError(t, store.Write(ctx, nick, Entry{Key: "one", Content: "a first entry"}))
	require.NoError(t, store.Write(ctx, nick, Entry{Key: "two", Content: "b second entry"}))
	require.NoError(t, store.Write(ctx, nick, Entry{Key: "three", Content: "c third entry"}))

	results, err := store.Search(ctx, nick, "a query", 1)
	require.NoError(t, err)
	require.Equal(t, []SearchResult{
		{Entry: Entry{Key: "one", Content: "a first entry"}, Similarity: 1.0},
	}, results)
}

func TestIndexedStore_Search_zero_limit_returns_all(t *testing.T) {
	ctx := t.Context()
	store := newTestIndexedStore(t, trivialEmbedder())
	nick := domain.Nick("zerolimit")

	require.NoError(t, store.Write(ctx, nick, Entry{Key: "one", Content: "first"}))
	require.NoError(t, store.Write(ctx, nick, Entry{Key: "two", Content: "second"}))
	require.NoError(t, store.Write(ctx, nick, Entry{Key: "three", Content: "third"}))

	results, err := store.Search(ctx, nick, "query", 0)
	require.NoError(t, err)
	require.ElementsMatch(t, []SearchResult{
		{Entry: Entry{Key: "one", Content: "first"}, Similarity: 1.0},
		{Entry: Entry{Key: "two", Content: "second"}, Similarity: 1.0},
		{Entry: Entry{Key: "three", Content: "third"}, Similarity: 1.0},
	}, results)
}

func TestIndexedStore_Search_negative_limit_returns_all(t *testing.T) {
	ctx := t.Context()
	store := newTestIndexedStore(t, trivialEmbedder())
	nick := domain.Nick("neglimit")

	require.NoError(t, store.Write(ctx, nick, Entry{Key: "one", Content: "first"}))
	require.NoError(t, store.Write(ctx, nick, Entry{Key: "two", Content: "second"}))

	results, err := store.Search(ctx, nick, "query", -1)
	require.NoError(t, err)
	require.ElementsMatch(t, []SearchResult{
		{Entry: Entry{Key: "one", Content: "first"}, Similarity: 1.0},
		{Entry: Entry{Key: "two", Content: "second"}, Similarity: 1.0},
	}, results)
}

func TestIndexedStore_Search_empty_collection(t *testing.T) {
	store := newTestIndexedStore(t, trivialEmbedder())

	results, err := store.Search(t.Context(), "nobody", "anything", 5)
	require.NoError(t, err)
	require.Empty(t, results)
}

func TestIndexedStore_Search_isolation_between_nicks(t *testing.T) {
	ctx := t.Context()
	topics := map[string]int{"cats": 0, "dogs": 1}
	store := newTestIndexedStore(t, fakeEmbedder(2, topics))

	require.NoError(t, store.Write(ctx, "alice", Entry{Key: "pet", Content: "cats are the best"}))
	require.NoError(t, store.Write(ctx, "bob", Entry{Key: "pet", Content: "dogs are the best"}))

	results, err := store.Search(ctx, "alice", "cats", 5)
	require.NoError(t, err)
	require.Equal(t, []SearchResult{
		{Entry: Entry{Key: "pet", Content: "cats are the best"}, Similarity: 1.0},
	}, results)
}

func TestIndexedStore_Search_after_delete(t *testing.T) {
	ctx := t.Context()
	topics := map[string]int{"cats": 0, "dogs": 1}
	store := newTestIndexedStore(t, fakeEmbedder(2, topics))
	nick := domain.Nick("deleter")

	require.NoError(t, store.Write(ctx, nick, Entry{Key: "cat_fact", Content: "cats sleep a lot"}))
	require.NoError(t, store.Write(ctx, nick, Entry{Key: "dog_fact", Content: "dogs are loyal"}))

	require.NoError(t, store.Delete(ctx, nick, "cat_fact"))

	results, err := store.Search(ctx, nick, "cats", 5)
	require.NoError(t, err)
	require.Equal(t, []SearchResult{
		{Entry: Entry{Key: "dog_fact", Content: "dogs are loyal"}, Similarity: 0},
	}, results)
}

func TestIndexedStore_Search_after_overwrite(t *testing.T) {
	ctx := t.Context()
	topics := map[string]int{"cats": 0, "dogs": 1}
	store := newTestIndexedStore(t, fakeEmbedder(2, topics))
	nick := domain.Nick("overwriter")

	require.NoError(t, store.Write(ctx, nick, Entry{Key: "fact", Content: "cats are great"}))
	require.NoError(t, store.Write(ctx, nick, Entry{Key: "fact", Content: "dogs are great"}))

	results, err := store.Search(ctx, nick, "dogs", 5)
	require.NoError(t, err)
	require.Equal(t, []SearchResult{
		{Entry: Entry{Key: "fact", Content: "dogs are great"}, Similarity: 1.0},
	}, results)
}

// --- Layer 2: Embedding error handling ---

func TestIndexedStore_Write_indexing_failure_still_persists(t *testing.T) {
	ctx := t.Context()
	store := newTestIndexedStore(t, failingEmbedder())
	nick := domain.Nick("embedfail")

	require.NoError(t, store.Write(ctx, nick, Entry{Key: "k", Content: "v"}))

	got, err := store.Read(ctx, nick)
	require.NoError(t, err)
	require.Equal(t, []Entry{{Key: "k", Content: "v"}}, got)
}

func TestIndexedStore_Search_embedding_failure_on_query(t *testing.T) {
	ctx := t.Context()
	nick := domain.Nick("searchfail")
	db := chromem.NewDB()

	// Seed the collection with a document using pre-computed embeddings
	// so the collection is non-empty.
	col, err := db.GetOrCreateCollection(string(nick), nil, failingEmbedder())
	require.NoError(t, err)
	require.NoError(t, col.Add(ctx,
		[]string{"k"},
		[][]float32{{1.0, 0.0, 0.0}},
		[]map[string]string{{"key": "k", "content": "v"}},
		[]string{"k: v"},
	))

	backing := NewStoreAdapter(storetest.NewMemoryStore(t))
	store := NewIndexedStoreFromDB(backing, db, failingEmbedder())

	_, err = store.Search(ctx, nick, "query", 5)
	require.Error(t, err)
}

// --- Layer 4: Persistence ---

func TestIndexedStore_persistence(t *testing.T) {
	ctx := t.Context()
	indexDir := t.TempDir()
	nick := domain.Nick("persist")

	embedder := fakeEmbedder(3, map[string]int{"cats": 0, "dogs": 1, "fish": 2})

	// Use a single SQLite store to simulate persistence across reopens.
	sqlStore := storetest.NewMemoryStore(t)

	backing1 := NewStoreAdapter(sqlStore)
	store1, err := NewIndexedStore(backing1, indexDir, embedder)
	require.NoError(t, err)

	require.NoError(t, store1.Write(ctx, nick, Entry{Key: "cat", Content: "cats are great"}))
	require.NoError(t, store1.Write(ctx, nick, Entry{Key: "dog", Content: "dogs are loyal"}))

	// Reopen: new adapter and index on the same backing store and directory.
	backing2 := NewStoreAdapter(sqlStore)
	store2, err := NewIndexedStore(backing2, indexDir, embedder)
	require.NoError(t, err)

	got, err := store2.Read(ctx, nick)
	require.NoError(t, err)
	require.Equal(t, []Entry{
		{Key: "cat", Content: "cats are great"},
		{Key: "dog", Content: "dogs are loyal"},
	}, got)

	results, err := store2.Search(ctx, nick, "cats", 1)
	require.NoError(t, err)
	require.Equal(t, []SearchResult{
		{Entry: Entry{Key: "cat", Content: "cats are great"}, Similarity: 1.0},
	}, results)
}

// --- Lazy reindex ---

func TestIndexedStore_Search_reindexes_from_backing_store(t *testing.T) {
	ctx := t.Context()
	embedder := fakeEmbedder(2, map[string]int{"cats": 0, "dogs": 1})
	nick := domain.Nick("reindex")

	// Write entries directly to the backing store (simulating migration
	// from a plain store with no vector index).
	backing := NewStoreAdapter(storetest.NewMemoryStore(t))
	require.NoError(t, backing.Write(ctx, nick, Entry{Key: "cat", Content: "cats are great"}))
	require.NoError(t, backing.Write(ctx, nick, Entry{Key: "dog", Content: "dogs are loyal"}))

	store := NewIndexedStoreFromDB(backing, chromem.NewDB(), embedder)

	// Search should trigger a lazy reindex and return results.
	results, err := store.Search(ctx, nick, "cats", 1)
	require.NoError(t, err)
	require.Equal(t, []SearchResult{
		{Entry: Entry{Key: "cat", Content: "cats are great"}, Similarity: 1.0},
	}, results)
}
