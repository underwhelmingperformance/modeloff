package modelclient

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
)

// fakeSearchableStore is a memory.Store that also implements
// memory.Searcher with a caller-controlled Searchable outcome, so
// instanceMemory.SearchMemory's gate can be exercised without a real
// IndexedStore and its chromem-go/SQLite dependencies.
type fakeSearchableStore struct {
	searchable bool
	results    []memory.SearchResult
}

func (f *fakeSearchableStore) Read(context.Context, domain.InstanceID) ([]memory.Entry, error) {
	return nil, nil
}

func (f *fakeSearchableStore) Write(context.Context, domain.InstanceID, memory.Entry) error {
	return nil
}

func (f *fakeSearchableStore) Delete(context.Context, domain.InstanceID, string) error {
	return nil
}

func (f *fakeSearchableStore) Reset(context.Context) error {
	return nil
}

func (f *fakeSearchableStore) Search(context.Context, domain.InstanceID, string, int) ([]memory.SearchResult, error) {
	return f.results, nil
}

func (f *fakeSearchableStore) Searchable() bool {
	return f.searchable
}

var (
	_ memory.Store    = (*fakeSearchableStore)(nil)
	_ memory.Searcher = (*fakeSearchableStore)(nil)
)

func TestInstanceMemory_SearchMemory_unsearchable_store_errors(t *testing.T) {
	mem := &instanceMemory{instanceID: "inst-1", store: &fakeSearchableStore{searchable: false}}

	_, err := mem.SearchMemory(t.Context(), "query", 5)
	require.Error(t, err)
}

func TestInstanceMemory_SearchMemory_searchable_store_delegates(t *testing.T) {
	want := []memory.SearchResult{{Entry: memory.Entry{Key: "k", Content: "v"}, Similarity: 0.5}}
	mem := &instanceMemory{instanceID: "inst-1", store: &fakeSearchableStore{searchable: true, results: want}}

	got, err := mem.SearchMemory(t.Context(), "query", 5)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// TestInstanceMemory_SearchMemory_non_searcher_store_errors pins the
// pre-existing behaviour for a Store that doesn't implement Searcher
// at all — e.g. the plain StoreAdapter NewDefaultStore falls back to
// when the vector index itself fails to open.
func TestInstanceMemory_SearchMemory_non_searcher_store_errors(t *testing.T) {
	mem := &instanceMemory{instanceID: "inst-1", store: memory.NewStoreAdapter(nil)}

	_, err := mem.SearchMemory(t.Context(), "query", 5)
	require.Error(t, err)
}

// TestSearchEnabled_matches_instanceMemory_gate pins that
// searchEnabled (tools.go), which decides whether search_memory is
// even advertised as a tool, agrees with instanceMemory.SearchMemory
// (this file), which decides whether an actual call succeeds. If
// these two ever diverged, a model would see search_memory offered
// and then have every call rejected (or the reverse) purely because
// two gates checked different things.
func TestSearchEnabled_matches_instanceMemory_gate(t *testing.T) {
	tests := []struct {
		name  string
		store memory.Store
		want  bool
	}{
		{name: "searchable store", store: &fakeSearchableStore{searchable: true}, want: true},
		{name: "unsearchable store", store: &fakeSearchableStore{searchable: false}, want: false},
		{name: "non-searcher store", store: memory.NewStoreAdapter(nil), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, searchEnabled(tt.store))

			mem := &instanceMemory{instanceID: "inst-1", store: tt.store}
			_, err := mem.SearchMemory(t.Context(), "query", 5)
			require.Equal(t, tt.want, err == nil)
		})
	}
}
