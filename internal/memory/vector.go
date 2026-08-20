package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	chromem "github.com/philippgille/chromem-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
)

var (
	_ Store           = (*IndexedStore)(nil)
	_ Searcher        = (*IndexedStore)(nil)
	_ InstanceDeleter = (*IndexedStore)(nil)
)

// IndexedStore wraps a backing [Store] (in production, a
// [StoreAdapter] over SQLite) with a chromem-go vector index to
// provide semantic search. CRUD operations delegate to the backing
// store. Writes and deletes also update the vector index, but
// indexing failures are logged rather than returned: the backing
// store is the source of truth, and [IndexedStore.ensureIndexed]
// reconciles the index against it on every search.
type IndexedStore struct {
	backing       Store
	db            *chromem.DB
	embeddingFunc chromem.EmbeddingFunc

	// indexDir is the directory NewIndexedStore opened db from, and
	// the root ReconcileEmbeddingModel persists its marker file
	// under. Empty for a store built via NewIndexedStoreFromDB (tests
	// and in-memory callers), which makes ReconcileEmbeddingModel's
	// persistence a no-op: there is no directory of this store's own
	// to keep it in.
	indexDir string

	// embeddingModelMu guards embeddingModel: ReconcileEmbeddingModel
	// reads the previous value, decides whether to reset the index,
	// and records the new one as one step, and a concurrent config
	// change must not interleave with that.
	embeddingModelMu sync.Mutex
	embeddingModel   string

	// searchable is the live outcome of the most recent probe of
	// embeddingFunc — see probeEmbeddingFunc. Search always has a
	// real chromem method regardless of whether the endpoint behind
	// it works, so this is the signal Searchable exposes for callers
	// deciding whether calling it will actually work. Set once at
	// construction and updated again by RefreshSearchable whenever a
	// caller knows the underlying endpoint may have changed (e.g.
	// NewDefaultStore, on every relevant config change).
	searchable atomic.Bool

	// probeErr is the error from that same probe, or nil when it
	// succeeded. ProbeError exposes it so a caller that just changed
	// the embedding configuration can report why search stayed
	// unavailable, not only that it did.
	probeErr atomic.Pointer[error]

	// tracerProvider is the OTel `TracerProvider` the store uses for
	// its spans. Defaults to `otel.GetTracerProvider()`; tests inject
	// a per-test recorder via `WithTracerProvider`.
	tracerProvider trace.TracerProvider
}

// NewIndexedStore creates an IndexedStore backed by the given memory
// Store and a persistent chromem-go database at indexDir. A single
// probe call to embeddingFunc during construction, on ctx, determines
// what Searchable reports until a caller calls RefreshSearchable.
// Threading ctx through matters: chromem-go's OpenAI-compatible
// embedding function builds its HTTP client with no timeout of its
// own by design, relying on the caller's context for cancellation, so
// a caller-supplied ctx (ultimately the process's own cancellable
// root context) is what keeps a black-holed endpoint from hanging
// construction — and therefore application startup — uncancellably.
func NewIndexedStore(ctx context.Context, backing Store, indexDir string, embeddingFunc chromem.EmbeddingFunc) (*IndexedStore, error) {
	db, err := chromem.NewPersistentDB(indexDir, false)
	if err != nil {
		return nil, fmt.Errorf("open vector index: %w", err)
	}

	s := &IndexedStore{
		backing:        backing,
		db:             db,
		indexDir:       indexDir,
		tracerProvider: otel.GetTracerProvider(),
	}
	s.embeddingFunc = s.instrumentEmbedding(embeddingFunc)
	s.storeProbeResult(probeEmbeddingFunc(ctx, embeddingFunc))

	return s, nil
}

// NewIndexedStoreFromDB creates an IndexedStore from an existing
// chromem-go DB. This allows callers to provide an in-memory database
// for testing while using a persistent one in production. As with
// NewIndexedStore, a single probe call to embeddingFunc during
// construction, on ctx, determines what Searchable reports until a
// caller calls RefreshSearchable.
func NewIndexedStoreFromDB(ctx context.Context, backing Store, db *chromem.DB, embeddingFunc chromem.EmbeddingFunc) *IndexedStore {
	s := &IndexedStore{
		backing:        backing,
		db:             db,
		tracerProvider: otel.GetTracerProvider(),
	}
	s.embeddingFunc = s.instrumentEmbedding(embeddingFunc)
	s.storeProbeResult(probeEmbeddingFunc(ctx, embeddingFunc))

	return s
}

// probeEmbeddingFunc performs a single, cheap call to fn to confirm
// the configured embedding endpoint actually responds — the
// OpenRouter-compatible base URL a Config otherwise defaults to
// serves no /embeddings endpoint, so without this check a broken
// endpoint is discovered only when a model calls search_memory
// and gets an error. A nil fn (no API key configured) short-circuits
// without a call and reports no error, since there is nothing to
// blame yet: a fresh install has no key by design.
func probeEmbeddingFunc(ctx context.Context, fn chromem.EmbeddingFunc) (bool, error) {
	if fn == nil {
		return false, nil
	}

	_, err := fn(ctx, "modeloff memory index probe")

	return err == nil, err
}

// WithTracerProvider overrides the OTel `TracerProvider` the store
// uses for its spans. Tests inject a per-test recorder so span
// recordings stay scoped to a single test rather than relying on the
// global provider's swap-and-restore. The instrumented embedding
// closure captures `s` by pointer, so changes made via this method
// are visible on subsequent embed calls.
func (s *IndexedStore) WithTracerProvider(tp trace.TracerProvider) *IndexedStore {
	s.tracerProvider = tp

	return s
}

func (s *IndexedStore) tracer() trace.Tracer {
	return s.tracerProvider.Tracer("github.com/laney/modeloff/internal/memory")
}

// inSpan brackets fn with a span and result-recording on the store's
// tracer provider. See `observability.SpanRunner`. Underlying
// failures are tagged `ErrorKindStore` since the indexed store is a
// thin facade over the backing store and the chromem index.
func (s *IndexedStore) inSpan(
	ctx context.Context,
	op string,
	attrs []attribute.KeyValue,
	fn func(ctx context.Context, span trace.Span) error,
) error {
	return observability.SpanRunner{
		Tracer:         s.tracer(),
		DefaultErrKind: observability.ErrorKindStore,
	}.Run(ctx, op, attrs, fn)
}

func (s *IndexedStore) collection(id domain.InstanceID) (*chromem.Collection, error) {
	return s.db.GetOrCreateCollection(string(id), nil, s.embeddingFunc)
}

// Read delegates to the backing store.
func (s *IndexedStore) Read(ctx context.Context, id domain.InstanceID) ([]Entry, error) {
	return s.backing.Read(ctx, id)
}

// Searchable reports the outcome of the most recent probe of this
// store's embedding function — the initial one at construction, or
// the latest RefreshSearchable call. Search always exists as a
// method regardless; this is the signal that calling it will
// actually reach a working embedding endpoint.
func (s *IndexedStore) Searchable() bool {
	return s.searchable.Load()
}

// ProbeError returns the error from the most recent probe of this
// store's embedding function, or nil if that probe succeeded (or no
// API key was configured for it to attempt).
func (s *IndexedStore) ProbeError() error {
	return *s.probeErr.Load()
}

// RefreshSearchable re-probes this store's embedding function,
// updates the outcome Searchable and ProbeError report, and returns
// the probe's error, or nil on success. embeddingFunc reads its
// underlying endpoint through an atomic pointer indirection
// (NewDefaultStore's storeEmbedder), so the same closure passed to
// the constructor already reflects a config change by the time this
// runs — a caller re-probes to learn whether that change turned a
// working endpoint into a broken one, or vice versa, without
// reattaching the store.
func (s *IndexedStore) RefreshSearchable(ctx context.Context) error {
	ok, err := probeEmbeddingFunc(ctx, s.embeddingFunc)
	s.storeProbeResult(ok, err)

	return err
}

// storeProbeResult records a probe's outcome for Searchable and
// ProbeError to report.
func (s *IndexedStore) storeProbeResult(ok bool, err error) {
	s.searchable.Store(ok)
	s.probeErr.Store(&err)
}

// Search finds memories semantically similar to the query, returning
// up to limit results ordered by descending similarity. Before
// querying, ensureIndexed reconciles the instance's collection
// against the backing store, so a divergence between the two (nothing
// indexed yet, or an index missing entries the store still has) is
// repaired first.
func (s *IndexedStore) Search(ctx context.Context, id domain.InstanceID, query string, limit int) ([]SearchResult, error) {
	var searchResults []SearchResult
	err := s.inSpan(ctx, "memory.search",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, span trace.Span) error {
			if err := s.ensureIndexed(ctx, id); err != nil {
				return fmt.Errorf("ensure indexed: %w", err)
			}

			col, err := s.collection(id)
			if err != nil {
				return fmt.Errorf("get collection: %w", err)
			}

			count := col.Count()
			if count == 0 {
				observability.RecordMemorySearchResults(ctx, 0)
				span.SetAttributes(attribute.Int(observability.AttrSearchResults, 0))
				searchResults = []SearchResult{}
				return nil
			}

			if limit <= 0 || limit > count {
				limit = count
			}

			results, err := col.Query(ctx, query, limit, nil, nil)
			if err != nil {
				return fmt.Errorf("search: %w", err)
			}

			searchResults = make([]SearchResult, 0, len(results))
			for _, r := range results {
				key, ok := r.Metadata["key"]
				if !ok {
					continue
				}

				content, ok := r.Metadata["content"]
				if !ok {
					continue
				}

				searchResults = append(searchResults, SearchResult{
					Entry:      Entry{Key: key, Content: content, At: parseEntryAt(r.Metadata["at"])},
					Similarity: r.Similarity,
				})
			}

			observability.RecordMemorySearchResults(ctx, len(searchResults))
			span.SetAttributes(attribute.Int(observability.AttrSearchResults, len(searchResults)))

			if len(searchResults) > 0 {
				observability.RecordMemorySearchTopScore(ctx, float64(searchResults[0].Similarity))
				span.SetAttributes(
					attribute.Float64(observability.AttrSearchTopScore, float64(searchResults[0].Similarity)),
				)
			}

			return nil
		})
	if err != nil {
		return nil, err
	}

	return searchResults, nil
}

// Write persists the entry to the backing store, then indexes it in
// the vector database. If indexing fails, the error is logged but not
// returned: the entry is still saved, and the next Search's
// ensureIndexed call reconciles the index against it.
func (s *IndexedStore) Write(ctx context.Context, id domain.InstanceID, entry Entry) error {
	return s.inSpan(ctx, "memory.write",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			if err := s.backing.Write(ctx, id, entry); err != nil {
				return err
			}

			if err := s.index(ctx, id, entry); err != nil {
				slog.Default().WarnContext(ctx, "failed to index memory",
					"instance_id", string(id), "key", entry.Key, "error", err)
			}

			return nil
		})
}

func (s *IndexedStore) index(ctx context.Context, id domain.InstanceID, entry Entry) error {
	col, err := s.collection(id)
	if err != nil {
		return fmt.Errorf("get collection: %w", err)
	}

	// Remove any existing document with this key so overwrites work.
	_ = col.Delete(ctx, nil, nil, entry.Key)

	doc := chromem.Document{
		ID:      entry.Key,
		Content: entry.Key + ": " + entry.Content,
		Metadata: map[string]string{
			"key":     entry.Key,
			"content": entry.Content,
			"at":      entry.At.Format(time.RFC3339Nano),
		},
	}

	return col.AddDocuments(ctx, []chromem.Document{doc}, 1)
}

// parseEntryAt reads a chromem document's "at" metadata value back
// into a time.Time. A document indexed before this metadata existed,
// or any other unparsable value, is not an error worth surfacing: it
// reads back as the zero time, [Entry.At]'s own zero-value meaning.
func parseEntryAt(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}

	return t
}

// Delete removes the entry from the vector index, then from the
// backing store. If the vector delete fails, it is logged but the
// backing store delete still proceeds.
func (s *IndexedStore) Delete(ctx context.Context, id domain.InstanceID, key string) error {
	return s.inSpan(ctx, "memory.delete",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			col, err := s.collection(id)
			if err != nil {
				slog.Default().WarnContext(ctx, "failed to get collection for delete",
					"instance_id", string(id), "key", key, "error", err)
			} else if err := col.Delete(ctx, nil, nil, key); err != nil {
				slog.Default().WarnContext(ctx, "failed to remove memory from index",
					"instance_id", string(id), "key", key, "error", err)
			}

			return s.backing.Delete(ctx, id, key)
		})
}

// DeleteInstance removes the given instance's chromem-go vector
// collection. It is a no-op if the instance never had a collection.
// The backing store's memories rows for the same instance are
// removed automatically when the instance's own row is deleted (see
// [InstanceDeleter]'s doc comment); this only needs to clear index
// state chromem owns independently of that row.
func (s *IndexedStore) DeleteInstance(ctx context.Context, id domain.InstanceID) error {
	return s.inSpan(ctx, "memory.delete_instance",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(_ context.Context, _ trace.Span) error {
			if err := s.db.DeleteCollection(string(id)); err != nil {
				return fmt.Errorf("delete collection: %w", err)
			}

			return nil
		})
}

// Reset removes all memories from both the vector index and the
// backing store.
func (s *IndexedStore) Reset(ctx context.Context) error {
	return s.inSpan(ctx, "memory.reset", nil, func(ctx context.Context, _ trace.Span) error {
		if err := s.db.Reset(); err != nil {
			return fmt.Errorf("reset vector index: %w", err)
		}

		return s.backing.Reset(ctx)
	})
}

// ensureIndexed reconciles an instance's collection against the
// backing store, which is the source of truth: it lists both sides
// by key and repairs any difference between them, so a divergence
// reconverges on the next search regardless of which direction it
// went in and regardless of whether the two sides happen to be the
// same size. Nothing indexed yet, an index wiped out from under a
// store that survived, a crash between the backing-store write and
// the index write leaving one entry unindexed, and a failed
// col.Delete during Delete leaving a document behind after its entry
// was removed are all the same shape of problem from here: an entry
// the collection is missing, or a document the collection holds that
// names no backing entry.
//
// A comparison keyed only by count would miss the case where one of
// each has happened: an orphaned document and a missing entry of the
// same instance leave the counts equal, and a check that stopped
// there would search on with the missing entry absent and the
// orphan's stale content still answering. Comparing the key sets
// catches both regardless of what the counts say.
//
// An instance with zero backing entries is the extreme case of the
// same problem: every document the collection still holds, if any,
// names no backing entry, so the whole collection is deleted rather
// than reconciled document by document. This is what keeps an
// instance that deleted its last memory from having a stale document
// go on answering search_memory after the backing store has nothing
// left for it.
//
// This is called lazily on Search so callers never need to think
// about reconciling. Missing entries are indexed one at a time on the
// calling goroutine: an instance's memories are a personal,
// human-scale list, not something worth an unbounded goroutine
// fan-out over concurrent embedding calls.
func (s *IndexedStore) ensureIndexed(ctx context.Context, id domain.InstanceID) error {
	entries, err := s.backing.Read(ctx, id)
	if err != nil {
		return fmt.Errorf("read entries for %s: %w", id, err)
	}

	if len(entries) == 0 {
		return s.clearCollection(ctx, id)
	}

	col, err := s.collection(id)
	if err != nil {
		return fmt.Errorf("get collection for %s: %w", id, err)
	}

	backingKeys := make(map[string]struct{}, len(entries))

	var anchor string
	var added int

	for _, entry := range entries {
		backingKeys[entry.Key] = struct{}{}
		anchor = entry.Key

		if _, err := col.GetByID(ctx, entry.Key); err == nil {
			continue
		}

		if err := s.index(ctx, id, entry); err != nil {
			return fmt.Errorf("index entry %s/%s: %w", id, entry.Key, err)
		}

		added++
	}

	removed, err := s.pruneOrphanDocuments(ctx, id, col, anchor, backingKeys)
	if err != nil {
		return err
	}

	if added > 0 || removed > 0 {
		slog.Default().InfoContext(ctx, "reconciled memory index against the backing store",
			"instance_id", string(id),
			"added", added,
			"removed", removed,
		)
	}

	return nil
}

// pruneOrphanDocuments removes every document col holds whose id is
// not a key in backingKeys, and returns how many it removed.
//
// chromem exposes no method to list a collection's document ids
// directly, so this asks for them the way it exposes: an exhaustive
// QueryEmbedding for every document the collection holds (nResults
// set to the collection's own count), which returns the full set
// regardless of relevance ranking. anchor names a document
// ensureIndexed has already guaranteed exists in col (any backing
// entry, once its own loop has run), and QueryEmbedding takes a
// precomputed vector rather than text to embed, so reusing that
// document's own embedding as the query vector costs no embedding-API
// call of its own.
func (s *IndexedStore) pruneOrphanDocuments(
	ctx context.Context,
	id domain.InstanceID,
	col *chromem.Collection,
	anchor string,
	backingKeys map[string]struct{},
) (int, error) {
	count := col.Count()
	if count == 0 {
		return 0, nil
	}

	anchorDoc, err := col.GetByID(ctx, anchor)
	if err != nil {
		return 0, fmt.Errorf("get anchor document %s/%s: %w", id, anchor, err)
	}

	results, err := col.QueryEmbedding(ctx, anchorDoc.Embedding, count, nil, nil)
	if err != nil {
		return 0, fmt.Errorf("enumerate index documents for %s: %w", id, err)
	}

	var removed int
	for _, r := range results {
		if _, ok := backingKeys[r.ID]; ok {
			continue
		}

		if err := col.Delete(ctx, nil, nil, r.ID); err != nil {
			slog.Default().WarnContext(ctx, "failed to remove orphaned memory from index",
				"instance_id", string(id), "key", r.ID, "error", err)

			continue
		}

		removed++
	}

	return removed, nil
}

// clearCollection deletes id's entire chromem collection when the
// backing store holds zero entries for it: every document such a
// collection still holds names no backing entry, so nothing needs an
// anchor or an enumeration to know it is orphaned. A no-op when the
// collection does not exist or is already empty, checked with
// s.db.GetCollection rather than s.collection so that querying an
// instance that has never written a memory does not create one.
func (s *IndexedStore) clearCollection(ctx context.Context, id domain.InstanceID) error {
	col := s.db.GetCollection(string(id), nil)
	if col == nil || col.Count() == 0 {
		return nil
	}

	removed := col.Count()

	if err := s.db.DeleteCollection(string(id)); err != nil {
		return fmt.Errorf("delete collection for %s: %w", id, err)
	}

	slog.Default().InfoContext(ctx, "reconciled memory index against the backing store",
		"instance_id", string(id),
		"added", 0,
		"removed", removed,
	)

	return nil
}

// embeddingModelMarkerFile is the name of the sidecar file
// ReconcileEmbeddingModel persists inside indexDir, recording which
// embedding model most recently built the index.
const embeddingModelMarkerFile = "embedding_model"

// ReconcileEmbeddingModel records model as the embedding model
// backing this store's index and, if a different model built it
// before, wipes the vector index so every instance's collection is
// rebuilt against model on its next search: entries embedded under
// one model are meaningless to query vectors built from another.
// Only the chromem-go index is cleared; the backing store, which
// still holds every entry regardless of what embedded it, is
// untouched, so ensureIndexed's reconciliation is what performs the
// actual reindex, lazily, the next time each instance is searched.
//
// The comparison is best-effort across restarts: when this store was
// built with an on-disk indexDir (NewIndexedStore), the previous
// model is also read from and written to a marker file there, so a
// model change made while the app was closed is still caught on the
// next open. A store built via NewIndexedStoreFromDB has no
// directory of its own, so persistence is skipped and only a change
// within this process's lifetime is caught: the shape every test
// double using an in-memory chromem.DB needs, and no less than what
// NewIndexedStore itself catches on its very first open, before any
// marker file exists to compare against.
//
// Call this once at construction with the configured model, and
// again whenever a config change might have altered it, the same
// moments NewDefaultStore already calls RefreshSearchable.
func (s *IndexedStore) ReconcileEmbeddingModel(ctx context.Context, model string) error {
	s.embeddingModelMu.Lock()
	defer s.embeddingModelMu.Unlock()

	previous := s.embeddingModel
	if previous == "" {
		previous = s.readEmbeddingModelMarker(ctx)
	}

	if previous != "" && previous != model {
		slog.Default().InfoContext(ctx, "embedding model changed, resetting the vector index",
			"previous_model", previous,
			"model", model,
		)

		if err := s.db.Reset(); err != nil {
			return fmt.Errorf("reset vector index for embedding model change: %w", err)
		}
	}

	s.embeddingModel = model
	s.writeEmbeddingModelMarker(ctx, model)

	return nil
}

// readEmbeddingModelMarker reads the persisted embedding-model
// marker from indexDir, or "" if there is no directory to read one
// from, no marker file yet (a fresh index), or the file could not be
// read. A read failure is logged rather than returned:
// ReconcileEmbeddingModel treats it the same as no previous marker,
// which risks a missed reset over a transient filesystem glitch
// rather than wiping a healthy index on one.
func (s *IndexedStore) readEmbeddingModelMarker(ctx context.Context) string {
	if s.indexDir == "" {
		return ""
	}

	data, err := os.ReadFile(filepath.Join(s.indexDir, embeddingModelMarkerFile))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Default().WarnContext(ctx, "read embedding model marker",
				"index_dir", s.indexDir, "error", err)
		}

		return ""
	}

	return string(data)
}

// writeEmbeddingModelMarker persists model to indexDir for a future
// open to compare against. A no-op when this store has no directory
// of its own (NewIndexedStoreFromDB); a write failure is logged, not
// returned, the same as every other index-maintenance failure in
// this store: the backing store remains the source of truth either
// way.
func (s *IndexedStore) writeEmbeddingModelMarker(ctx context.Context, model string) {
	if s.indexDir == "" {
		return
	}

	if err := os.WriteFile(filepath.Join(s.indexDir, embeddingModelMarkerFile), []byte(model), 0o600); err != nil {
		slog.Default().WarnContext(ctx, "write embedding model marker",
			"index_dir", s.indexDir, "error", err)
	}
}

func (s *IndexedStore) instrumentEmbedding(inner chromem.EmbeddingFunc) chromem.EmbeddingFunc {
	return func(ctx context.Context, text string) ([]float32, error) {
		var embedding []float32
		err := s.inSpan(ctx, "memory.embed", nil, func(ctx context.Context, _ trace.Span) error {
			var inErr error
			embedding, inErr = inner(ctx, text)

			return inErr
		})
		if err != nil {
			return nil, err
		}

		return embedding, nil
	}
}
