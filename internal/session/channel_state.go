package session

import (
	"context"
	"sync"

	"github.com/laney/modeloff/internal/domain"
)

// channelState holds the session's live channel records. A running
// IRC server keeps its channels in memory and answers every question
// about them from there, writing each mutation through to the store
// as the durable record.
//
// Only the command loop installs and drops records, so a record is
// never half-written. Readers arrive from any goroutine — the
// event-delivery filter, the send gates, a model assembling its
// prompt — and each is handed its own copy, so nothing they do can
// reach the record the next command will read.
//
// One mutex covers the whole map, and it is held across the store
// calls that fill and destroy an entry. That is deliberate and is
// the mechanism, not an accident of coarseness: a fill is the only
// way a name enters the map and a destroy is the only way one
// leaves, so a fill that could interleave with a destroy would read
// the row the destroy is about to delete and reinstate the channel
// the command loop had just destroyed. Both spans hold the lock, so
// neither can see the other half-done. The cost is that filling one
// cold channel briefly blocks readers of every channel.
//
// Entries arrive on demand, so the map holds the channels this
// session has touched. Answering a question about one named channel
// comes from here; enumerating them all
// ([Session.DirectoryChannels], [Session.ChannelWindowNames]) reads
// the store, which every commit writes through to. The two sources
// agree while the write-through succeeds. A failed `SaveWindow`
// leaves a channel that `GetWindow` can see and `/list` cannot, and
// a failed `DeleteWindow` leaves one that `/list` still enumerates
// and the poke scheduler still nudges; the persistence-failure
// counter is what reports that the two have parted company.
// The map is keyed by [domain.ChannelKey], the casemapped form of
// the name, so `#Dev` and `#dev` reach one record. Each record keeps
// the spelling it was created with under
// [domain.ChannelWindow.Name], and that is what goes on the wire.
type channelState struct {
	mu      sync.Mutex
	windows map[domain.ChannelKey]*domain.ChannelWindow
}

func newChannelState() *channelState {
	return &channelState{windows: make(map[domain.ChannelKey]*domain.ChannelWindow)}
}

// liveChannelWindow returns the live record for `name` as an
// independent copy, filling the entry from the store on first
// access. The error surface is the store's: [store.ErrNoSuchChannel]
// for an unknown name, [domain.ErrNotChannelWindow] for a row that
// exists but is a status or DM window.
func (s *Session) liveChannelWindow(ctx context.Context, name domain.ChannelName) (*domain.ChannelWindow, error) {
	s.channels.mu.Lock()
	defer s.channels.mu.Unlock()

	if cw, ok := s.channels.windows[domain.KeyForChannel(name)]; ok {
		return cw.Clone(), nil
	}

	cw, err := s.loadChannelWindowFromStore(ctx, name)
	if err != nil {
		return nil, err
	}

	s.channels.windows[domain.KeyForChannel(cw.Name())] = cw

	return cw.Clone(), nil
}

// installChannelWindow makes `w` the live record for its name. The
// stored copy is independent of `w`, so a caller that keeps mutating
// its own handle after committing does not edit live state.
func (s *Session) installChannelWindow(w *domain.ChannelWindow) {
	s.channels.mu.Lock()
	defer s.channels.mu.Unlock()

	s.channels.windows[domain.KeyForChannel(w.Name())] = w.Clone()
}

// destroyChannel ends a channel (RFC 2811 §2): the live record and
// the persisted row go together, under the lock, so a concurrent
// read cannot slip between them and refill the map from a row that
// is about to disappear.
func (s *Session) destroyChannel(ctx context.Context, name domain.ChannelName) error {
	s.channels.mu.Lock()
	defer s.channels.mu.Unlock()

	delete(s.channels.windows, domain.KeyForChannel(name))
	s.channelFlood.forget(name)

	return s.store.DeleteWindow(ctx, name)
}

// channelModes returns the live mode set for `name`, and whether the
// channel exists. Event delivery consults this for every message it
// fans out, so it answers from the live record and copies nothing
// but the plain mode struct.
func (s *Session) channelModes(ctx context.Context, name domain.ChannelName) (domain.ChannelModes, bool) {
	s.channels.mu.Lock()
	defer s.channels.mu.Unlock()

	cw, ok := s.channels.windows[domain.KeyForChannel(name)]
	if !ok {
		var err error

		cw, err = s.loadChannelWindowFromStore(ctx, name)
		if err != nil {
			return domain.ChannelModes{}, false
		}

		s.channels.windows[domain.KeyForChannel(cw.Name())] = cw
	}

	return cw.Modes, true
}
