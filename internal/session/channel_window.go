package session

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/laney/modeloff/internal/domain"
)

// setUserMode records the user's mode for a channel. It is called
// from joinAs on a successful join and from SetMode when the user's
// mode changes. The mode is used by loadChannel/loadChannels to
// re-inject the user into channel member lists returned from the
// store.
func (s *Session) setUserMode(ctx context.Context, ch domain.ChannelName, mode domain.NickMode) {
	s.userMu.Lock()
	s.userModes[ch] = mode
	s.userMu.Unlock()

	slog.Default().DebugContext(ctx, "user mode changed",
		"component", "session",
		"channel", ch,
		"mode", mode.String(),
	)
}

// forgetUserMode drops the recorded mode for a channel when the
// user parts or is kicked.
func (s *Session) forgetUserMode(ctx context.Context, ch domain.ChannelName) {
	s.userMu.Lock()
	delete(s.userModes, ch)
	s.userMu.Unlock()

	slog.Default().DebugContext(ctx, "user mode cleared",
		"component", "session",
		"channel", ch,
	)
}

// userModeFor reads the recorded mode for a channel. The zero value
// (ModeNone) is returned when no mode has been recorded. Callers
// that ask about a channel the user isn't in get a debug-level log
// line as a diagnostic aid — the mode map is only meaningful for
// channels the user is currently in, but legitimate callers
// (assertions, tests) may probe non-member channels and ModeNone is
// the right answer for them.
func (s *Session) userModeFor(ctx context.Context, ch domain.ChannelName) domain.NickMode {
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
	user := s.userInstance()
	if user == nil {
		return false
	}

	channels := user.Channels()
	if channels == nil {
		return false
	}

	_, ok := channels.Get(ch)
	return ok
}

// loadChannelWindow reads an addressable `#`-channel as its typed
// `*ChannelWindow`, with the user re-injected as a member when
// the session records them as being in the channel. Returns
// `domain.ErrNotChannelWindow` if the row exists but is not a
// channel (status / DM) — channel-only callers rely on this as
// a typed guard.
func (s *Session) loadChannelWindow(ctx context.Context, name domain.ChannelName) (*domain.ChannelWindow, error) {
	w, err := s.store.GetWindow(ctx, name)
	if err != nil {
		return nil, err
	}

	cw, ok := w.(*domain.ChannelWindow)
	if !ok {
		return nil, fmt.Errorf("%w: kind %d for %q", domain.ErrNotChannelWindow, w.Kind(), name)
	}

	s.injectUserIfChannelMember(ctx, cw)

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

	if mode := s.userModeFor(ctx, cw.Name()); mode != domain.ModeNone {
		cw.Members.SetMode(user, mode)
	}
}

// persistChannelWindow saves a `*ChannelWindow` through the
// store's typed `SaveWindow` surface, with the user stripped from
// the member list — same contract as `persistChannel`. The user
// is an ephemeral session actor and is never persisted; the
// equivalent load path injects them back via
// `injectUserIfMember`.
func (s *Session) persistChannelWindow(ctx context.Context, w *domain.ChannelWindow) error {
	clone := *w
	clone.Members = cloneMembersWithout(w.Members, s.userInstance())
	return s.store.SaveWindow(ctx, &clone)
}

// commitChannel decides `window`'s fate after a membership
// mutation: persist the updated state, or delete the window
// outright when no occupants remain. RFC 2811 §2: "the channel
// ceases to exist when the last user leaves." Channel-mode state
// — including the `+i` invitation list — disappears with the
// row; a re-creation under the same name starts fresh.
func (s *Session) commitChannel(ctx context.Context, window *domain.ChannelWindow) error {
	if s.channelOccupied(window) {
		return s.persistChannelWindow(ctx, window)
	}

	return s.store.DeleteWindow(ctx, window.Name())
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
		s.forgetUserMode(ctx, ch)
	} else {
		if err := s.store.SaveInstance(ctx, actor); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}
	}

	return s.commitChannel(ctx, window)
}

// cloneMembersWithout returns a new MemberList containing every
// member of src except the one whose handle equals `excluded`.
// Modes are preserved.
func cloneMembersWithout(src domain.MemberList, excluded *domain.Instance) domain.MemberList {
	dst := domain.NewMemberList()
	for m := range src.All() {
		if m.Instance == excluded {
			continue
		}

		dst.Add(m.Instance)
		if m.Mode != domain.ModeNone {
			dst.SetMode(m.Instance, m.Mode)
		}
	}

	return dst
}
