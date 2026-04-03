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

	// Messages

	ListMessages(ctx context.Context, ch domain.ChannelName) ([]domain.Message, error)
	SaveMessage(ctx context.Context, msg domain.Message) error

	// Model instances

	ListInstances(ctx context.Context) ([]domain.ModelInstance, error)
	GetInstance(ctx context.Context, nick domain.Nick) (domain.ModelInstance, error)
	SaveInstance(ctx context.Context, inst domain.ModelInstance) error
	DeleteInstance(ctx context.Context, nick domain.Nick) error

	// State

	GetLastChannel(ctx context.Context) (domain.ChannelName, error)
	SetLastChannel(ctx context.Context, name domain.ChannelName) error

	// Last-read tracking

	GetLastRead(ctx context.Context, ch domain.ChannelName) (string, error)
	SetLastRead(ctx context.Context, ch domain.ChannelName, messageID string) error
}
