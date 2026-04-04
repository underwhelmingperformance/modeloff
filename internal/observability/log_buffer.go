package observability

import "sync"

// LogBuffer stores a bounded in-memory log history for the UI.
type LogBuffer struct {
	mu       sync.RWMutex
	capacity int
	entries  []PanelEntry
	ingest   chan PanelEntry
	updates  chan struct{}
	done     chan struct{}
}

// NewLogBuffer creates a bounded log buffer and starts its drain loop.
func NewLogBuffer(capacity int) *LogBuffer {
	if capacity <= 0 {
		capacity = 1000
	}

	b := &LogBuffer{
		capacity: capacity,
		ingest:   make(chan PanelEntry, capacity),
		updates:  make(chan struct{}, 1),
		done:     make(chan struct{}),
	}

	go b.run()

	return b
}

func (b *LogBuffer) run() {
	defer close(b.done)

	for entry := range b.ingest {
		b.mu.Lock()
		if len(b.entries) >= b.capacity {
			copy(b.entries, b.entries[1:])
			b.entries[len(b.entries)-1] = entry
		} else {
			b.entries = append(b.entries, entry)
		}
		b.mu.Unlock()

		select {
		case b.updates <- struct{}{}:
		default:
		}
	}
}

// Ingest returns the channel the OTel log pipeline writes into.
func (b *LogBuffer) Ingest() chan<- PanelEntry {
	return b.ingest
}

// Updates returns a notification channel that fires when new logs arrive.
func (b *LogBuffer) Updates() <-chan struct{} {
	return b.updates
}

// Entries returns a stable snapshot of the buffered entries.
func (b *LogBuffer) Entries() []PanelEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()

	entries := make([]PanelEntry, len(b.entries))
	copy(entries, b.entries)

	return entries
}

// Close stops the drain loop.
func (b *LogBuffer) Close() {
	close(b.ingest)
	<-b.done
}
