// Package store provides persistence for channels, messages, model
// instances, and application state. It is the single source of truth
// for all data that survives across sessions.
package store

import "errors"

// ErrNoSuchNick signals that a nick lookup did not match any
// stored instance.
var ErrNoSuchNick = errors.New("no such nick")

// ErrNoSuchChannel signals that a window lookup did not match any
// row in the channels table.
var ErrNoSuchChannel = errors.New("no such channel")

// MemoryEntry is a single memory stored by a model instance.
type MemoryEntry struct {
	Key     string
	Content string
}
