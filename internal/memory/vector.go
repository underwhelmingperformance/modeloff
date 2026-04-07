package memory

import (
	"context"
	"fmt"
	"log/slog"

	chromem "github.com/philippgille/chromem-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
)

var (
	_ Store    = (*IndexedStore)(nil)
	_ Searcher = (*IndexedStore)(nil)

	memoryTracer = otel.Tracer("github.com/laney/modeloff/internal/memory")
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
}

// NewIndexedStore creates an IndexedStore backed by the given memory
// Store and a persistent chromem-go database at indexDir.
func NewIndexedStore(backing Store, indexDir string, embeddingFunc chromem.EmbeddingFunc) (*IndexedStore, error) {
	db, err := chromem.NewPersistentDB(indexDir, false)
	if err != nil {
		return nil, fmt.Errorf("open vector index: %w", err)
	}

	return &IndexedStore{
		backing:       backing,
		db:            db,
		embeddingFunc: instrumentEmbedding(embeddingFunc),
	}, nil
}

// NewIndexedStoreFromDB creates an IndexedStore from an existing
// chromem-go DB. This allows callers to provide an in-memory database
// for testing while using a persistent one in production.
func NewIndexedStoreFromDB(backing Store, db *chromem.DB, embeddingFunc chromem.EmbeddingFunc) *IndexedStore {
	return &IndexedStore{
		backing:       backing,
		db:            db,
		embeddingFunc: instrumentEmbedding(embeddingFunc),
	}
}

func (s *IndexedStore) collection(nick domain.Nick) (*chromem.Collection, error) {
	return s.db.GetOrCreateCollection(string(nick), nil, s.embeddingFunc)
}

// Read delegates to the underlying FileStore.
func (s *IndexedStore) Read(ctx context.Context, nick domain.Nick) ([]Entry, error) {
	return s.backing.Read(ctx, nick)
}

// Search finds memories semantically similar to the query, returning
// up to limit results ordered by descending similarity. On the first
// call for a nick, the index is rebuilt from the FileStore if needed.
func (s *IndexedStore) Search(ctx context.Context, nick domain.Nick, query string, limit int) ([]SearchResult, error) {
	ctx, span := memoryTracer.Start(ctx, "memory.search")
	span.SetAttributes(
		attribute.String(observability.AttrOperation, "memory.search"),
		attribute.String(observability.AttrMemoryNick, string(nick)),
	)
	defer span.End()

	if err := s.ensureIndexed(ctx, nick); err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())

		return nil, fmt.Errorf("ensure indexed: %w", err)
	}

	col, err := s.collection(nick)
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
func (s *IndexedStore) Write(ctx context.Context, nick domain.Nick, entry Entry) error {
	ctx, span := memoryTracer.Start(ctx, "memory.write")
	span.SetAttributes(
		attribute.String(observability.AttrOperation, "memory.write"),
		attribute.String(observability.AttrMemoryNick, string(nick)),
	)
	defer span.End()

	if err := s.backing.Write(ctx, nick, entry); err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())

		return err
	}

	if err := s.index(ctx, nick, entry); err != nil {
		slog.Default().WarnContext(ctx, "failed to index memory",
			"nick", nick, "key", entry.Key, "error", err)
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	return nil
}

func (s *IndexedStore) index(ctx context.Context, nick domain.Nick, entry Entry) error {
	col, err := s.collection(nick)
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
func (s *IndexedStore) Delete(ctx context.Context, nick domain.Nick, key string) error {
	ctx, span := memoryTracer.Start(ctx, "memory.delete")
	span.SetAttributes(
		attribute.String(observability.AttrOperation, "memory.delete"),
		attribute.String(observability.AttrMemoryNick, string(nick)),
	)
	defer span.End()

	col, err := s.collection(nick)
	if err != nil {
		slog.Default().WarnContext(ctx, "failed to get collection for delete",
			"nick", nick, "key", key, "error", err)
	} else {
		if err := col.Delete(ctx, nil, nil, key); err != nil {
			slog.Default().WarnContext(ctx, "failed to remove memory from index",
				"nick", nick, "key", key, "error", err)
		}
	}

	if err := s.backing.Delete(ctx, nick, key); err != nil {
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

// reindexNick reads all entries for a nick from the FileStore and
// indexes them. This handles migration from a plain FileStore and
// recovery from a corrupted index.
func (s *IndexedStore) reindexNick(ctx context.Context, nick domain.Nick) error {
	entries, err := s.backing.Read(ctx, nick)
	if err != nil {
		return fmt.Errorf("read entries for %s: %w", nick, err)
	}

	for _, entry := range entries {
		if err := s.index(ctx, nick, entry); err != nil {
			return fmt.Errorf("index entry %s/%s: %w", nick, entry.Key, err)
		}
	}

	return nil
}

// ensureIndexed checks whether a nick's collection is empty and, if
// the FileStore has entries, rebuilds the index. This is called
// lazily on Search so callers never need to think about reindexing.
func (s *IndexedStore) ensureIndexed(ctx context.Context, nick domain.Nick) error {
	col, err := s.collection(nick)
	if err != nil {
		return fmt.Errorf("get collection for %s: %w", nick, err)
	}

	if col.Count() > 0 {
		return nil
	}

	entries, err := s.backing.Read(ctx, nick)
	if err != nil {
		return fmt.Errorf("read entries for %s: %w", nick, err)
	}

	if len(entries) == 0 {
		return nil
	}

	slog.Default().InfoContext(ctx, "rebuilding memory index", "nick", nick)

	return s.reindexNick(ctx, nick)
}

func instrumentEmbedding(inner chromem.EmbeddingFunc) chromem.EmbeddingFunc {
	return func(ctx context.Context, text string) ([]float32, error) {
		ctx, span := memoryTracer.Start(ctx, "memory.embed")
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
