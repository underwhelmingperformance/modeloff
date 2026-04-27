// Package store provides persistence for channels, messages, model
// instances, and application state. It is the single source of truth
// for all data that survives across sessions.
package store

import (
	"context"
	"errors"

	"github.com/laney/modeloff/internal/domain"
)

// ErrNoSuchNick is returned by `Store.ResolveNick` when the given
// nick does not map to any stored instance. Callers detect missing
// nicks with `errors.Is(err, store.ErrNoSuchNick)`.
var ErrNoSuchNick = errors.New("no such nick")

// ErrNoSuchChannel is returned by `Store.GetWindow` when the lookup
// name does not match any row in the channels table. Callers detect
// missing addressable windows with `errors.Is(err,
// store.ErrNoSuchChannel)`.
var ErrNoSuchChannel = errors.New("no such channel")

// Store defines the interface for all persistent data operations.
type Store interface {
	// Windows
	//
	// Addressable-by-name windows live in the `channels` table.
	// Loads return the typed concrete `Window` (`*StatusWindow` /
	// `*ChannelWindow` / `*DMWindow`) so callers can downcast where
	// per-kind state matters. DM windows resolve their counterpart
	// `*Instance` through the store's instance registry; a DM whose
	// counterpart row has been deleted is dropped at load time and
	// logged.

	ListWindows(ctx context.Context) ([]domain.Window, error)
	GetWindow(ctx context.Context, name domain.ChannelName) (domain.Window, error)
	SaveWindow(ctx context.Context, w domain.Window) error
	DeleteWindow(ctx context.Context, name domain.ChannelName) error

	// Event log

	AppendEvent(ctx context.Context, ch domain.ChannelName, event domain.PersistableEvent) (int64, error)
	EventsBefore(ctx context.Context, ch domain.ChannelName, before *int64, n int) ([]domain.StoredEvent, error)
	EventsFrom(ctx context.Context, ch domain.ChannelName, from *int64, n int) ([]domain.StoredEvent, error)

	// Model instances.
	//
	// The store is the sole authority for `*Instance` pointer
	// identity: callers receive the same `*Instance` pointer for a
	// given InstanceID on every load. `GetWindow` returns a
	// `*ChannelWindow` whose member list already carries canonical
	// pointers — callers never resolve ids themselves.

	ListInstances(ctx context.Context) ([]*domain.Instance, error)
	GetInstanceByID(ctx context.Context, id domain.InstanceID) (*domain.Instance, error)
	SaveInstance(ctx context.Context, inst *domain.Instance) error
	DeleteInstanceByID(ctx context.Context, id domain.InstanceID) error

	// ResolveNick returns the canonical `*Instance` handle whose
	// current display nick matches the argument. This is the single
	// boundary where nick-in-hand callers (the command parser) turn
	// user input into an identity handle.
	ResolveNick(ctx context.Context, nick domain.Nick) (*domain.Instance, error)

	// State

	GetLastChannel(ctx context.Context) (domain.ChannelName, error)
	SetLastChannel(ctx context.Context, name domain.ChannelName) error

	// Session-active marker

	GetSessionActive(ctx context.Context) (string, error)
	SetSessionActive(ctx context.Context, value string) error
	ClearSessionActive(ctx context.Context) error

	// Last-read tracking

	GetLastRead(ctx context.Context, ch domain.ChannelName) (int64, error)
	SetLastRead(ctx context.Context, ch domain.ChannelName, eventID int64) error

	// Memories

	ReadMemories(ctx context.Context, id domain.InstanceID) ([]MemoryEntry, error)
	WriteMemory(ctx context.Context, id domain.InstanceID, key, content string) error
	DeleteMemory(ctx context.Context, id domain.InstanceID, key string) error

	// Personas

	ListPersonas(ctx context.Context) ([]domain.Persona, error)
	GetPersona(ctx context.Context, id string) (domain.Persona, error)
	SavePersona(ctx context.Context, p domain.Persona) error
	DeletePersonasByOrigin(ctx context.Context, origin domain.PersonaOrigin) error
	ReplaceGeneratedPersonas(ctx context.Context, personas []domain.Persona) error

	// Autojoin

	ListAutojoinChannels(ctx context.Context) ([]domain.ChannelName, error)
	SetAutojoinChannels(ctx context.Context, channels []domain.ChannelName) error

	// Reset

	Reset(ctx context.Context) error
}

// MemoryEntry is a single memory stored by a model instance.
type MemoryEntry struct {
	Key     string
	Content string
}
