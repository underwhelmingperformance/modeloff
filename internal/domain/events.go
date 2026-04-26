package domain

import (
	"time"
)

// ModelReplyEvent is emitted when a model instance responds to events
// in a channel. Instance is the replying instance's handle. The
// embedded `Message` is the prepared message the chat screen
// will commit to the channel's event log if the user does not abort.
type ModelReplyEvent struct {
	Channel  ChannelName
	Event    Message
	Instance *Instance
	At       time.Time
}

// DMOpenedEvent is emitted when the user opens a direct-message
// window. `DM` is the typed `*DMWindow` the chat screen registers
// in its sidebar; the `Counterpart` carried on the window is the
// non-user instance, resolved at open time. There is no event
// equivalent for models — DMs are stateless from the server's
// point of view, so a model "opening" a conversation isn't an
// observable thing.
type DMOpenedEvent struct {
	DM      *DMWindow
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

// Event is the sealed top-level interface every domain event
// implements. The session's background event channel is typed as
// `chan Event`, so every concrete domain event (persistable
// `Channel*` types and pure-live types alike) flows through one
// pipe. Persistability is a per-handler concern: the store accepts
// only `PersistableEvent` (a subset of `Event` that adds the methods
// needed for marshalling and replay), and consumers that handle
// derived/transient state (dispatch lifecycle, focus changes, etc.)
// just type-switch on the variants they care about.
type Event interface {
	domainEvent()
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

// NamesReplyEvent is emitted UI-only at user-join time to broadcast
// the current member list of a channel the user has just joined,
// matching IRC's RPL_NAMREPLY. The joiner's UI uses it to populate
// its local member-list cache with members that pre-existed the
// join — without it, the cache would only see the joiner themselves
// and miss any models or other users already in the channel. Models
// already in the channel see their own future events through the
// usual emission paths; this is a joiner-targeted snapshot, not a
// broadcast to everyone.
type NamesReplyEvent struct {
	Channel ChannelName
	Members MemberList
	At      time.Time
}

// StatusOpenedEvent is emitted UI-only when the session opens its
// status window (`&modeloff`) on connect. The status window is a
// virtual server view, not a channel: it has no members, no modes,
// and no join/part lifecycle. Consumers use this signal to register
// the window in their sidebar without faking the channel-join
// scaffolding that would otherwise be required.
type StatusOpenedEvent struct {
	Channel ChannelName
	At      time.Time
}

// Pure-live (non-persistable) event types implement Event so they
// flow through the session's unified event channel without
// satisfying PersistableEvent. The persistable Channel* types implement
// Event via channel_event.go.

func (ModelReplyEvent) domainEvent()      {}
func (DMOpenedEvent) domainEvent()        {}
func (ConfigChangedEvent) domainEvent()   {}
func (PokeEvent) domainEvent()            {}
func (ErrorEvent) domainEvent()           {}
func (DispatchStartedEvent) domainEvent() {}
func (DispatchDoneEvent) domainEvent()    {}
func (FocusChannelEvent) domainEvent()    {}
func (SystemNoticeEvent) domainEvent()    {}
func (NamesReplyEvent) domainEvent()      {}
func (StatusOpenedEvent) domainEvent()    {}
