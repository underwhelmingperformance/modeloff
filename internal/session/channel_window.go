package session

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/laney/modeloff/internal/domain"
)

// setUserModes records the privileges the user holds in a channel.
// The user is never persisted, so loadChannel/loadChannels read this
// record to re-inject the user into member lists loaded from the
// store.
func (s *Session) setUserModes(ctx context.Context, ch domain.ChannelName, modes domain.MemberModes) {
	s.userMu.Lock()
	s.userModes[ch] = modes
	s.userMu.Unlock()

	slog.Default().DebugContext(ctx, "user mode changed",
		"component", "session",
		"channel", ch,
		"mode", modes.IRCString(),
	)
}

// forgetUserModes drops the recorded privileges for a channel when
// the user parts or is kicked.
func (s *Session) forgetUserModes(ctx context.Context, ch domain.ChannelName) {
	s.userMu.Lock()
	delete(s.userModes, ch)
	s.userMu.Unlock()

	slog.Default().DebugContext(ctx, "user mode cleared",
		"component", "session",
		"channel", ch,
	)
}

// userModesFor reads the recorded privileges for a channel. It
// returns the zero value (no privileges) when nothing has been
// recorded. Callers that ask about a channel the user isn't in get a
// debug-level log line as a diagnostic aid: the record is only
// meaningful for channels the user is currently in, but legitimate
// callers (assertions, tests) may probe non-member channels, and the
// zero value is the right answer for them.
func (s *Session) userModesFor(ctx context.Context, ch domain.ChannelName) domain.MemberModes {
	if !s.userInChannel(ch) {
		slog.Default().DebugContext(ctx, "user mode requested for channel user is not in",
			"component", "session",
			"channel", ch,
		)
	}

	s.userMu.Lock()
	defer s.userMu.Unlock()

	return s.userModes[ch]
}

// userInChannel reports whether the user's in-memory Channels map
// lists the given channel. The map is authoritative for session-
// ephemeral membership: the user is never saved to the store, so
// channels loaded from disk rely on this to know whether to
// re-inject the user.
func (s *Session) userInChannel(ch domain.ChannelName) bool {
	return s.userInstance().InChannel(ch)
}

// loadChannelWindow reads an addressable `#`-channel as its typed
// `*ChannelWindow`, with the user re-injected as a member when
// the session records them as being in the channel. The record
// comes from the session's live channel state; the caller owns the
// returned copy and may mutate it freely before committing it back
// through [Session.persistChannelWindow]. Returns
// `domain.ErrNotChannelWindow` if the row exists but is not a
// channel (status / DM) — channel-only callers rely on this as
// a typed guard.
func (s *Session) loadChannelWindow(ctx context.Context, name domain.ChannelName) (*domain.ChannelWindow, error) {
	cw, err := s.liveChannelWindow(ctx, name)
	if err != nil {
		return nil, err
	}

	s.injectUserIfChannelMember(ctx, cw)

	return cw, nil
}

// loadChannelWindowFromStore reads the persisted row for `name` and
// asserts it is a channel. This is the cold path behind the live
// channel state and the only place channel records enter it.
func (s *Session) loadChannelWindowFromStore(ctx context.Context, name domain.ChannelName) (*domain.ChannelWindow, error) {
	w, err := s.store.GetWindow(ctx, name)
	if err != nil {
		return nil, err
	}

	cw, ok := w.(*domain.ChannelWindow)
	if !ok {
		return nil, fmt.Errorf("%w: kind %d for %q", domain.ErrNotChannelWindow, w.Kind(), name)
	}

	return cw, nil
}

// injectUserIfChannelMember adds the user to a `*ChannelWindow`'s
// member list when the session records them as in that channel.
// The user is an ephemeral session actor and is never persisted;
// `persistChannelWindow` strips them on save and this helper
// adds them back on load.
func (s *Session) injectUserIfChannelMember(ctx context.Context, cw *domain.ChannelWindow) {
	user := s.userInstance()
	if user == nil {
		return
	}

	if !s.userInChannel(cw.Name()) {
		return
	}

	if cw.Members.HasInstance(user) {
		return
	}

	cw.Members.Add(user)
	cw.Members.SetModes(user, s.userModesFor(ctx, cw.Name()))
}

// persistChannelWindow commits a `*ChannelWindow` as the session's
// live record for that channel and writes it through to the store,
// with the user stripped from the member list. The user is an
// ephemeral session actor and is never persisted; the load path
// injects them back via `injectUserIfChannelMember`.
//
// Live state is updated first so the next reader sees the committed
// record even if the durable write fails. A failed write leaves the
// store behind live state, and counts against the
// persistence-failure metric so operators see the two diverge.
func (s *Session) persistChannelWindow(ctx context.Context, w *domain.ChannelWindow) error {
	clone := *w
	clone.Members = cloneMembersWithout(w.Members, s.userInstance())

	s.installChannelWindow(&clone)

	if err := s.store.SaveWindow(ctx, &clone); err != nil {
		s.recordPersistenceFailure(ctx, w.Name())

		return err
	}

	return nil
}

// commitChannel decides `window`'s fate after a membership
// mutation: persist the updated state, or destroy the channel
// outright when no occupants remain. RFC 2811 §2: "the channel
// ceases to exist when the last user leaves." Channel-mode state
// — including the `+i` invitation list — disappears with the
// record; a re-creation under the same name starts fresh.
func (s *Session) commitChannel(ctx context.Context, window *domain.ChannelWindow) error {
	if s.channelOccupied(window) {
		return s.persistChannelWindow(ctx, window)
	}

	if err := s.destroyChannel(ctx, window.Name()); err != nil {
		s.recordPersistenceFailure(ctx, window.Name())

		return err
	}

	return nil
}

// channelOccupied reports whether `window` still has any
// occupants after the most recent membership mutation. A model
// occupant lives in `window.Members` with a non-empty
// `InstanceID`; the user is tracked separately via the
// session's user-instance channels map and is checked through
// `userInChannel` (the persisted member list never contains the
// user, and any in-memory injection has already been undone by
// the caller).
func (s *Session) channelOccupied(window *domain.ChannelWindow) bool {
	for member := range window.Members.All() {
		if member.Instance.ID() != "" {
			return true
		}
	}

	return s.userInChannel(window.Name())
}

// removeMember is the single membership-decrement primitive
// shared by every action that drops an actor from a channel
// (PART, KICK, model QUIT). It mutates `window.Members`, keeps
// actor-side state in sync (the channel is dropped from
// `actor.Channels()`; the instance row is saved for a model
// actor; the user-mode map is cleared for the user actor), and
// commits the window — persisting the updated state or deleting
// the row when the channel is now empty (RFC 2811 §2).
//
// Callers own the broadcast event that announces the departure
// (PART, KICK, QUIT) and any caller-specific bookkeeping
// (autojoin-list refresh, instance-row deletion on model QUIT).
func (s *Session) removeMember(ctx context.Context, window *domain.ChannelWindow, actor *domain.Instance) error {
	ch := window.Name()

	if m, ok := window.Members.GetByInstance(actor); ok {
		window.Members.Remove(m)
	}

	actor.MutateChannels(func(m *orderedmap.OrderedMap[domain.ChannelName, time.Time]) {
		m.Delete(ch)
	})

	if actor.ID() == "" {
		s.forgetUserModes(ctx, ch)
	} else {
		if err := s.store.SaveInstance(ctx, actor); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}
	}

	return s.commitChannel(ctx, window)
}

// cloneMembersWithout returns a new MemberList containing every
// member of src except the one whose handle equals `excluded`.
// Privileges are preserved.
func cloneMembersWithout(src domain.MemberList, excluded *domain.Instance) domain.MemberList {
	dst := domain.NewMemberList()
	for m := range src.All() {
		if m.Instance == excluded {
			continue
		}

		dst.Add(m.Instance)
		dst.SetModes(m.Instance, m.Modes)
	}

	return dst
}
