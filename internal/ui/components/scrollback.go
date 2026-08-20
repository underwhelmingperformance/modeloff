package components

import (
	"sync/atomic"

	"github.com/laney/modeloff/internal/domain"
)

// ScrollbackLimit is how many events one window keeps in memory.
// Without a limit a window grows for as long as the session runs, and
// only `/clear` gives any of it back. The limit is well past a full
// screen at any terminal size, so the reader can still scroll back
// through a long conversation.
const ScrollbackLimit = 4000

// scrollbackInitialCap is how much room a window's history starts
// with. Most windows never fill a screen, and a ring sized for the
// limit up front would cost every one of them the memory only the
// busiest window needs.
const scrollbackInitialCap = 64

// scrollbackOriginStride is how far apart two scrollbacks start
// numbering. A rendered line is matched back to its event by the
// window it came from and that event's sequence number, so the
// scrollback a `/part` dropped and the one the rejoin builds must not
// number an event alike. Starting them this far apart is what keeps
// their numbers from meeting: a window would have to take four
// billion events in one session to reach the next scrollback's range.
const scrollbackOriginStride = 1 << 32

// scrollbackOrigin hands out the number each scrollback starts from.
var scrollbackOrigin atomic.Int64

// Scrollback is one window's in-memory event history, bounded to
// [ScrollbackLimit] events. Appending past the limit drops the oldest
// event.
//
// Every event gets a sequence number when it enters, and keeps it
// until it is dropped. The numbers run consecutively across the events
// the scrollback holds, so the caller can name a particular event
// without naming a position in a slice: dropping the oldest event
// shifts every position by one, and a reading mark or a cache entry
// recorded against a position would silently come to mean the event
// after the one it was recorded for.
//
// The events live in a ring the value holds a handle to, so hold a
// scrollback through a `*Scrollback` and let [NewScrollback] make it:
// a copy of the value would share the ring while keeping its own
// numbering, and the two would disagree about what the ring holds.
type Scrollback struct {
	ring *RingBuffer[domain.Event]

	// first is the sequence number of the oldest event held. It rises
	// as events are dropped, and never falls, so a sequence number is
	// never given to a second event.
	first int64

	// events memoises `ring.Slice()`. Callers walk the whole history
	// on every render and the ring cannot hand out its contents as one
	// slice, so the copy is taken once per change and read many times.
	events []domain.Event
}

// NewScrollback returns an empty history numbering from a point no
// other scrollback reaches.
func NewScrollback() *Scrollback {
	return &Scrollback{first: scrollbackOrigin.Add(scrollbackOriginStride)}
}

// Append adds an event to the end of the history, dropping the oldest
// when the scrollback is already at [ScrollbackLimit].
func (s *Scrollback) Append(event domain.Event) {
	s.makeRoom()

	if s.ring.Len() == s.ring.Cap() {
		s.first++
	}

	s.ring.Append(event)
	s.events = nil
}

// makeRoom readies the ring for one more event. It is built on the
// first append and doubles from there up to [ScrollbackLimit]. At the
// limit it stays as it is and the next append drops the oldest event.
func (s *Scrollback) makeRoom() {
	if s.ring == nil {
		s.ring = NewRingBuffer[domain.Event](scrollbackInitialCap)
		return
	}

	if s.ring.Len() < s.ring.Cap() || s.ring.Cap() >= ScrollbackLimit {
		return
	}

	s.ring.Resize(min(s.ring.Cap()*2, ScrollbackLimit))
}

// Prepend puts `held` in front of the events already there, for lines
// that were buffered before the window they belong to existed.
//
// Everything is renumbered from the next unused sequence number, so
// the held lines take numbers no event has had. A number below the
// oldest event's would have to be one an already-dropped event used,
// and a reader's mark or a cached line recorded against that event
// would then answer for one of these.
func (s *Scrollback) Prepend(held []domain.Event) {
	if len(held) == 0 {
		return
	}

	existing := s.Events()

	s.first = s.NextSeq()
	s.events = nil

	if s.ring != nil {
		s.ring.Clear()
	}

	for _, event := range held {
		s.Append(event)
	}

	for _, event := range existing {
		s.Append(event)
	}
}

// Clear drops every event. Sequence numbering carries on from where
// it was, so nothing appended afterwards can be mistaken for one of
// the events that has just gone.
func (s *Scrollback) Clear() {
	s.first = s.NextSeq()
	s.events = nil

	if s.ring != nil {
		s.ring.Clear()
	}
}

// Events returns the history in order, oldest first. The returned
// slice is the scrollback's own and is replaced, never written
// through, on the next change; callers must not modify it.
func (s *Scrollback) Events() []domain.Event {
	if s == nil || s.ring == nil {
		return nil
	}

	if s.events == nil {
		s.events = s.ring.Slice()
	}

	return s.events
}

// Len returns how many events the scrollback holds.
func (s *Scrollback) Len() int {
	if s == nil {
		return 0
	}

	return s.ring.Len()
}

// FirstSeq returns the sequence number of the oldest event held. For
// an empty scrollback it is the number the next event will take.
func (s *Scrollback) FirstSeq() int64 {
	if s == nil {
		return 0
	}

	return s.first
}

// NextSeq returns the sequence number the next event will take.
func (s *Scrollback) NextSeq() int64 {
	if s == nil {
		return 0
	}

	return s.first + int64(s.ring.Len())
}
