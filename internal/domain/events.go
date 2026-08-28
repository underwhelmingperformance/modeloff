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

// ErrorEvent wraps a backend error as a domain event. Target names
// the window the failed command was issued from, the same role
// the command reply envelope carries for server replies: the
// chat-screen renders the error there even after the user has
// switched windows, falling back when Target is empty.
type ErrorEvent struct {
	Operation string
	Err       error
	Target    ChannelName
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
// instance's dispatch turn. The session routes it only to clients
// that can observe that turn's channel or direct-message window. The
// routing target stays in the delivery envelope rather than the IRC
// event because dispatch lifecycle is transient client state.
type ModelDispatchStarted struct {
	Source Source
	At     time.Time
}

// ModelDispatchDone is the pair to [ModelDispatchStarted], emitted
// when the turn completes (whether or not it produced replies).
// Routing matches `ModelDispatchStarted`; the recipient clears
// the per-instance "thinking" mark and recomputes the aggregate
// pending state from the union of still-dispatching instances.
type ModelDispatchDone struct {
	Source Source
	At     time.Time
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

// ConnectionError is the server's fatal ERROR message. It is the last
// event sent directly to a client before its connection closes.
type ConnectionError struct {
	Reason string
	At     time.Time
}

// KillNotice is the operator-authored KILL message delivered to the
// client whose connection is about to close. Shared peers receive the
// subsequent [Quit], not this point-to-point event.
type KillNotice struct {
	Source  Source
	Subject Nick
	Reason  string
	At      time.Time
}

// ModelFailureReason says what stopped a dispatch turn. There is one
// reason per thing the operator reading the line would do about it, so
// two failures share a reason only when they call for the same action.
// The zero value covers a turn that faulted with no more specific cause.
type ModelFailureReason string

const (
	// ModelFailureUnavailable is a dispatch turn that faulted for a
	// reason the dispatch path could not classify.
	ModelFailureUnavailable ModelFailureReason = ""
	// ModelFailureNoAPIKey is a turn that never reached a provider
	// because no API key is configured.
	ModelFailureNoAPIKey ModelFailureReason = "no_api_key"
	// ModelFailureAuth is a provider rejecting the configured key.
	ModelFailureAuth ModelFailureReason = "auth_refused"
	// ModelFailureForbidden is a provider accepting the key and
	// refusing the request anyway, which is what an account without
	// access to a model receives.
	ModelFailureForbidden ModelFailureReason = "forbidden"
	// ModelFailureNoCredit is a provider refusing the request for
	// payment.
	ModelFailureNoCredit ModelFailureReason = "no_credit"
	// ModelFailureUnknownModel is a provider that does not serve the
	// model this instance was created with.
	ModelFailureUnknownModel ModelFailureReason = "unknown_model"
	// ModelFailureRateLimited is a provider asking for fewer requests.
	ModelFailureRateLimited ModelFailureReason = "rate_limited"
	// ModelFailureBadRequest is a provider refusing the request as
	// malformed, which is this application's fault and not the
	// operator's.
	ModelFailureBadRequest ModelFailureReason = "bad_request"
	// ModelFailureContextWindow is a prompt the model's context window
	// cannot hold, which compaction could not bring inside it.
	ModelFailureContextWindow ModelFailureReason = "context_window"
	// ModelFailureUpstream is a provider failing on its own side.
	ModelFailureUpstream ModelFailureReason = "upstream"
)

// String completes the sentence [ModelUnavailableError.Error] opens
// with the model's nick. Each answer names what to do, because the line
// exists to be acted on.
func (r ModelFailureReason) String() string {
	switch r {
	case ModelFailureNoAPIKey:
		return "cannot dispatch until an API key is configured"
	case ModelFailureAuth:
		return "was refused by its provider: check the configured API key"
	case ModelFailureForbidden:
		return "was refused by its provider: the key is accepted but this " +
			"account cannot use this model"
	case ModelFailureNoCredit:
		return "was refused by its provider for payment: add credit to the account"
	case ModelFailureUnknownModel:
		return "names a model its provider does not serve: pick another with /add-model"
	case ModelFailureRateLimited:
		return "is being rate limited by its provider: it will answer again shortly"
	case ModelFailureBadRequest:
		return "sent a request its provider would not accept, which is a fault " +
			"in this application and not in the configuration"
	case ModelFailureContextWindow:
		return "has more context than its window holds, and a retry will not clear it"
	case ModelFailureUpstream:
		return "could not complete its provider request: the provider failed on its own side"
	}

	return "unavailable for dispatch"
}

// ModelUnavailableError announces that a dispatch turn
// could not produce a reply from a model because the context store
// was unreachable, the model returned an error, or the dispatch path
// faulted. No RFC analogue; the IRC
// dispatcher protocol does not model server-side LLM failures.
// The delivery envelope supplies each recipient's corresponding
// window.
//
// Reason is what separates the failures an operator can act on. A
// refused key and an overflowing context window both stop every turn
// the instance takes, and the two need different things done about
// them.
type ModelUnavailableError struct {
	Source Source
	Reason ModelFailureReason
	At     time.Time
}

// Error makes [ModelUnavailableError] satisfy `error` for the
// emission boundary's `errors.As` extraction. The string is also what
// the chat-screen renders for the failure and what surfaces to
// operators reading logs.
func (e ModelUnavailableError) Error() string {
	return fmt.Sprintf("model %q %s", e.Source.Nick(), e.Reason)
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
func (ConnectionError) domainEvent()       {}
func (KillNotice) domainEvent()            {}
func (ModelUnavailableError) domainEvent() {}
