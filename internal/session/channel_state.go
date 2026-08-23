package session

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/store"
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
// calls that fill, reload and destroy an entry. A fill that could
// interleave with a destroy would read the row the destroy is about
// to delete and reinstate the channel the command loop had just
// destroyed. Holding the lock across these operations prevents that
// half-written state. The cost is that filling one cold channel
// briefly blocks readers of every channel.
//
// Entries arrive on demand, so the map holds the channels this
// session has touched. Answering a question about one named channel
// comes from here. Directory reads start with the stored rows, then
// overlay these live records and omit rows for channels destroyed in
// this session. A failed write can leave durable state behind, but it
// cannot make `/list` or the poke scheduler contradict live state.
// The persistence-failure counter reports the durable divergence.
// The map is keyed by [domain.ChannelKey], the casemapped form of
// the name, so `#Dev` and `#dev` reach one record. Each record keeps
// the spelling it was created with under
// [domain.ChannelWindow.Name], and that is what goes on the wire.
type channelState struct {
	mu                    sync.Mutex
	windows               map[domain.ChannelKey]*domain.ChannelWindow
	generations           map[domain.ChannelKey]uint64
	invitationGenerations map[invitationKey]uint64
}

type invitationKey struct {
	channel domain.ChannelKey
	actor   domain.InstanceID
}

type invitationGeneration struct {
	channel uint64
	invite  uint64
}

type invitationAuthority uint8

const (
	invitationAuthorityNone invitationAuthority = iota
	invitationAuthorityPending
	invitationAuthorityMember
)

func newChannelState() *channelState {
	return &channelState{
		windows:               make(map[domain.ChannelKey]*domain.ChannelWindow),
		generations:           make(map[domain.ChannelKey]uint64),
		invitationGenerations: make(map[invitationKey]uint64),
	}
}

// liveChannelWindow returns the live record for `name` as an
// independent copy, filling the entry from the store on first
// access. The error surface is the store's: [store.ErrNoSuchChannel]
// for an unknown name, [domain.ErrNotChannelWindow] for a row that
// exists but is a status or DM window.
func (s *Session) liveChannelWindow(ctx context.Context, name domain.ChannelName) (*domain.ChannelWindow, error) {
	s.channels.mu.Lock()
	defer s.channels.mu.Unlock()

	cw, err := s.liveChannelWindowLocked(ctx, name)
	if err != nil {
		return nil, err
	}

	return cw.Clone(), nil
}

func (s *Session) liveChannelWindowLocked(
	ctx context.Context,
	name domain.ChannelName,
) (*domain.ChannelWindow, error) {
	key := domain.KeyForChannel(name)
	if cw := s.channels.windows[key]; cw != nil {
		return cw, nil
	}
	// A generation without a live row is the tombstone left by
	// destroyChannel. The durable delete may have failed, so that row
	// cannot repopulate live state during this session.
	if s.channels.generations[key] > 0 {
		return nil, store.ErrNoSuchChannel
	}

	cw, err := s.loadChannelWindowFromStore(ctx, name)
	if err != nil {
		return nil, err
	}

	s.channels.windows[domain.KeyForChannel(cw.Name())] = cw

	return cw, nil
}

// installChannelWindow makes `w` the live record for its name. The
// stored copy is independent of `w`, so a caller that keeps mutating
// its own handle after committing does not edit live state.
func (s *Session) installChannelWindow(w *domain.ChannelWindow) {
	s.channels.mu.Lock()
	defer s.channels.mu.Unlock()

	s.channels.windows[domain.KeyForChannel(w.Name())] = w.Clone()
	s.directoryGeneration.Add(1)
}

func (s *Session) installChannelWindowWithInvitationChange(
	w *domain.ChannelWindow,
	actor domain.InstanceID,
) {
	s.channels.mu.Lock()
	defer s.channels.mu.Unlock()

	channel := domain.KeyForChannel(w.Name())
	s.channels.windows[channel] = w.Clone()
	s.channels.invitationGenerations[invitationKey{channel: channel, actor: actor}]++
	s.directoryGeneration.Add(1)
}

func (s *Session) removeLiveChannel(name domain.ChannelName) {
	s.channels.mu.Lock()
	defer s.channels.mu.Unlock()

	key := domain.KeyForChannel(name)
	delete(s.channels.windows, key)
	s.channels.generations[key]++
	s.directoryGeneration.Add(1)
	s.channelFlood.forget(name)
}

// destroyChannel ends a channel (RFC 2811 §2): the live record and
// the persisted row go together, under the lock, so a concurrent
// read cannot slip between them and refill the map from a row that
// is about to disappear.
func (s *Session) destroyChannel(ctx context.Context, name domain.ChannelName) error {
	s.channels.mu.Lock()
	defer s.channels.mu.Unlock()

	key := domain.KeyForChannel(name)
	delete(s.channels.windows, key)
	s.channels.generations[key]++
	s.directoryGeneration.Add(1)
	s.channelFlood.forget(name)

	return s.store.DeleteWindow(ctx, name)
}

func (s *Session) invitationState(
	ctx context.Context,
	name domain.ChannelName,
	actor domain.InstanceID,
) (*domain.ChannelWindow, invitationGeneration, bool) {
	s.channels.mu.Lock()
	defer s.channels.mu.Unlock()

	key := domain.KeyForChannel(name)
	inviteKey := invitationKey{channel: key, actor: actor}
	generation := invitationGeneration{
		channel: s.channels.generations[key],
		invite:  s.channels.invitationGenerations[inviteKey],
	}
	window, err := s.liveChannelWindowLocked(ctx, name)
	if err != nil {
		return nil, generation, false
	}

	return window.Clone(), generation, window.Invitations.Contains(actor)
}

func (s *Session) invitationGuardState(
	ctx context.Context,
	guard invitationGuard,
	windowEpoch uint64,
) (*domain.ChannelWindow, invitationAuthority, error) {
	s.channels.mu.Lock()
	defer s.channels.mu.Unlock()

	return s.invitationGuardStateLocked(ctx, guard, windowEpoch)
}

func (s *Session) invitationGuardStateLocked(
	ctx context.Context,
	guard invitationGuard,
	windowEpoch uint64,
) (*domain.ChannelWindow, invitationAuthority, error) {
	key := domain.KeyForChannel(guard.channel)
	actor := guard.client.instance.ID()
	generation := invitationGeneration{
		channel: s.channels.generations[key],
		invite: s.channels.invitationGenerations[invitationKey{
			channel: key,
			actor:   actor,
		}],
	}
	if generation.channel != guard.generation.channel {
		return nil, invitationAuthorityNone, nil
	}

	pending := generation.invite == guard.generation.invite &&
		windowEpoch == guard.windowEpoch
	member := generation.invite == guard.generation.invite+1 &&
		windowEpoch == guard.windowEpoch+1
	if !pending && !member {
		return nil, invitationAuthorityNone, nil
	}

	window, err := s.liveChannelWindowLocked(ctx, guard.channel)
	if err != nil {
		return nil, invitationAuthorityNone, err
	}
	if pending && window.Invitations.Contains(actor) {
		return window.Clone(), invitationAuthorityPending, nil
	}
	if member && !window.Invitations.Contains(actor) &&
		window.Members.HasInstance(guard.client.instance) &&
		guard.client.instance.InChannel(window.Name()) {
		return window.Clone(), invitationAuthorityMember, nil
	}

	return nil, invitationAuthorityNone, nil
}

// channelModes returns the live mode set for `name`, and whether the
// channel exists. Event delivery consults this for every message it
// fans out, so it answers from the live record and copies nothing
// but the plain mode struct.
func (s *Session) channelModes(ctx context.Context, name domain.ChannelName) (domain.ChannelModes, bool) {
	s.channels.mu.Lock()
	defer s.channels.mu.Unlock()

	cw, err := s.liveChannelWindowLocked(ctx, name)
	if err != nil {
		return domain.ChannelModes{}, false
	}

	return cw.Modes, true
}

func (s *Session) directoryChannelWindows(ctx context.Context) ([]*domain.ChannelWindow, error) {
	stored, err := s.store.ListWindows(ctx)
	if err != nil {
		return nil, err
	}

	s.channels.mu.Lock()
	defer s.channels.mu.Unlock()

	windows := make([]*domain.ChannelWindow, 0, len(stored)+len(s.channels.windows))
	seen := make(map[domain.ChannelKey]struct{}, len(stored))
	for _, window := range stored {
		channel, ok := window.(*domain.ChannelWindow)
		if !ok {
			continue
		}

		key := domain.KeyForChannel(channel.Name())
		seen[key] = struct{}{}
		if live, exists := s.channels.windows[key]; exists {
			windows = append(windows, live.Clone())
			continue
		}
		if s.channels.generations[key] > 0 {
			continue
		}

		windows = append(windows, channel.Clone())
	}

	var liveOnly []*domain.ChannelWindow
	for key, window := range s.channels.windows {
		if _, exists := seen[key]; exists {
			continue
		}
		liveOnly = append(liveOnly, window.Clone())
	}
	slices.SortFunc(liveOnly, func(a, b *domain.ChannelWindow) int {
		return strings.Compare(string(a.Name()), string(b.Name()))
	})

	return append(windows, liveOnly...), nil
}
