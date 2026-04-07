// Package store provides persistence for channels, messages, model
// instances, and application state. It is the single source of truth
// for all data that survives across sessions.
package store

import (
	"context"

	"github.com/laney/modeloff/internal/domain"
)

// Store defines the interface for all persistent data operations.
type Store interface {
	// Channels

	ListChannels(ctx context.Context) ([]domain.Channel, error)
	GetChannel(ctx context.Context, name domain.ChannelName) (domain.Channel, error)
	SaveChannel(ctx context.Context, ch domain.Channel) error
	DeleteChannel(ctx context.Context, name domain.ChannelName) error

	// Event log

	AppendEvent(ctx context.Context, ch domain.ChannelName, event domain.ChannelEvent) (int64, error)
	EventsBefore(ctx context.Context, ch domain.ChannelName, before *int64, n int) ([]domain.StoredEvent, error)
	EventsFrom(ctx context.Context, ch domain.ChannelName, from *int64, n int) ([]domain.StoredEvent, error)

	// Model instances

	ListInstances(ctx context.Context) ([]domain.ModelInstance, error)
	GetInstance(ctx context.Context, nick domain.Nick) (domain.ModelInstance, error)
	SaveInstance(ctx context.Context, inst domain.ModelInstance) error
	DeleteInstance(ctx context.Context, nick domain.Nick) error

	// State

	GetLastChannel(ctx context.Context) (domain.ChannelName, error)
	SetLastChannel(ctx context.Context, name domain.ChannelName) error

	// Last-read tracking

	GetLastRead(ctx context.Context, ch domain.ChannelName) (int64, error)
	SetLastRead(ctx context.Context, ch domain.ChannelName, eventID int64) error

	// Memories

	ReadMemories(ctx context.Context, nick domain.Nick) ([]MemoryEntry, error)
	WriteMemory(ctx context.Context, nick domain.Nick, key, content string) error
	DeleteMemory(ctx context.Context, nick domain.Nick, key string) error

	// Reset

	Reset(ctx context.Context) error
}

// MemoryEntry is a single memory stored by a model instance.
type MemoryEntry struct {
	Key     string
	Content string
}
