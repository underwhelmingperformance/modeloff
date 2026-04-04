package domain

import "time"

// Event is a marker interface for all domain events that flow through
// the system. These are converted to tea.Msg at the UI boundary.
type Event interface {
	eventMarker()
}

// MessageEvent is emitted when a new message is sent in a channel.
type MessageEvent struct {
	Message Message
}

func (MessageEvent) eventMarker() {}

// JoinEvent is emitted when a user or model joins a channel. Created
// is true when the channel was newly created by this join.
type JoinEvent struct {
	Channel ChannelName
	Nick    Nick
	Created bool
	At      time.Time
}

func (JoinEvent) eventMarker() {}

// PartEvent is emitted when a user or model leaves a channel.
type PartEvent struct {
	Channel ChannelName
	Nick    Nick
	At      time.Time
}

func (PartEvent) eventMarker() {}

// NickChangeEvent is emitted when a user changes their nickname.
type NickChangeEvent struct {
	OldNick Nick
	NewNick Nick
	At      time.Time
}

func (NickChangeEvent) eventMarker() {}

// TopicChangeEvent is emitted when a channel's topic is changed.
type TopicChangeEvent struct {
	Channel ChannelName
	Topic   string
	By      Nick
	At      time.Time
}

func (TopicChangeEvent) eventMarker() {}

// ModelInvitedEvent is emitted when a model instance is added to a
// channel.
type ModelInvitedEvent struct {
	Channel  ChannelName
	Instance ModelInstance
	At       time.Time
}

func (ModelInvitedEvent) eventMarker() {}

// ModelKickedEvent is emitted when a model instance is removed from a
// channel.
type ModelKickedEvent struct {
	Channel ChannelName
	Nick    Nick
	At      time.Time
}

func (ModelKickedEvent) eventMarker() {}
