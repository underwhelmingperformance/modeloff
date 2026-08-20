package session

import (
	"context"
	"fmt"

	"github.com/laney/modeloff/internal/domain"
)

// loadChannelWindow reads an addressable `#`-channel as its typed
// `*ChannelWindow`. The record comes from the session's live channel
// state; the caller owns the returned copy and may mutate it freely
// before committing it back through [Session.persistChannelWindow].
// Returns `domain.ErrNotChannelWindow` if the row exists but is not
// a channel (status or DM). Channel-only callers rely on that as a
// typed guard.
func (s *Session) loadChannelWindow(ctx context.Context, name domain.ChannelName) (*domain.ChannelWindow, error) {
	return s.liveChannelWindow(ctx, name)
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

// persistChannelWindow commits a `*ChannelWindow` as the session's
// live record for that channel and writes it through to the store.
//
// Live state is updated first so the next reader sees the committed
// record even if the durable write fails. A failed write leaves the
// store behind live state, and counts against the
// persistence-failure metric so operators see the two diverge.
func (s *Session) persistChannelWindow(ctx context.Context, w *domain.ChannelWindow) error {
	clone := *w
	clone.Members = w.Members.Clone()

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
	if window.Members.Len() > 0 {
		return s.persistChannelWindow(ctx, window)
	}

	if err := s.destroyChannel(ctx, window.Name()); err != nil {
		s.recordPersistenceFailure(ctx, window.Name())

		return err
	}

	return nil
}

// removeMember is the single membership-decrement primitive
// shared by every action that drops an actor from a channel
// (PART, KICK, QUIT). It mutates `window.Members`, drops the
// channel from `actor.Channels()`, writes the actor's instance
// row, and commits the window: persisting the updated state, or
// deleting the row when the channel is now empty (RFC 2811 §2).
//
// Callers own the broadcast event that announces the departure
// (PART, KICK, QUIT) and any caller-specific bookkeeping.
func (s *Session) removeMember(ctx context.Context, window *domain.ChannelWindow, actor *domain.Instance) error {
	ch := window.Name()

	if m, ok := window.Members.GetByInstance(actor); ok {
		window.Members.Remove(m)
	}

	actor.LeaveChannels(ch)

	if err := s.store.SaveInstance(ctx, actor); err != nil {
		return fmt.Errorf("save instance: %w", err)
	}

	return s.commitChannel(ctx, window)
}
