// Package memory provides a per-model-instance memory system. Each
// instance (keyed by InstanceID) can store and retrieve memories,
// which are exposed as tools to the model so it can decide when to
// read and write them. Keying by stable identity means a `/nick`
// rename does not orphan an instance's memories.
package memory

import (
	"context"

	"github.com/laney/modeloff/internal/domain"
)

// Entry represents a single memory entry stored by a model instance.
type Entry struct {
	Key     string `json:"key"`
	Content string `json:"content"`
}

// SearchResult pairs a memory entry with its similarity score from a
// vector search.
type SearchResult struct {
	Entry      Entry
	Similarity float32
}

// Store defines the interface for persisting model memories. Each
// store is keyed by stable identity (InstanceID), not by display
// nick, so a `/nick` rename by the model does not orphan its
// memories.
type Store interface {
	// Read retrieves all memories for a given model instance.
	Read(ctx context.Context, id domain.InstanceID) ([]Entry, error)

	// Write stores a memory entry for a given model instance.
	Write(ctx context.Context, id domain.InstanceID, entry Entry) error

	// Delete removes a specific memory entry.
	Delete(ctx context.Context, id domain.InstanceID, key string) error

	// Reset removes all memories for all instances.
	Reset(ctx context.Context) error
}

// Searcher is an optional interface that a Store can implement to
// provide semantic search over memories. Search is structurally
// present on any type that implements it regardless of whether its
// embedding endpoint actually works; Searchable reports the real,
// probed outcome. Callers should check both: the type assertion
// before offering search_memory as a tool at all, and Searchable
// before trusting that a Search call will succeed.
type Searcher interface {
	Search(ctx context.Context, id domain.InstanceID, query string, limit int) ([]SearchResult, error)

	// Searchable reports whether this store's embedding endpoint was
	// confirmed to work by a probe at construction time.
	Searchable() bool
}

// InstanceDeleter is an optional capability a memory Store can
// implement to remove state it owns for a single instance, keyed by
// identity. The FileStore-backed memories rows are already removed
// when the instance's own row is deleted (see
// [github.com/laney/modeloff/internal/store.SQLiteStore.DeleteInstanceByID]);
// this exists for state the memory layer owns independently of that
// row, such as an IndexedStore's chromem-go vector collection.
type InstanceDeleter interface {
	DeleteInstance(ctx context.Context, id domain.InstanceID) error
}
