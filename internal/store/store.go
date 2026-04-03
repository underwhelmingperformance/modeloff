// Package store provides persistence for rooms, messages, model
// instances, and application state. It is the single source of truth
// for all data that survives across sessions.
package store

import (
	"context"

	"github.com/laney/modeloff/internal/domain"
)

// Store defines the interface for all persistent data operations.
type Store interface {
	// Rooms

	ListRooms(ctx context.Context) ([]domain.Room, error)
	GetRoom(ctx context.Context, name domain.RoomName) (domain.Room, error)
	SaveRoom(ctx context.Context, room domain.Room) error
	DeleteRoom(ctx context.Context, name domain.RoomName) error

	// Messages

	ListMessages(ctx context.Context, room domain.RoomName) ([]domain.Message, error)
	SaveMessage(ctx context.Context, msg domain.Message) error

	// Model instances

	ListInstances(ctx context.Context) ([]domain.ModelInstance, error)
	GetInstance(ctx context.Context, nick domain.Nick) (domain.ModelInstance, error)
	SaveInstance(ctx context.Context, inst domain.ModelInstance) error
	DeleteInstance(ctx context.Context, nick domain.Nick) error

	// State

	GetLastRoom(ctx context.Context) (domain.RoomName, error)
	SetLastRoom(ctx context.Context, name domain.RoomName) error
}
