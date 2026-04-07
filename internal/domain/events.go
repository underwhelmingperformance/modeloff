package domain

import (
	"time"
)

// MessageEvent is emitted when a new message is sent in a channel.
type MessageEvent struct {
	Event ChannelMessage
}

// JoinEvent is emitted when a user or model joins a channel. Created
// is true when the channel was newly created by this join.
type JoinEvent struct {
	Channel ChannelName
	Nick    Nick
	Created bool
	Message string
	At      time.Time
}

// PartEvent is emitted when a user or model leaves a channel.
type PartEvent struct {
	Channel ChannelName
	Nick    Nick
	Message string
	At      time.Time
}

// QuitEvent is emitted when a user or model quits the server,
// leaving all channels.
type QuitEvent struct {
	Nick    Nick
	Message string
	At      time.Time
}

// NickChangeEvent is emitted when a user changes their nickname.
// One event is emitted per channel the user is in.
type NickChangeEvent struct {
	Channel ChannelName
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
	Instance Instance
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
	Event    ChannelMessage
	Instance Nick
	At       time.Time
}

// ModeChangeEvent is emitted when a member's privilege level changes
// in a channel.
type ModeChangeEvent struct {
	Channel ChannelName
	Nick    Nick
	Mode    NickMode
	Actor   string
	At      time.Time
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

// PokeEvent is emitted when a periodic poke should be dispatched to
// model instances in a channel.
type PokeEvent struct {
	Channel ChannelName
	At      time.Time
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

// DispatchDoneEvent is emitted when dispatch to all model instances
// in a channel has completed.
type DispatchDoneEvent struct {
	Channel ChannelName
}

// All event types implement SessionEvent so they can flow through the
// session's unified event channel.

func (MessageEvent) sessionEvent()         {}
func (JoinEvent) sessionEvent()            {}
func (PartEvent) sessionEvent()            {}
func (QuitEvent) sessionEvent()            {}
func (NickChangeEvent) sessionEvent()      {}
func (TopicChangeEvent) sessionEvent()     {}
func (ModelInvitedEvent) sessionEvent()    {}
func (ModelKickedEvent) sessionEvent()     {}
func (ModelReplyEvent) sessionEvent()      {}
func (ModeChangeEvent) sessionEvent()      {}
func (DMOpenedEvent) sessionEvent()        {}
func (ConfigChangedEvent) sessionEvent()   {}
func (PokeEvent) sessionEvent()            {}
func (ErrorEvent) sessionEvent()           {}
func (DispatchStartedEvent) sessionEvent() {}
func (DispatchDoneEvent) sessionEvent()    {}

// InitialLoadEvent carries the data needed to render the chat screen
// after loading from the session at startup.
type InitialLoadEvent struct {
	Channels  []Channel
	Instances []Instance
	Active    ChannelName
	Topic     string
	Unread    map[ChannelName]int
	Members   MemberList
	At        time.Time
}
