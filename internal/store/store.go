// Package store provides persistence for channels, messages, model
// instances, and application state. It is the single source of truth
// for all data that survives across sessions.
package store

import (
	"errors"
	"time"
)

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

	// At is when this entry was written, as recorded by
	// WriteMemory's caller. A row written before this field existed
	// carries the zero time: nothing recorded its original write
	// time, so it sorts as the oldest possible entry rather than
	// being backdated to a time it was not actually written.
	At time.Time
}
