package session

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
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

// persistChannelWindow writes a `*ChannelWindow` to the store and then
// installs the same state as the session's live record.
func (s *Session) persistChannelWindow(ctx context.Context, w *domain.ChannelWindow) error {
	clone := w.Clone()

	if err := s.store.SaveWindow(ctx, clone); err != nil {
		s.recordPersistenceFailure(ctx, w.Name())

		return err
	}

	s.installChannelWindow(clone)

	return nil
}

func (s *Session) persistChannelWindowWithInvitationChange(
	ctx context.Context,
	w *domain.ChannelWindow,
	actor domain.InstanceID,
) error {
	clone := w.Clone()

	if err := s.store.SaveWindow(ctx, clone); err != nil {
		s.recordPersistenceFailure(ctx, w.Name())

		return err
	}

	s.installChannelWindowWithInvitationChange(clone, actor)

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

func (s *Session) commitChannelUpdate(
	ctx context.Context,
	window *domain.ChannelWindow,
	event broadcastEvent,
) error {
	routes := s.channelEventRoutes(ctx, event, window.Name())
	locked := lockRouteSubscriptions(routes)
	defer func() {
		for _, sub := range slices.Backward(locked) {
			sub.replayMu.Unlock()
		}
	}()

	records, indexes := projectedScrollbackRecords(routes)
	committed, err := s.store.CommitChannelUpdate(ctx, store.ChannelUpdate{
		Window: window, Event: event, Scrollback: records,
	})
	if err != nil {
		s.recordPersistenceFailure(ctx, window.Name())

		return err
	}

	s.installChannelWindow(window)
	for i := range routes {
		routes[i].eventID = committed.EventID
	}
	for _, sub := range locked {
		sub.outMu.Lock()
	}
	_, overflowed := queueRoutesLocked(
		routes, indexes, committed.ScrollbackIDs, len(records),
	)
	for _, sub := range slices.Backward(locked) {
		sub.outMu.Unlock()
	}

	for _, sub := range overflowed {
		s.disconnectOverflowed(ctx, sub)
	}

	return nil
}

func (s *Session) commitMemberDeparture(
	ctx context.Context,
	window *domain.ChannelWindow,
	actor *domain.Instance,
	event domain.ChannelDepartureEvent,
) error {
	candidateWindow := window.Clone()
	candidateActor := actor.Snapshot()
	s.removeMemberFromWindow(candidateWindow, candidateActor)

	routes := s.channelEventRoutes(ctx, event, window.Name())
	actorClient := s.lookupClientHandle(protocol.ClientID(actor.ID()))
	locked := lockRouteSubscriptions(routes, actorClient)
	defer func() {
		for _, sub := range slices.Backward(locked) {
			sub.replayMu.Unlock()
		}
	}()

	excluded := map[domain.InstanceID]struct{}{actor.ID(): {}}
	records, indexes := projectedScrollbackRecordsExcept(routes, excluded)
	committed, err := s.store.CommitChannelDeparture(ctx, store.ChannelDeparture{
		Window:     candidateWindow,
		Instance:   candidateActor,
		Event:      event,
		Scrollback: records,
	})
	if err != nil {
		s.recordPersistenceFailure(ctx, window.Name())

		return err
	}

	actor.LeaveChannels(window.Name())
	if actorClient != nil {
		actorClient.bumpWindowEpoch(window.Name())
	}
	if candidateWindow.Members.Len() == 0 {
		s.removeLiveChannel(window.Name())
	} else {
		s.installChannelWindow(candidateWindow)
	}

	for i := range routes {
		routes[i].eventID = committed.EventID
	}
	for _, sub := range locked {
		sub.outMu.Lock()
	}
	_, overflowed := queueRoutesLocked(routes, indexes, committed.ScrollbackIDs, len(records))
	for _, sub := range slices.Backward(locked) {
		sub.outMu.Unlock()
	}

	for _, sub := range overflowed {
		s.disconnectOverflowed(ctx, sub)
	}

	return nil
}

func (s *Session) removeDeletedMember(ctx context.Context, window *domain.ChannelWindow, actor *domain.Instance) error {
	return s.withClientReplay(actor.ID(), func() error {
		s.bumpClientWindow(actor.ID(), window.Name())
		s.removeMemberFromWindow(window, actor)

		var channelErr error
		if window.Members.Len() == 0 {
			channelErr = s.destroyChannel(ctx, window.Name())
		} else {
			channelErr = s.store.SaveWindow(ctx, window)
			s.installChannelWindow(window)
		}
		if channelErr != nil {
			s.recordPersistenceFailure(ctx, window.Name())
		}
		scrollbackErr := s.store.DeleteChannelScrollback(ctx, actor.ID(), window.Name())
		repliesErr := s.store.DeleteInstanceRepliesForWindow(ctx, actor.ID(), protocol.ChannelWindowTarget(window.Name()))
		turnsErr := s.store.DeleteModelTurnsForWindow(ctx, actor.ID(), protocol.ChannelWindowTarget(window.Name()))

		return errors.Join(channelErr, scrollbackErr, repliesErr, turnsErr)
	})
}

func (s *Session) withClientReplay(id domain.InstanceID, fn func() error) error {
	client := s.lookupClientHandle(protocol.ClientID(id))
	if client == nil {
		return fn()
	}

	client.replayMu.Lock()
	defer client.replayMu.Unlock()

	return fn()
}

func (s *Session) bumpClientWindow(id domain.InstanceID, window domain.ChannelName) {
	if client := s.lookupClientHandle(protocol.ClientID(id)); client != nil {
		client.bumpWindowEpoch(window)
	}
}

func (s *Session) removeMemberFromWindow(window *domain.ChannelWindow, actor *domain.Instance) {
	if member, ok := window.Members.GetByInstance(actor); ok {
		window.Members.Remove(member)
	}

	actor.LeaveChannels(window.Name())
}
