// Package memory provides a per-model-instance memory system. Each
// instance (keyed by nick) can store and retrieve memories, which are
// exposed as tools to the model so it can decide when to read and
// write them.
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

// Store defines the interface for persisting model memories.
type Store interface {
	// Read retrieves all memories for a given model instance.
	Read(ctx context.Context, nick domain.Nick) ([]Entry, error)

	// Write stores a memory entry for a given model instance.
	Write(ctx context.Context, nick domain.Nick, entry Entry) error

	// Delete removes a specific memory entry.
	Delete(ctx context.Context, nick domain.Nick, key string) error

	// Reset removes all memories for all instances.
	Reset(ctx context.Context) error
}

// Searcher is an optional interface that a Store can implement to
// provide semantic search over memories. Callers should check for
// this with a type assertion before offering search_memory as a tool.
type Searcher interface {
	Search(ctx context.Context, nick domain.Nick, query string, limit int) ([]SearchResult, error)
}
