// Package session provides the backend coordinator that ties together
// stores, the API client, and the protocol layer. It manages rooms,
// model instances, and handles commands by updating state and emitting
// domain events.
package session

import (
	"context"
	"fmt"
	"time"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/store"
)

// Session is the backend coordinator. It bridges the UI layer and
// the underlying stores and API client.
type Session struct {
	store  store.Store
	memory memory.Store
	api    api.Client
	config config.Store

	userNick domain.Nick
	now      func() time.Time
}

// New creates a Session with the given dependencies.
func New(
	s store.Store,
	m memory.Store,
	a api.Client,
	c config.Store,
	userNick domain.Nick,
) *Session {
	return &Session{
		store:    s,
		memory:   m,
		api:      a,
		config:   c,
		userNick: userNick,
		now:      time.Now,
	}
}

// UserNick returns the current user nickname.
func (s *Session) UserNick() domain.Nick {
	return s.userNick
}

// Join creates or opens a room and returns a JoinEvent.
func (s *Session) Join(ctx context.Context, roomName string) (domain.JoinEvent, error) {
	name := domain.RoomName(roomName)

	var created bool

	_, err := s.store.GetRoom(ctx, name)
	if err != nil {
		created = true

		room := domain.Room{
			Name:    name,
			Kind:    domain.RoomChannel,
			Members: []domain.Nick{s.userNick},
			Created: s.now(),
		}

		if err := s.store.SaveRoom(ctx, room); err != nil {
			return domain.JoinEvent{}, fmt.Errorf("save room: %w", err)
		}
	}

	if err := s.store.SetLastRoom(ctx, name); err != nil {
		return domain.JoinEvent{}, fmt.Errorf("set last room: %w", err)
	}

	return domain.JoinEvent{
		Room:    name,
		Nick:    s.userNick,
		Created: created,
		At:      s.now(),
	}, nil
}

// Leave records the user leaving a room and returns a PartEvent.
func (s *Session) Leave(ctx context.Context, roomName domain.RoomName) (domain.PartEvent, error) {
	_, err := s.store.GetRoom(ctx, roomName)
	if err != nil {
		return domain.PartEvent{}, fmt.Errorf("room not found: %w", err)
	}

	return domain.PartEvent{
		Room: roomName,
		Nick: s.userNick,
		At:   s.now(),
	}, nil
}

// ListRooms returns all persisted rooms.
func (s *Session) ListRooms(ctx context.Context) ([]domain.Room, error) {
	return s.store.ListRooms(ctx)
}

// Invite adds a model instance to a room. If the model has no nick
// yet, one is generated via the API.
func (s *Session) Invite(
	ctx context.Context,
	roomName domain.RoomName,
	modelID domain.ModelID,
) (domain.ModelInvitedEvent, error) {
	nick, err := s.api.GenerateNick(ctx, modelID)
	if err != nil {
		return domain.ModelInvitedEvent{}, fmt.Errorf("generate nick: %w", err)
	}

	inst := domain.ModelInstance{
		Nick:    nick,
		ModelID: modelID,
		Rooms:   []domain.RoomName{roomName},
	}

	if err := s.store.SaveInstance(ctx, inst); err != nil {
		return domain.ModelInvitedEvent{}, fmt.Errorf("save instance: %w", err)
	}

	// Add model to room members.
	room, err := s.store.GetRoom(ctx, roomName)
	if err != nil {
		return domain.ModelInvitedEvent{}, fmt.Errorf("get room: %w", err)
	}

	room.Members = append(room.Members, nick)

	if err := s.store.SaveRoom(ctx, room); err != nil {
		return domain.ModelInvitedEvent{}, fmt.Errorf("save room: %w", err)
	}

	return domain.ModelInvitedEvent{
		Room:     roomName,
		Instance: inst,
		At:       s.now(),
	}, nil
}

// Kick removes a model instance from a room.
func (s *Session) Kick(
	ctx context.Context,
	roomName domain.RoomName,
	nick domain.Nick,
) (domain.ModelKickedEvent, error) {
	room, err := s.store.GetRoom(ctx, roomName)
	if err != nil {
		return domain.ModelKickedEvent{}, fmt.Errorf("get room: %w", err)
	}

	filtered := make([]domain.Nick, 0, len(room.Members))
	for _, m := range room.Members {
		if m != nick {
			filtered = append(filtered, m)
		}
	}

	room.Members = filtered

	if err := s.store.SaveRoom(ctx, room); err != nil {
		return domain.ModelKickedEvent{}, fmt.Errorf("save room: %w", err)
	}

	return domain.ModelKickedEvent{
		Room: roomName,
		Nick: nick,
		At:   s.now(),
	}, nil
}

// SendMessage saves a message to a room and returns the message
// event.
func (s *Session) SendMessage(
	ctx context.Context,
	roomName domain.RoomName,
	body string,
) (domain.MessageEvent, error) {
	msg := domain.Message{
		ID:     fmt.Sprintf("%d", s.now().UnixNano()),
		Room:   roomName,
		From:   s.userNick,
		Body:   body,
		SentAt: s.now(),
	}

	if err := s.store.SaveMessage(ctx, msg); err != nil {
		return domain.MessageEvent{}, fmt.Errorf("save message: %w", err)
	}

	return domain.MessageEvent{Message: msg}, nil
}

// SetTitle sets the title of a room.
func (s *Session) SetTitle(
	ctx context.Context,
	roomName domain.RoomName,
	title string,
) (domain.TopicChangeEvent, error) {
	room, err := s.store.GetRoom(ctx, roomName)
	if err != nil {
		return domain.TopicChangeEvent{}, fmt.Errorf("get room: %w", err)
	}

	room.Title = title

	if err := s.store.SaveRoom(ctx, room); err != nil {
		return domain.TopicChangeEvent{}, fmt.Errorf("save room: %w", err)
	}

	return domain.TopicChangeEvent{
		Room:  roomName,
		Title: title,
		By:    s.userNick,
		At:    s.now(),
	}, nil
}

// ChangeNick changes the user's nickname.
func (s *Session) ChangeNick(newNick domain.Nick) domain.NickChangeEvent {
	evt := domain.NickChangeEvent{
		OldNick: s.userNick,
		NewNick: newNick,
		At:      s.now(),
	}

	s.userNick = newNick

	return evt
}

// Whois returns metadata about a model instance.
func (s *Session) Whois(ctx context.Context, nick domain.Nick) (domain.ModelInstance, error) {
	return s.store.GetInstance(ctx, nick)
}

// LastRoom returns the room that was last active.
func (s *Session) LastRoom(ctx context.Context) (domain.RoomName, error) {
	return s.store.GetLastRoom(ctx)
}

// Messages returns all messages for a room.
func (s *Session) Messages(ctx context.Context, room domain.RoomName) ([]domain.Message, error) {
	return s.store.ListMessages(ctx, room)
}
