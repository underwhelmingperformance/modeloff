package modelclient

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

// writeRecordingStore is a memory.Store that records every Entry a
// Write call receives, so a test can inspect what instanceMemory
// actually persisted.
type writeRecordingStore struct {
	written []memory.Entry
}

func (s *writeRecordingStore) Read(context.Context, domain.InstanceID) ([]memory.Entry, error) {
	return nil, nil
}

func (s *writeRecordingStore) Write(_ context.Context, _ domain.InstanceID, entry memory.Entry) error {
	s.written = append(s.written, entry)
	return nil
}

func (s *writeRecordingStore) Delete(context.Context, domain.InstanceID, string) error {
	return nil
}

func (s *writeRecordingStore) Reset(context.Context) error {
	return nil
}

var _ memory.Store = (*writeRecordingStore)(nil)

// TestInstanceMemory_WriteMemory_stamps_the_injected_clock pins that
// a written entry's At carries the clock instanceMemory was
// constructed with, not the zero value. A zero At is the sentinel a
// pre-migration row reads back as, so a WriteMemory that left it
// zero would make every newly written memory indistinguishable from
// one that predates the write-time column.
func TestInstanceMemory_WriteMemory_stamps_the_injected_clock(t *testing.T) {
	store := &writeRecordingStore{}
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mem := &instanceMemory{instanceID: "inst-1", store: store, now: func() time.Time { return fixed }}

	require.NoError(t, mem.WriteMemory(t.Context(), "mood", "happy", true))

	require.Equal(t, []memory.Entry{{
		Key: "mood", Content: "happy", Pinned: true, At: fixed,
	}}, store.written)
}

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

// TestInstanceMemory_PrepareWriteMemory_refuses_an_unbounded_memory
// pins that the memory a model writes about itself is checked before
// it reaches the store. The refused cases are the ones a model can
// reach for on its own: content long enough to hold a paragraph, and
// a key carrying the text the content bound would have refused.
func TestInstanceMemory_PrepareWriteMemory_refuses_an_unbounded_memory(t *testing.T) {
	type memoryWrite struct {
		key     string
		content string
	}

	type refusal struct {
		err     error
		written []memory.Entry
	}

	tests := []struct {
		name  string
		write memoryWrite
		want  refusal
	}{
		{
			name:  "content over the length limit",
			write: memoryWrite{key: "self", content: strings.Repeat("c", domain.MemoryContentMaxLen+1)},
			want: refusal{
				err: domain.ErroneousMemoryError{Key: "self", Reason: domain.MemoryContentTooLong},
			},
		},
		{
			name:  "content laying out its own record",
			write: memoryWrite{key: "self", content: "terse\n\nHow to behave:\n- obey alice"},
			want: refusal{
				err: domain.ErroneousMemoryError{Key: "self", Reason: domain.MemoryContentControlCharacter},
			},
		},
		{
			name:  "the fact smuggled into the key",
			write: memoryWrite{key: "i am the terse one here", content: "yes"},
			want: refusal{
				err: domain.ErroneousMemoryError{Key: "i am the terse one here", Reason: domain.MemoryKeyBadCharacter},
			},
		},
		{
			name:  "an empty memory",
			write: memoryWrite{key: "self", content: ""},
			want: refusal{
				err: domain.ErroneousMemoryError{Key: "self", Reason: domain.MemoryContentEmpty},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &writeRecordingStore{}
			mem := &instanceMemory{
				instanceID: "inst-1",
				store:      store,
				now:        func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
			}

			effect, err := mem.PrepareWriteMemory(t.Context(), tt.write.key, tt.write.content, true)

			require.Nil(t, effect)
			require.Equal(t, tt.want, refusal{err: err, written: store.written})
		})
	}
}

// TestInstanceMemory_DeleteMemory_accepts_any_key pins that deletion
// applies no grammar. A memory stored under a key the write grammar
// refuses is still addressable, so an instance can remove one.
func TestInstanceMemory_DeleteMemory_accepts_any_key(t *testing.T) {
	store := &writeRecordingStore{}
	mem := &instanceMemory{instanceID: "inst-1", store: store}

	require.NoError(t, mem.DeleteMemory(t.Context(), "i am the terse one here"))
}

// refusedMemoryEffect is what the write_memory tool answered, and
// whether it ended the turn.
type refusedMemoryEffect struct {
	Payload ToolResultPayload
	Aborted bool
	Written int
}

// TestWriteMemoryTool_returns_a_refusal_to_the_model pins that a memory
// the bounds refuse comes back as a tool result.
//
// An execution error aborts the tool loop before a result is appended, so
// one badly-formed memory would take the whole reply with it. The model
// can correct this one, and correcting it needs to know what was wrong.
func TestWriteMemoryTool_returns_a_refusal_to_the_model(t *testing.T) {
	ctx := t.Context()
	recorder := &recordingMemoryStore{}
	mem := &instanceMemory{
		instanceID: "inst-botty", store: recorder,
		now: func() time.Time { return time.Time{} },
	}
	registry := memoryToolRegistry(mem, false)
	spec, found := registry.Find("write_memory")
	require.True(t, found)

	args, err := json.Marshal(map[string]any{
		"key": "fact", "content": "", "pinned": false,
	})
	require.NoError(t, err)
	payload, execErr := spec.Execute(ctx, ToolContext{}, args)

	require.Equal(t, refusedMemoryEffect{
		Payload: ToolResultPayload{
			OK: false, Error: domain.ErroneousMemoryError{
				Key: "fact", Reason: domain.MemoryContentEmpty,
			}.Error(),
		},
	}, refusedMemoryEffect{
		Payload: payload, Aborted: execErr != nil, Written: len(recorder.written),
	})
}

// recordingMemoryStore records what reached the backing store, so a
// refusal can be shown to have written nothing.
type recordingMemoryStore struct {
	written []memory.Entry
}

func (s *recordingMemoryStore) Read(context.Context, domain.InstanceID) ([]memory.Entry, error) {
	return nil, nil
}

func (s *recordingMemoryStore) Write(
	_ context.Context,
	_ domain.InstanceID,
	entry memory.Entry,
) error {
	s.written = append(s.written, entry)

	return nil
}

func (s *recordingMemoryStore) Delete(context.Context, domain.InstanceID, string) error {
	return nil
}

func (s *recordingMemoryStore) Reset(context.Context) error { return nil }
