package memory

import (
	"context"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/store"
)

// StoreAdapter implements the memory Store interface by delegating
// to a store.Store's memory methods.
type StoreAdapter struct {
	store store.Store
}

// NewStoreAdapter creates a memory store backed by the given data
// store.
func NewStoreAdapter(s store.Store) *StoreAdapter {
	return &StoreAdapter{store: s}
}

// Read retrieves all memories for a given model instance.
func (a *StoreAdapter) Read(ctx context.Context, nick domain.Nick) ([]Entry, error) {
	entries, err := a.store.ReadMemories(ctx, nick)
	if err != nil {
		return nil, err
	}

	result := make([]Entry, len(entries))
	for i, e := range entries {
		result[i] = Entry{Key: e.Key, Content: e.Content}
	}

	return result, nil
}

// Write stores a memory entry for a given model instance.
func (a *StoreAdapter) Write(ctx context.Context, nick domain.Nick, entry Entry) error {
	return a.store.WriteMemory(ctx, nick, entry.Key, entry.Content)
}

// Delete removes a specific memory entry by key.
func (a *StoreAdapter) Delete(ctx context.Context, nick domain.Nick, key string) error {
	return a.store.DeleteMemory(ctx, nick, key)
}

// Reset removes all memories. This delegates to ResetMemories on the
// store if available, otherwise it's a no-op.
func (a *StoreAdapter) Reset(ctx context.Context) error {
	if r, ok := a.store.(interface {
		ResetMemories(context.Context) error
	}); ok {
		return r.ResetMemories(ctx)
	}

	return nil
}
