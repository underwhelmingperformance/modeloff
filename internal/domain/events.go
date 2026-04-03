package domain

import "time"

// Event is a marker interface for all domain events that flow through
// the system. These are converted to tea.Msg at the UI boundary.
type Event interface {
	eventMarker()
}

// MessageEvent is emitted when a new message is sent in a room.
type MessageEvent struct {
	Message Message
}

func (MessageEvent) eventMarker() {}

// JoinEvent is emitted when a user or model joins a room. Created is
// true when the room was newly created by this join.
type JoinEvent struct {
	Room    RoomName
	Nick    Nick
	Created bool
	At      time.Time
}

func (JoinEvent) eventMarker() {}

// PartEvent is emitted when a user or model leaves a room.
type PartEvent struct {
	Room RoomName
	Nick Nick
	At   time.Time
}

func (PartEvent) eventMarker() {}

// NickChangeEvent is emitted when a user changes their nickname.
type NickChangeEvent struct {
	OldNick Nick
	NewNick Nick
	At      time.Time
}

func (NickChangeEvent) eventMarker() {}

// TopicChangeEvent is emitted when a room's title is changed.
type TopicChangeEvent struct {
	Room  RoomName
	Title string
	By    Nick
	At    time.Time
}

func (TopicChangeEvent) eventMarker() {}

// ModelInvitedEvent is emitted when a model instance is added to a room.
type ModelInvitedEvent struct {
	Room     RoomName
	Instance ModelInstance
	At       time.Time
}

func (ModelInvitedEvent) eventMarker() {}

// ModelKickedEvent is emitted when a model instance is removed from a room.
type ModelKickedEvent struct {
	Room RoomName
	Nick Nick
	At   time.Time
}

func (ModelKickedEvent) eventMarker() {}
