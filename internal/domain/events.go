package domain

import (
	"fmt"
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

// ModelDispatchStarted is emitted at the start of a single model
// instance's dispatch turn. The event is actor-scoped — it routes
// to every subscriber that shares at least one channel with
// `Instance`, mirroring RFC 2812 §3.3.1's intersection rule for
// peer-state signals like `Quit` and `NickChange`. No channel
// field: the dispatch's window is observability metadata only,
// and recipients track "this instance is busy" without caring
// which specific window the turn is running in.
type ModelDispatchStarted struct {
	Instance *Instance
	At       time.Time
}

// ModelDispatchDone is the pair to [ModelDispatchStarted], emitted
// when the turn completes (whether or not it produced replies).
// Routing matches `ModelDispatchStarted`; the recipient clears
// the per-instance "thinking" mark and recomputes the aggregate
// pending state from the union of still-dispatching instances.
type ModelDispatchDone struct {
	Instance *Instance
	At       time.Time
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

// NamesEnd closes the member-list reply, mirroring RFC 2812 numeric
// 366 (RPL_ENDOFNAMES). It follows the [NamesReplyEvent] on the
// joiner's own subscription and names the same channel, so a client
// that consumes the snapshot knows the list is complete.
type NamesEnd struct {
	Channel ChannelName
	At      time.Time
}

// Welcome announces successful connection registration, mirroring
// RFC 2812 numeric 001 (RPL_WELCOME). The session emits it once
// per [Session.Connect] so listening clients render the equivalent
// of "Welcome to <server>, <nick>" without inferring it from
// out-of-band state. The chat-screen renders it in its local
// `&modeloff` view; the connection screen surfaces it in the
// boot-time pane.
type Welcome struct {
	ServerName Nick
	Nick       Nick
	At         time.Time
}

// Reconnected announces that the prior session shut down
// uncleanly and the current [Session.Connect] reconciled the
// stale in-memory state. No direct RFC analogue; modeloff-defined
// RPL-style. The chat-screen surfaces it in `&modeloff` so the
// user can see the recovery happened.
type Reconnected struct {
	At time.Time
}

// ModelUnavailableError announces that a per-channel dispatch turn
// could not produce a reply from a model — the store backing the
// dispatch context was unreachable, the model returned an error,
// or the dispatch path itself faulted. No RFC analogue; the IRC
// dispatcher protocol does not model server-side LLM failures.
// `Channel` and `Nick` identify the failed turn so the chat-screen
// can surface the reason in `&modeloff`.
type ModelUnavailableError struct {
	Channel ChannelName
	Nick    Nick
	At      time.Time
}

// Error makes [ModelUnavailableError] satisfy `error` for the
// emission boundary's `errors.As` extraction. The string is also
// what surfaces to operators reading logs.
func (e ModelUnavailableError) Error() string {
	return fmt.Sprintf("model %q unavailable for dispatch in %s", e.Nick, e.Channel)
}

// Pure-live (non-persistable) event types implement Event so they
// flow through the same channel as the persistable types without
// satisfying PersistableEvent. The persistable Channel* types
// implement Event via channel_event.go.

func (ModelReplyEvent) domainEvent()       {}
func (PokeEvent) domainEvent()             {}
func (ErrorEvent) domainEvent()            {}
func (ModelDispatchStarted) domainEvent()  {}
func (ModelDispatchDone) domainEvent()     {}
func (NamesReplyEvent) domainEvent()       {}
func (NamesEnd) domainEvent()              {}
func (Welcome) domainEvent()               {}
func (Reconnected) domainEvent()           {}
func (ModelUnavailableError) domainEvent() {}
