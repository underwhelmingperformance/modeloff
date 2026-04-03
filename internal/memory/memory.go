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

// Store defines the interface for persisting model memories.
type Store interface {
	// Read retrieves all memories for a given model instance.
	Read(ctx context.Context, nick domain.Nick) ([]Entry, error)

	// Write stores a memory entry for a given model instance.
	Write(ctx context.Context, nick domain.Nick, entry Entry) error

	// Delete removes a specific memory entry.
	Delete(ctx context.Context, nick domain.Nick, key string) error
}
