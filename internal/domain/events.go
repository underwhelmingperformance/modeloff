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

// ModelReplyEvent is emitted when a model instance responds to events
// in a channel.
type ModelReplyEvent struct {
	Channel  ChannelName
	Message  Message
	Instance Nick
	At       time.Time
}

func (ModelReplyEvent) eventMarker() {}

// DMOpenedEvent is emitted when a direct message conversation is
// opened or created.
type DMOpenedEvent struct {
	Channel Channel
	Nick    Nick
	Created bool
	At      time.Time
}

func (DMOpenedEvent) eventMarker() {}

// ConfigChangedEvent is emitted when a runtime configuration value is
// updated.
type ConfigChangedEvent struct {
	Operation string
	At        time.Time
}

func (ConfigChangedEvent) eventMarker() {}

// ErrorEvent wraps a backend error as a domain event.
type ErrorEvent struct {
	Operation string
	Err       error
	At        time.Time
}

func (ErrorEvent) eventMarker() {}

// InitialLoadEvent carries the data needed to render the chat screen
// after loading from the session at startup.
type InitialLoadEvent struct {
	Channels  []Channel
	Instances []ModelInstance
	Active    ChannelName
	Topic     string
	Messages  []Message
	Unread    map[ChannelName]int
	Members   []Member
	At        time.Time
}

func (InitialLoadEvent) eventMarker() {}
