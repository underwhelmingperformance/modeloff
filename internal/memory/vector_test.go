package memory

import (
	"context"
	"fmt"
	"math"
	"sync/atomic"
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

	return NewIndexedStoreFromDB(t.Context(), backing, db, embedder)
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

// --- Construction-time embedding probe ---

func TestProbeEmbeddingFunc(t *testing.T) {
	tests := []struct {
		name    string
		fn      chromem.EmbeddingFunc
		want    bool
		wantErr bool
	}{
		{name: "nil function (no API key) is unreachable", fn: nil, want: false, wantErr: false},
		{name: "failing function is unreachable", fn: failingEmbedder(), want: false, wantErr: true},
		{name: "working function is reachable", fn: trivialEmbedder(), want: true, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, err := probeEmbeddingFunc(t.Context(), tt.fn)
			require.Equal(t, tt.want, ok)
			require.Equal(t, tt.wantErr, err != nil)
		})
	}
}

func TestIndexedStore_Searchable(t *testing.T) {
	tests := []struct {
		name     string
		embedder chromem.EmbeddingFunc
		want     bool
	}{
		{name: "working embedder is searchable", embedder: trivialEmbedder(), want: true},
		{name: "failing embedder is not searchable", embedder: failingEmbedder(), want: false},
		{name: "nil embedder is not searchable", embedder: nil, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestIndexedStore(t, tt.embedder)
			require.Equal(t, tt.want, store.Searchable())
		})
	}
}

// TestIndexedStore_RefreshSearchable pins the /config recovery path:
// a store constructed while the embedding endpoint is unreachable
// (a fresh install with no API key yet, or a real network failure)
// becomes searchable again once the underlying embedder starts
// working, without reattaching the store — and the reverse, so a
// working endpoint later going bad is also reflected. It also pins
// that ProbeError and RefreshSearchable's own return value track the
// same transitions: nil once the embedder works, the probe's error
// once it fails again.
func TestIndexedStore_RefreshSearchable(t *testing.T) {
	ctx := t.Context()

	var working atomic.Bool
	probeErr := fmt.Errorf("embedding service unavailable")
	embedder := func(context.Context, string) ([]float32, error) {
		if working.Load() {
			return []float32{1.0, 0.0, 0.0}, nil
		}

		return nil, probeErr
	}

	store := newTestIndexedStore(t, embedder)
	require.False(t, store.Searchable())
	require.Equal(t, probeErr, store.ProbeError())

	working.Store(true)
	require.NoError(t, store.RefreshSearchable(ctx))
	require.True(t, store.Searchable())
	require.NoError(t, store.ProbeError())

	working.Store(false)
	require.Equal(t, probeErr, store.RefreshSearchable(ctx))
	require.False(t, store.Searchable())
	require.Equal(t, probeErr, store.ProbeError())
}

// --- DeleteInstance ---

func TestIndexedStore_DeleteInstance_removes_collection(t *testing.T) {
	ctx := t.Context()
	topics := map[string]int{"cats": 0, "dogs": 1}
	store := newTestIndexedStore(t, fakeEmbedder(2, topics))
	id := domain.InstanceID("deleteme")

	require.NoError(t, store.Write(ctx, id, Entry{Key: "fact", Content: "cats are great"}))

	// Confirm the collection is actually populated before deleting it.
	col, err := store.collection(id)
	require.NoError(t, err)
	require.Equal(t, 1, col.Count())

	require.NoError(t, store.DeleteInstance(ctx, id))

	col, err = store.collection(id)
	require.NoError(t, err)
	require.Zero(t, col.Count())
}

func TestIndexedStore_DeleteInstance_leaves_backing_store_and_other_instances_untouched(t *testing.T) {
	ctx := t.Context()
	topics := map[string]int{"cats": 0, "dogs": 1}
	store := newTestIndexedStore(t, fakeEmbedder(2, topics))

	require.NoError(t, store.Write(ctx, "gone", Entry{Key: "fact", Content: "cats are great"}))
	require.NoError(t, store.Write(ctx, "kept", Entry{Key: "fact", Content: "dogs are loyal"}))

	require.NoError(t, store.DeleteInstance(ctx, "gone"))

	// DeleteInstance only clears chromem's collection; the backing
	// store's row is a separate concern the store's own instance-
	// deletion path (store.SQLiteStore.DeleteInstanceByID) already
	// handles.
	entries, err := store.Read(ctx, "gone")
	require.NoError(t, err)
	require.Equal(t, []Entry{{Key: "fact", Content: "cats are great"}}, entries)

	results, err := store.Search(ctx, "kept", "dogs", 5)
	require.NoError(t, err)
	require.Equal(t, []SearchResult{
		{Entry: Entry{Key: "fact", Content: "dogs are loyal"}, Similarity: 1.0},
	}, results)
}

func TestIndexedStore_DeleteInstance_noop_for_instance_never_indexed(t *testing.T) {
	require.NoError(t, newTestIndexedStore(t, trivialEmbedder()).DeleteInstance(t.Context(), "never-seen"))
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
	id := domain.InstanceID("bob")

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

func TestIndexedStore_WriteOverwritesExistingKey(t *testing.T) {
	ctx := t.Context()
	store := newTestIndexedStore(t, trivialEmbedder())
	id := domain.InstanceID("charlie")

	require.NoError(t, store.Write(ctx, id, Entry{Key: "mood", Content: "happy"}))
	require.NoError(t, store.Write(ctx, id, Entry{Key: "mood", Content: "excited"}))

	got, err := store.Read(ctx, id)
	require.NoError(t, err)
	require.Equal(t, []Entry{{Key: "mood", Content: "excited"}}, got)
}

func TestIndexedStore_Delete(t *testing.T) {
	ctx := t.Context()
	store := newTestIndexedStore(t, trivialEmbedder())
	id := domain.InstanceID("dave")

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
	id := domain.InstanceID("searcher")

	require.NoError(t, store.Write(ctx, id, Entry{Key: "cat_fact", Content: "cats sleep 16 hours a day"}))
	require.NoError(t, store.Write(ctx, id, Entry{Key: "dog_fact", Content: "dogs are loyal companions"}))
	require.NoError(t, store.Write(ctx, id, Entry{Key: "fish_fact", Content: "fish breathe through gills"}))

	results, err := store.Search(ctx, id, "cats are great", 3)
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
	id := domain.InstanceID("limited")

	require.NoError(t, store.Write(ctx, id, Entry{Key: "one", Content: "a first entry"}))
	require.NoError(t, store.Write(ctx, id, Entry{Key: "two", Content: "b second entry"}))
	require.NoError(t, store.Write(ctx, id, Entry{Key: "three", Content: "c third entry"}))

	results, err := store.Search(ctx, id, "a query", 1)
	require.NoError(t, err)
	require.Equal(t, []SearchResult{
		{Entry: Entry{Key: "one", Content: "a first entry"}, Similarity: 1.0},
	}, results)
}

func TestIndexedStore_Search_zero_limit_returns_all(t *testing.T) {
	ctx := t.Context()
	store := newTestIndexedStore(t, trivialEmbedder())
	id := domain.InstanceID("zerolimit")

	require.NoError(t, store.Write(ctx, id, Entry{Key: "one", Content: "first"}))
	require.NoError(t, store.Write(ctx, id, Entry{Key: "two", Content: "second"}))
	require.NoError(t, store.Write(ctx, id, Entry{Key: "three", Content: "third"}))

	results, err := store.Search(ctx, id, "query", 0)
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
	id := domain.InstanceID("neglimit")

	require.NoError(t, store.Write(ctx, id, Entry{Key: "one", Content: "first"}))
	require.NoError(t, store.Write(ctx, id, Entry{Key: "two", Content: "second"}))

	results, err := store.Search(ctx, id, "query", -1)
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
	id := domain.InstanceID("deleter")

	require.NoError(t, store.Write(ctx, id, Entry{Key: "cat_fact", Content: "cats sleep a lot"}))
	require.NoError(t, store.Write(ctx, id, Entry{Key: "dog_fact", Content: "dogs are loyal"}))

	require.NoError(t, store.Delete(ctx, id, "cat_fact"))

	results, err := store.Search(ctx, id, "cats", 5)
	require.NoError(t, err)
	require.Equal(t, []SearchResult{
		{Entry: Entry{Key: "dog_fact", Content: "dogs are loyal"}, Similarity: 0},
	}, results)
}

func TestIndexedStore_Search_after_overwrite(t *testing.T) {
	ctx := t.Context()
	topics := map[string]int{"cats": 0, "dogs": 1}
	store := newTestIndexedStore(t, fakeEmbedder(2, topics))
	id := domain.InstanceID("overwriter")

	require.NoError(t, store.Write(ctx, id, Entry{Key: "fact", Content: "cats are great"}))
	require.NoError(t, store.Write(ctx, id, Entry{Key: "fact", Content: "dogs are great"}))

	results, err := store.Search(ctx, id, "dogs", 5)
	require.NoError(t, err)
	require.Equal(t, []SearchResult{
		{Entry: Entry{Key: "fact", Content: "dogs are great"}, Similarity: 1.0},
	}, results)
}

// --- Layer 2: Embedding error handling ---

func TestIndexedStore_Write_indexing_failure_still_persists(t *testing.T) {
	ctx := t.Context()
	store := newTestIndexedStore(t, failingEmbedder())
	id := domain.InstanceID("embedfail")

	require.NoError(t, store.Write(ctx, id, Entry{Key: "k", Content: "v"}))

	got, err := store.Read(ctx, id)
	require.NoError(t, err)
	require.Equal(t, []Entry{{Key: "k", Content: "v"}}, got)
}

func TestIndexedStore_Search_embedding_failure_on_query(t *testing.T) {
	ctx := t.Context()
	id := domain.InstanceID("searchfail")
	db := chromem.NewDB()

	// Seed the collection with a document using pre-computed embeddings
	// so the collection is non-empty.
	col, err := db.GetOrCreateCollection(string(id), nil, failingEmbedder())
	require.NoError(t, err)
	require.NoError(t, col.Add(ctx,
		[]string{"k"},
		[][]float32{{1.0, 0.0, 0.0}},
		[]map[string]string{{"key": "k", "content": "v"}},
		[]string{"k: v"},
	))

	backing := NewStoreAdapter(storetest.NewMemoryStore(t))
	// ensureIndexed clears the whole collection when the backing store
	// has nothing for id (see clearCollection); a matching backing
	// entry keeps this test's seeded document alive so the failure
	// under test still comes from the query phase, not reconciliation
	// wiping it out first.
	require.NoError(t, backing.Write(ctx, id, Entry{Key: "k", Content: "v"}))
	store := NewIndexedStoreFromDB(ctx, backing, db, failingEmbedder())

	_, err = store.Search(ctx, id, "query", 5)
	require.Error(t, err)
}

// --- Layer 4: Persistence ---

func TestIndexedStore_persistence(t *testing.T) {
	ctx := t.Context()
	indexDir := t.TempDir()
	id := domain.InstanceID("persist")

	embedder := fakeEmbedder(3, map[string]int{"cats": 0, "dogs": 1, "fish": 2})

	// Use a single SQLite store to simulate persistence across reopens.
	sqlStore := storetest.NewMemoryStore(t)

	backing1 := NewStoreAdapter(sqlStore)
	store1, err := NewIndexedStore(ctx, backing1, indexDir, embedder)
	require.NoError(t, err)

	require.NoError(t, store1.Write(ctx, id, Entry{Key: "cat", Content: "cats are great"}))
	require.NoError(t, store1.Write(ctx, id, Entry{Key: "dog", Content: "dogs are loyal"}))

	// Reopen: new adapter and index on the same backing store and directory.
	backing2 := NewStoreAdapter(sqlStore)
	store2, err := NewIndexedStore(ctx, backing2, indexDir, embedder)
	require.NoError(t, err)

	got, err := store2.Read(ctx, id)
	require.NoError(t, err)
	require.Equal(t, []Entry{
		{Key: "cat", Content: "cats are great"},
		{Key: "dog", Content: "dogs are loyal"},
	}, got)

	results, err := store2.Search(ctx, id, "cats", 1)
	require.NoError(t, err)
	require.Equal(t, []SearchResult{
		{Entry: Entry{Key: "cat", Content: "cats are great"}, Similarity: 1.0},
	}, results)
}

// --- Lazy reindex ---

func TestIndexedStore_Search_reindexes_from_backing_store(t *testing.T) {
	ctx := t.Context()
	embedder := fakeEmbedder(2, map[string]int{"cats": 0, "dogs": 1})
	id := domain.InstanceID("reindex")

	// Write entries directly to the backing store (simulating migration
	// from a plain store with no vector index).
	backing := NewStoreAdapter(storetest.NewMemoryStore(t))
	require.NoError(t, backing.Write(ctx, id, Entry{Key: "cat", Content: "cats are great"}))
	require.NoError(t, backing.Write(ctx, id, Entry{Key: "dog", Content: "dogs are loyal"}))

	store := NewIndexedStoreFromDB(ctx, backing, chromem.NewDB(), embedder)

	// Search should trigger a lazy reindex and return results.
	results, err := store.Search(ctx, id, "cats", 1)
	require.NoError(t, err)
	require.Equal(t, []SearchResult{
		{Entry: Entry{Key: "cat", Content: "cats are great"}, Similarity: 1.0},
	}, results)
}

// countingEmbedder wraps fn and counts every call it receives, so a
// test can pin exactly how many embed calls a search performed. In
// particular, reconciling a partially-diverged index should cost one
// embed call per missing entry, not one per entry the index already
// has.
func countingEmbedder(fn chromem.EmbeddingFunc) (chromem.EmbeddingFunc, *atomic.Int32) {
	var calls atomic.Int32

	return func(ctx context.Context, text string) ([]float32, error) {
		calls.Add(1)
		return fn(ctx, text)
	}, &calls
}

// TestIndexedStore_Search_reconverges_a_partially_diverged_index
// covers the reconvergence finding directly: a crash between the
// backing-store write and the index write (or any other event that
// removes one entry's document from chromem without touching SQLite)
// leaves the collection short of the backing store's count. The next
// Search must notice the mismatch and reindex only the missing entry.
// The embed-call count confirms it: it would be higher if the
// untouched entry were re-embedded too. Both entries answer correctly
// afterwards.
func TestIndexedStore_Search_reconverges_a_partially_diverged_index(t *testing.T) {
	ctx := t.Context()
	topics := map[string]int{"cats": 0, "dogs": 1}
	embedder, calls := countingEmbedder(fakeEmbedder(2, topics))
	store := newTestIndexedStore(t, embedder)
	id := domain.InstanceID("diverged")

	// newTestIndexedStore's construction already spent one embed call
	// on its probe; reset the counter so what follows counts only the
	// calls this test cares about.
	calls.Store(0)

	require.NoError(t, store.Write(ctx, id, Entry{Key: "cat_fact", Content: "cats are great"}))
	require.NoError(t, store.Write(ctx, id, Entry{Key: "dog_fact", Content: "dogs are loyal"}))
	require.Equal(t, int32(2), calls.Load(), "one embed call per write")

	// Simulate the crash: the collection loses one document, but the
	// backing store (and the other document) are untouched.
	col, err := store.collection(id)
	require.NoError(t, err)
	require.NoError(t, col.Delete(ctx, nil, nil, "dog_fact"))
	require.Equal(t, 1, col.Count(), "the index is now short of the backing store's 2 entries")

	results, err := store.Search(ctx, id, "dogs", 5)
	require.NoError(t, err)
	require.ElementsMatch(t, []SearchResult{
		{Entry: Entry{Key: "cat_fact", Content: "cats are great"}, Similarity: 0},
		{Entry: Entry{Key: "dog_fact", Content: "dogs are loyal"}, Similarity: 1.0},
	}, results)

	require.Equal(t, 2, col.Count(), "the missing entry was reindexed")
	require.Equal(t, int32(4), calls.Load(),
		"the 2 writes, plus one embed call to reindex the single missing entry and one for the query itself, none for the untouched entry")
}

// TestIndexedStore_ensureIndexed_is_a_noop_when_counts_already_match
// pins the cheap path: when the collection already holds as many
// documents as the backing store, ensureIndexed must not re-embed
// anything on every search.
func TestIndexedStore_ensureIndexed_is_a_noop_when_counts_already_match(t *testing.T) {
	ctx := t.Context()
	topics := map[string]int{"cats": 0}
	embedder, calls := countingEmbedder(fakeEmbedder(1, topics))
	store := newTestIndexedStore(t, embedder)
	id := domain.InstanceID("steady")

	calls.Store(0)

	require.NoError(t, store.Write(ctx, id, Entry{Key: "fact", Content: "cats are great"}))
	require.Equal(t, int32(1), calls.Load())

	_, err := store.Search(ctx, id, "cats", 5)
	require.NoError(t, err)

	// The query text costs one embed call; reconciling an already-
	// converged index must cost none on top of it.
	require.Equal(t, int32(2), calls.Load())
}

// --- Orphaned and masked index documents ---

// TestIndexedStore_Search_removes_an_orphaned_index_document covers
// the class of divergence a failed col.Delete inside Delete leaves
// behind: a document survives in the index after its backing entry
// is gone, so it would otherwise answer later searches with content
// the store no longer holds. ensureIndexed must remove it even
// though every backing entry is already indexed and nothing is
// missing.
func TestIndexedStore_Search_removes_an_orphaned_index_document(t *testing.T) {
	ctx := t.Context()
	topics := map[string]int{"cats": 0, "dogs": 1}
	store := newTestIndexedStore(t, fakeEmbedder(2, topics))
	id := domain.InstanceID("orphaned")

	require.NoError(t, store.Write(ctx, id, Entry{Key: "cat_fact", Content: "cats are great"}))

	// Index a document directly, bypassing the backing store: the
	// same shape a failed col.Delete inside Delete leaves, a document
	// with no corresponding backing row.
	require.NoError(t, store.index(ctx, id, Entry{Key: "orphan", Content: "dogs are loyal"}))

	col, err := store.collection(id)
	require.NoError(t, err)
	require.Equal(t, 2, col.Count(), "the orphan sits alongside the real entry before reconciling")

	results, err := store.Search(ctx, id, "dogs", 5)
	require.NoError(t, err)
	require.Equal(t, []SearchResult{
		{Entry: Entry{Key: "cat_fact", Content: "cats are great"}, Similarity: 0},
	}, results, "only the real entry survives to answer a search")

	require.Equal(t, 1, col.Count(), "the orphan was removed")

	_, err = col.GetByID(ctx, "orphan")
	require.Error(t, err, "the orphan is gone")
}

// TestIndexedStore_Search_reconverges_a_masked_divergence covers the
// case a count comparison alone cannot see: one entry missing from
// the index and one orphaned document left in it are the same size
// change in opposite directions, so the two counts agree while the
// content does not. ensureIndexed must still find and correct both.
func TestIndexedStore_Search_reconverges_a_masked_divergence(t *testing.T) {
	ctx := t.Context()
	topics := map[string]int{"cats": 0, "dogs": 1, "fish": 2}
	store := newTestIndexedStore(t, fakeEmbedder(3, topics))
	id := domain.InstanceID("masked")

	require.NoError(t, store.Write(ctx, id, Entry{Key: "cat_fact", Content: "cats are great"}))
	require.NoError(t, store.Write(ctx, id, Entry{Key: "dog_fact", Content: "dogs are loyal"}))

	col, err := store.collection(id)
	require.NoError(t, err)

	// Simulate the masked divergence directly: dog_fact's document is
	// gone from the index, and an orphan takes its place, so the
	// count stays at 2 even though the content has diverged.
	require.NoError(t, col.Delete(ctx, nil, nil, "dog_fact"))
	require.NoError(t, store.index(ctx, id, Entry{Key: "orphan", Content: "fish breathe through gills"}))
	require.Equal(t, 2, col.Count(), "counts coincide even though the content has diverged")

	results, err := store.Search(ctx, id, "dogs", 5)
	require.NoError(t, err)
	require.Equal(t, []SearchResult{
		{Entry: Entry{Key: "dog_fact", Content: "dogs are loyal"}, Similarity: 1.0},
		{Entry: Entry{Key: "cat_fact", Content: "cats are great"}, Similarity: 0},
	}, results, "the missing entry is back and the orphan no longer answers")

	require.Equal(t, 2, col.Count(), "the collection holds exactly the 2 real entries again")

	_, err = col.GetByID(ctx, "orphan")
	require.Error(t, err, "the orphan is gone")
}

// TestIndexedStore_Search_empties_the_collection_when_the_last_entry_is_deleted
// covers the case ensureIndexed's early return on zero backing entries
// left open: an instance that deletes its last memory, leaving the
// backing store empty but its chromem document still indexed (the
// same shape a failed col.Delete inside Delete leaves), kept a
// nonzero collection whose orphaned document went on answering
// search_memory with the deleted content. With zero backing entries
// every document the collection holds is by definition orphaned, so
// ensureIndexed must clear the whole collection rather than return
// early.
func TestIndexedStore_Search_empties_the_collection_when_the_last_entry_is_deleted(t *testing.T) {
	ctx := t.Context()
	topics := map[string]int{"cats": 0}
	store := newTestIndexedStore(t, fakeEmbedder(1, topics))
	id := domain.InstanceID("last-deleted")

	require.NoError(t, store.Write(ctx, id, Entry{Key: "cat_fact", Content: "cats are great"}))

	// Delete the backing row directly, bypassing IndexedStore.Delete:
	// the same shape a failed col.Delete inside Delete leaves, a
	// backing store with nothing left for this instance and a
	// document still sitting in the index.
	require.NoError(t, store.backing.Delete(ctx, id, "cat_fact"))

	col, err := store.collection(id)
	require.NoError(t, err)
	require.Equal(t, 1, col.Count(), "the orphaned document is still indexed")

	results, err := store.Search(ctx, id, "cats", 5)
	require.NoError(t, err)
	require.Empty(t, results, "the deleted entry must not answer a search")

	// col is now stale: ensureIndexed deleted the collection it named,
	// so a fresh handle is what answers for the current state.
	col, err = store.collection(id)
	require.NoError(t, err)
	require.Zero(t, col.Count(), "the collection was cleared")
}

// --- Embedding model change ---

// TestIndexedStore_ReconcileEmbeddingModel_first_call_records_the_model
// pins that establishing the baseline on a fresh index (no previous
// model recorded yet) does not reset anything: there is nothing to
// reconcile against on the very first call.
func TestIndexedStore_ReconcileEmbeddingModel_first_call_records_the_model(t *testing.T) {
	ctx := t.Context()
	store := newTestIndexedStore(t, fakeEmbedder(2, map[string]int{"cats": 0, "dogs": 1}))
	id := domain.InstanceID("fresh")

	require.NoError(t, store.ReconcileEmbeddingModel(ctx, "model-a"))
	require.NoError(t, store.Write(ctx, id, Entry{Key: "fact", Content: "cats are great"}))

	col, err := store.collection(id)
	require.NoError(t, err)
	require.Equal(t, 1, col.Count())
}

// TestIndexedStore_ReconcileEmbeddingModel_unchanged_model_is_a_noop
// pins that calling it again with the same model leaves an already-
// built index alone.
func TestIndexedStore_ReconcileEmbeddingModel_unchanged_model_is_a_noop(t *testing.T) {
	ctx := t.Context()
	store := newTestIndexedStore(t, fakeEmbedder(2, map[string]int{"cats": 0, "dogs": 1}))
	id := domain.InstanceID("steady")

	require.NoError(t, store.ReconcileEmbeddingModel(ctx, "model-a"))
	require.NoError(t, store.Write(ctx, id, Entry{Key: "fact", Content: "cats are great"}))

	require.NoError(t, store.ReconcileEmbeddingModel(ctx, "model-a"))

	col, err := store.collection(id)
	require.NoError(t, err)
	require.Equal(t, 1, col.Count(), "the same model must not reset the index")
}

// TestIndexedStore_ReconcileEmbeddingModel_changed_model_resets_the_index_and_reconverges
// covers the model-change finding end to end: entries embedded under
// one model are meaningless to query vectors built from another, so
// a changed model wipes the vector index (the backing store stays
// untouched) and composes with ensureIndexed's reconciliation to
// rebuild it lazily, correctly, on the next search.
func TestIndexedStore_ReconcileEmbeddingModel_changed_model_resets_the_index_and_reconverges(t *testing.T) {
	ctx := t.Context()
	topics := map[string]int{"cats": 0, "dogs": 1}
	store := newTestIndexedStore(t, fakeEmbedder(2, topics))
	id := domain.InstanceID("migrated")

	require.NoError(t, store.ReconcileEmbeddingModel(ctx, "model-a"))
	require.NoError(t, store.Write(ctx, id, Entry{Key: "cat_fact", Content: "cats are great"}))

	require.NoError(t, store.ReconcileEmbeddingModel(ctx, "model-b"))

	col, err := store.collection(id)
	require.NoError(t, err)
	require.Zero(t, col.Count(), "the vector index was wiped")

	entries, err := store.Read(ctx, id)
	require.NoError(t, err)
	require.Equal(t, []Entry{{Key: "cat_fact", Content: "cats are great"}}, entries,
		"the backing store is untouched by an embedding-model change")

	results, err := store.Search(ctx, id, "cats", 5)
	require.NoError(t, err)
	require.Equal(t, []SearchResult{
		{Entry: Entry{Key: "cat_fact", Content: "cats are great"}, Similarity: 1.0},
	}, results, "the next search reconverges the index under the new model")
}

// TestIndexedStore_ReconcileEmbeddingModel_persists_across_reopen
// pins the cross-restart half of the finding: a model change made
// while the app was closed (nothing in this process ever called
// ReconcileEmbeddingModel with the old model) is still caught on the
// next open, because NewIndexedStore persists what it saw to a
// marker file in indexDir.
func TestIndexedStore_ReconcileEmbeddingModel_persists_across_reopen(t *testing.T) {
	ctx := t.Context()
	indexDir := t.TempDir()
	id := domain.InstanceID("persist")
	topics := map[string]int{"cats": 0, "dogs": 1}

	sqlStore := storetest.NewMemoryStore(t)

	backing1 := NewStoreAdapter(sqlStore)
	store1, err := NewIndexedStore(ctx, backing1, indexDir, fakeEmbedder(2, topics))
	require.NoError(t, err)
	require.NoError(t, store1.ReconcileEmbeddingModel(ctx, "model-a"))
	require.NoError(t, store1.Write(ctx, id, Entry{Key: "cat_fact", Content: "cats are great"}))

	// Reopen against the same indexDir and backing store, as a fresh
	// process would: store2 starts with no in-memory record of
	// "model-a", so it must recover it from the marker file store1
	// wrote.
	backing2 := NewStoreAdapter(sqlStore)
	store2, err := NewIndexedStore(ctx, backing2, indexDir, fakeEmbedder(2, topics))
	require.NoError(t, err)

	require.NoError(t, store2.ReconcileEmbeddingModel(ctx, "model-b"))

	col, err := store2.collection(id)
	require.NoError(t, err)
	require.Zero(t, col.Count(), "the model change since the last open was still caught")

	results, err := store2.Search(ctx, id, "cats", 5)
	require.NoError(t, err)
	require.Equal(t, []SearchResult{
		{Entry: Entry{Key: "cat_fact", Content: "cats are great"}, Similarity: 1.0},
	}, results)
}
