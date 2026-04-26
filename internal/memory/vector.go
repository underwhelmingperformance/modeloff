package memory

import (
	"context"
	"fmt"
	"log/slog"

	chromem "github.com/philippgille/chromem-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
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
	ctx, span := s.tracer().Start(ctx, "memory.search")
	span.SetAttributes(
		attribute.String(observability.AttrOperation, "memory.search"),
		attribute.String(observability.AttrInstanceID, string(id)),
	)
	defer span.End()

	if err := s.ensureIndexed(ctx, id); err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())

		return nil, fmt.Errorf("ensure indexed: %w", err)
	}

	col, err := s.collection(id)
	if err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())

		return nil, fmt.Errorf("get collection: %w", err)
	}

	count := col.Count()
	if count == 0 {
		observability.RecordMemorySearchResults(ctx, 0)
		span.SetAttributes(
			attribute.String(observability.AttrResult, observability.ResultOK),
			attribute.Int(observability.AttrSearchResults, 0),
		)

		return []SearchResult{}, nil
	}

	if limit <= 0 || limit > count {
		limit = count
	}

	results, err := col.Query(ctx, query, limit, nil, nil)
	if err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())

		return nil, fmt.Errorf("search: %w", err)
	}

	searchResults := make([]SearchResult, 0, len(results))
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

	searchAttrs := []attribute.KeyValue{
		attribute.String(observability.AttrResult, observability.ResultOK),
		attribute.Int(observability.AttrSearchResults, len(searchResults)),
	}

	observability.RecordMemorySearchResults(ctx, len(searchResults))

	if len(searchResults) > 0 {
		observability.RecordMemorySearchTopScore(ctx, float64(searchResults[0].Similarity))
		searchAttrs = append(searchAttrs,
			attribute.Float64(observability.AttrSearchTopScore, float64(searchResults[0].Similarity)))
	}

	span.SetAttributes(searchAttrs...)

	return searchResults, nil
}

// Write persists the entry to the FileStore, then indexes it in the
// vector database. If indexing fails, the error is logged but not
// returned — the entry is still saved.
func (s *IndexedStore) Write(ctx context.Context, id domain.InstanceID, entry Entry) error {
	ctx, span := s.tracer().Start(ctx, "memory.write")
	span.SetAttributes(
		attribute.String(observability.AttrOperation, "memory.write"),
		attribute.String(observability.AttrInstanceID, string(id)),
	)
	defer span.End()

	if err := s.backing.Write(ctx, id, entry); err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())

		return err
	}

	if err := s.index(ctx, id, entry); err != nil {
		slog.Default().WarnContext(ctx, "failed to index memory",
			"instance_id", string(id), "key", entry.Key, "error", err)
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	return nil
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
	ctx, span := s.tracer().Start(ctx, "memory.delete")
	span.SetAttributes(
		attribute.String(observability.AttrOperation, "memory.delete"),
		attribute.String(observability.AttrInstanceID, string(id)),
	)
	defer span.End()

	col, err := s.collection(id)
	if err != nil {
		slog.Default().WarnContext(ctx, "failed to get collection for delete",
			"instance_id", string(id), "key", key, "error", err)
	} else {
		if err := col.Delete(ctx, nil, nil, key); err != nil {
			slog.Default().WarnContext(ctx, "failed to remove memory from index",
				"instance_id", string(id), "key", key, "error", err)
		}
	}

	if err := s.backing.Delete(ctx, id, key); err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())

		return err
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	return nil
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
		ctx, span := s.tracer().Start(ctx, "memory.embed")
		span.SetAttributes(attribute.String(observability.AttrOperation, "memory.embed"))
		defer span.End()

		embedding, err := inner(ctx, text)
		if err != nil {
			span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
			span.SetStatus(codes.Error, err.Error())

			return nil, err
		}

		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

		return embedding, nil
	}
}
