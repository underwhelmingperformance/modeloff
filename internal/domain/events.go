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
// InstanceID identifies the joiner; it is empty for the human user.
type JoinEvent struct {
	Channel    ChannelName
	InstanceID InstanceID
	Nick       Nick
	Created    bool
	Message    string
	At         time.Time
}

// PartEvent is emitted when a user or model leaves a channel.
// InstanceID identifies the leaver; it is empty for the human user.
type PartEvent struct {
	Channel    ChannelName
	InstanceID InstanceID
	Nick       Nick
	Message    string
	At         time.Time
}

// QuitEvent is emitted when a user or model quits the server,
// leaving all channels. InstanceID identifies the quitter; it is
// empty for the human user.
type QuitEvent struct {
	InstanceID InstanceID
	Nick       Nick
	Message    string
	At         time.Time
}

// NickChangeEvent is emitted when a user changes their nickname.
// One event is emitted per channel the user is in. InstanceID
// identifies the renamed actor so membership can be re-keyed in
// place without identity loss.
type NickChangeEvent struct {
	Channel    ChannelName
	InstanceID InstanceID
	OldNick    Nick
	NewNick    Nick
	At         time.Time
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
	By       Nick
	At       time.Time
}

// ModelKickedEvent is emitted when a model instance is removed from a
// channel.
type ModelKickedEvent struct {
	Channel    ChannelName
	InstanceID InstanceID
	Nick       Nick
	By         Nick
	At         time.Time
}

// ModelReplyEvent is emitted when a model instance responds to events
// in a channel. InstanceID identifies the replying instance.
type ModelReplyEvent struct {
	Channel    ChannelName
	Event      ChannelMessage
	InstanceID InstanceID
	Instance   Nick
	At         time.Time
}

// ModeChangeEvent is emitted when a member's privilege level changes
// in a channel. InstanceID identifies the affected member; it is
// empty for the human user.
type ModeChangeEvent struct {
	Channel    ChannelName
	InstanceID InstanceID
	Nick       Nick
	Mode       NickMode
	Actor      string
	At         time.Time
}

// DMOpenedEvent is emitted when a direct message conversation is
// opened or created.
type DMOpenedEvent struct {
	Channel Channel
	Nick    Nick
	Created bool
	At      time.Time
}

// TopicInfoEvent is emitted during the join protocol when a channel
// has a topic. It is display-only and not persisted.
type TopicInfoEvent struct {
	Channel    ChannelName
	Topic      string
	TopicSetBy Nick
	TopicSetAt time.Time
	At         time.Time
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

// ChannelFocusEvent is returned by UI commands when the user switches
// to a channel they already belong to. Unlike JoinEvent, it is not
// emitted by the session and does not flow through the event channel.
type ChannelFocusEvent struct {
	Channel ChannelName
}

// ErrorEvent wraps a backend error as a domain event.
type ErrorEvent struct {
	Operation string
	Err       error
	At        time.Time
}

// FocusChannelEvent is emitted by the session when a channel should
// become active in the UI. Unlike ChannelFocusEvent (which is a
// UI-only message used by direct channel switches), this flows
// through the session event channel so the chat screen can react to
// session-driven focus changes such as last-channel restoration at
// the end of autojoin.
type FocusChannelEvent struct {
	Channel ChannelName
	At      time.Time
}

// SystemNoticeEvent is emitted when a system notice has been
// appended to a channel's event log. UI consumers can use it to
// refresh the affected channel's view in real time without polling.
type SystemNoticeEvent struct {
	Channel ChannelName
	Stored  StoredEvent
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
func (TopicInfoEvent) sessionEvent()       {}
func (ConfigChangedEvent) sessionEvent()   {}
func (PokeEvent) sessionEvent()            {}
func (ErrorEvent) sessionEvent()           {}
func (DispatchStartedEvent) sessionEvent() {}
func (DispatchDoneEvent) sessionEvent()    {}
func (FocusChannelEvent) sessionEvent()    {}
func (SystemNoticeEvent) sessionEvent()    {}

