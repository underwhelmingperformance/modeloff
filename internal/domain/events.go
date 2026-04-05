package domain

import "time"

// MessageEvent is emitted when a new message is sent in a channel.
type MessageEvent struct {
	Message Message
}

// JoinEvent is emitted when a user or model joins a channel. Created
// is true when the channel was newly created by this join.
type JoinEvent struct {
	Channel ChannelName
	Nick    Nick
	Created bool
	At      time.Time
}

// PartEvent is emitted when a user or model leaves a channel.
type PartEvent struct {
	Channel ChannelName
	Nick    Nick
	At      time.Time
}

// NickChangeEvent is emitted when a user changes their nickname.
type NickChangeEvent struct {
	OldNick Nick
	NewNick Nick
	At      time.Time
}

// TopicChangeEvent is emitted when a channel's topic is changed.
type TopicChangeEvent struct {
	Channel ChannelName
	Topic   string
	By      Nick
	At      time.Time
}

// ModelInvitedEvent is emitted when a model instance is added to a
// channel.
type ModelInvitedEvent struct {
	Channel  ChannelName
	Instance ModelInstance
	At       time.Time
}

// ModelKickedEvent is emitted when a model instance is removed from a
// channel.
type ModelKickedEvent struct {
	Channel ChannelName
	Nick    Nick
	At      time.Time
}

// ModelReplyEvent is emitted when a model instance responds to events
// in a channel.
type ModelReplyEvent struct {
	Channel  ChannelName
	Message  Message
	Instance Nick
	At       time.Time
}

// DMOpenedEvent is emitted when a direct message conversation is
// opened or created.
type DMOpenedEvent struct {
	Channel Channel
	Nick    Nick
	Created bool
	At      time.Time
}

// ConfigChangedEvent is emitted when a runtime configuration value is
// updated.
type ConfigChangedEvent struct {
	Operation string
	At        time.Time
}

// ErrorEvent wraps a backend error as a domain event.
type ErrorEvent struct {
	Operation string
	Err       error
	At        time.Time
}

// SessionEvent is the interface for events emitted on the session's
// background event channel.
type SessionEvent interface {
	sessionEvent()
}

// DispatchStartedEvent is emitted when the session begins dispatching
// events to model instances in a channel.
type DispatchStartedEvent struct {
	Channel ChannelName
	Nicks   []Nick
}

func (DispatchStartedEvent) sessionEvent() {}

// DispatchDoneEvent is emitted when dispatch to all model instances
// in a channel has completed.
type DispatchDoneEvent struct {
	Channel ChannelName
}

func (DispatchDoneEvent) sessionEvent() {}

// Marker methods for existing event types that can appear on the
// session event channel.

func (ModelReplyEvent) sessionEvent() {}
func (ErrorEvent) sessionEvent()      {}

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
