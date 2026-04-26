package memory

import (
	"context"
	"fmt"
	"log/slog"

	chromem "github.com/philippgille/chromem-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
)

var (
	_ Store    = (*IndexedStore)(nil)
	_ Searcher = (*IndexedStore)(nil)
)

// IndexedStore wraps a FileStore with a chromem-go vector index to
// provide semantic search. CRUD operations delegate to the underlying
// FileStore. Writes and deletes also update the vector index, but
// indexing failures are logged rather than returned — the FileStore
// is the source of truth.
type IndexedStore struct {
	backing       Store
	db            *chromem.DB
	embeddingFunc chromem.EmbeddingFunc

	// tracerProvider is the OTel `TracerProvider` the store uses for
	// its spans. Defaults to `otel.GetTracerProvider()`; tests inject
	// a per-test recorder via `WithTracerProvider`.
	tracerProvider trace.TracerProvider
}

// NewIndexedStore creates an IndexedStore backed by the given memory
// Store and a persistent chromem-go database at indexDir.
func NewIndexedStore(backing Store, indexDir string, embeddingFunc chromem.EmbeddingFunc) (*IndexedStore, error) {
	db, err := chromem.NewPersistentDB(indexDir, false)
	if err != nil {
		return nil, fmt.Errorf("open vector index: %w", err)
	}

	s := &IndexedStore{
		backing:        backing,
		db:             db,
		tracerProvider: otel.GetTracerProvider(),
	}
	s.embeddingFunc = s.instrumentEmbedding(embeddingFunc)

	return s, nil
}

// NewIndexedStoreFromDB creates an IndexedStore from an existing
// chromem-go DB. This allows callers to provide an in-memory database
// for testing while using a persistent one in production.
func NewIndexedStoreFromDB(backing Store, db *chromem.DB, embeddingFunc chromem.EmbeddingFunc) *IndexedStore {
	s := &IndexedStore{
		backing:        backing,
		db:             db,
		tracerProvider: otel.GetTracerProvider(),
	}
	s.embeddingFunc = s.instrumentEmbedding(embeddingFunc)

	return s
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
// thin facade over the file store and the chromem index.
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

// Read delegates to the underlying FileStore.
func (s *IndexedStore) Read(ctx context.Context, id domain.InstanceID) ([]Entry, error) {
	return s.backing.Read(ctx, id)
}

// Search finds memories semantically similar to the query, returning
// up to limit results ordered by descending similarity. On the first
// call for an instance, the index is rebuilt from the FileStore if
// needed.
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
					Entry:      Entry{Key: key, Content: content},
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

// Write persists the entry to the FileStore, then indexes it in the
// vector database. If indexing fails, the error is logged but not
// returned — the entry is still saved.
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
		},
	}

	return col.AddDocuments(ctx, []chromem.Document{doc}, 1)
}

// Delete removes the entry from the vector index, then from the
// FileStore. If the vector delete fails, it is logged but the
// FileStore delete still proceeds.
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

// Reset removes all memories from both the vector index and the
// FileStore.
func (s *IndexedStore) Reset(ctx context.Context) error {
	if err := s.db.Reset(); err != nil {
		return fmt.Errorf("reset vector index: %w", err)
	}

	return s.backing.Reset(ctx)
}

// reindexInstance reads all entries for an instance from the
// FileStore and indexes them. This handles migration from a plain
// FileStore and recovery from a corrupted index.
func (s *IndexedStore) reindexInstance(ctx context.Context, id domain.InstanceID) error {
	entries, err := s.backing.Read(ctx, id)
	if err != nil {
		return fmt.Errorf("read entries for %s: %w", id, err)
	}

	for _, entry := range entries {
		if err := s.index(ctx, id, entry); err != nil {
			return fmt.Errorf("index entry %s/%s: %w", id, entry.Key, err)
		}
	}

	return nil
}

// ensureIndexed checks whether an instance's collection is empty
// and, if the FileStore has entries, rebuilds the index. This is
// called lazily on Search so callers never need to think about
// reindexing.
func (s *IndexedStore) ensureIndexed(ctx context.Context, id domain.InstanceID) error {
	col, err := s.collection(id)
	if err != nil {
		return fmt.Errorf("get collection for %s: %w", id, err)
	}

	if col.Count() > 0 {
		return nil
	}

	entries, err := s.backing.Read(ctx, id)
	if err != nil {
		return fmt.Errorf("read entries for %s: %w", id, err)
	}

	if len(entries) == 0 {
		return nil
	}

	slog.Default().InfoContext(ctx, "rebuilding memory index", "instance_id", string(id))

	return s.reindexInstance(ctx, id)
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
