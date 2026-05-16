package domain

import (
	"fmt"
	"time"
)

// ProtocolEvent is the curated subset of [Event] that the
// `internal/protocol` package exposes as its `Event` sum. Membership
// is sealed by the unexported `isProtocolEvent` method declared
// below: the only types that satisfy it are the ones listed below.
// The seal lives in `domain` rather than `protocol` because Go
// scopes unexported method names to the declaring package —
// declaring it here lets the existing persistable and pure-live
// event types satisfy the sum without a wrapper layer.
//
// This is referenced by clients via the `protocol.Event` alias.
type ProtocolEvent interface {
	Event
	isProtocolEvent()
}

// Persistable channel events. Order matches the seal block in
// `channel_event.go` so reviewers can diff the two side by side.
func (Message) isProtocolEvent()      {}
func (Join) isProtocolEvent()         {}
func (Part) isProtocolEvent()         {}
func (Quit) isProtocolEvent()         {}
func (TopicChange) isProtocolEvent()  {}
func (ModeChange) isProtocolEvent()   {}
func (ModelInvited) isProtocolEvent() {}
func (ModelKicked) isProtocolEvent()  {}
func (NickChange) isProtocolEvent()   {}
func (TopicInfo) isProtocolEvent()    {}
func (Help) isProtocolEvent()         {}
func (Whois) isProtocolEvent()        {}
func (ListReply) isProtocolEvent()    {}
func (ListEnd) isProtocolEvent()      {}
func (CommandError) isProtocolEvent() {}
func (UsageHint) isProtocolEvent()    {}
func (SystemNotice) isProtocolEvent() {}
func (PersonasList) isProtocolEvent() {}

// Pure-live events. Order matches the seal block in `events.go`.
func (PokeEvent) isProtocolEvent()             {}
func (DispatchStartedEvent) isProtocolEvent()  {}
func (DispatchDoneEvent) isProtocolEvent()     {}
func (FocusChannelEvent) isProtocolEvent()     {}
func (NamesReplyEvent) isProtocolEvent()       {}
func (Welcome) isProtocolEvent()               {}
func (Reconnected) isProtocolEvent()           {}
func (ModelUnavailableError) isProtocolEvent() {}

// Typed errors that double as protocol events. They satisfy both
// the `error` interface (for `errors.As` extraction at the
// emission boundary) and the protocol-event seal (so the session
// can `emit` them like any other wire event).
func (UnknownNickError) isProtocolEvent()   {}
func (NoSuchChannelError) isProtocolEvent() {}
func (NickInUseError) isProtocolEvent()     {}
func (NotOperatorError) isProtocolEvent()   {}

func (UnknownNickError) domainEvent()   {}
func (NoSuchChannelError) domainEvent() {}
func (NickInUseError) domainEvent()     {}
func (NotOperatorError) domainEvent()   {}

// `Killed` is a protocol-only event (no domain-side persistence).
func (Killed) isProtocolEvent() {}

// Killed is the target-side notification emitted on a client's
// Events channel as the final wire event before the server reaps
// its subscription, in response to an operator-issued KILL
// (RFC 2812 §3.7.1). Renderers display it as the scrollback's last
// word; peers in shared channels separately receive a QUIT line
// with the conventional "Killed by <oper> (<reason>)" reason.
type Killed struct {
	By     Nick
	Reason string
	At     time.Time
}

func (Killed) domainEvent() {}

// NotOperatorError is the protocol-shaped form of ERR_NOPRIVILEGES
// (RFC 2812 numeric 481). The dispatcher returns it from operator-
// gated handlers ([protocol.AddModel], [protocol.Kill]) in
// `Response.Err` when the issuing client lacks
// [protocol.ModeOperator]. It is also a protocol event so future
// emission paths can surface it on the bus.
type NotOperatorError struct {
	// Command names the operator-gated command that was refused, so
	// renderers and tool-result formatters can identify which call
	// was rejected without reparsing the error string.
	Command string
}

// Error makes [NotOperatorError] satisfy the `error` interface.
// The message follows the IRC numeric-reply convention.
func (e NotOperatorError) Error() string {
	if e.Command == "" {
		return "permission denied: not an operator"
	}

	return fmt.Sprintf("permission denied: %s requires operator privileges", e.Command)
}
