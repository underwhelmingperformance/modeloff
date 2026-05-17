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
func (ModelDispatchStarted) isProtocolEvent()  {}
func (ModelDispatchDone) isProtocolEvent()     {}
func (NamesReplyEvent) isProtocolEvent()       {}
func (Welcome) isProtocolEvent()               {}
func (Reconnected) isProtocolEvent()           {}
func (ModelUnavailableError) isProtocolEvent() {}

// Typed errors that double as protocol events. They satisfy both
// the `error` interface (for `errors.As` extraction at the
// emission boundary) and the protocol-event seal (so the session
// can `emit` them like any other wire event).
func (UnknownNickError) isProtocolEvent()         {}
func (NoSuchChannelError) isProtocolEvent()       {}
func (NickInUseError) isProtocolEvent()           {}
func (NotOperatorError) isProtocolEvent()         {}
func (OperFailedError) isProtocolEvent()          {}
func (ChanOpRequiredError) isProtocolEvent()      {}
func (UnknownModeFlagError) isProtocolEvent()     {}
func (MissingModeParamError) isProtocolEvent()    {}
func (ChannelKeyMismatchError) isProtocolEvent()  {}
func (ChannelInviteOnlyError) isProtocolEvent()   {}
func (ChannelFullError) isProtocolEvent()         {}
func (CannotSendToChannelError) isProtocolEvent() {}
func (UnknownCommandError) isProtocolEvent()      {}
func (UnknownConfigKeyError) isProtocolEvent()    {}
func (InvalidDurationError) isProtocolEvent()     {}
func (UnsupportedModelError) isProtocolEvent()    {}

func (UnknownNickError) domainEvent()         {}
func (NoSuchChannelError) domainEvent()       {}
func (NickInUseError) domainEvent()           {}
func (NotOperatorError) domainEvent()         {}
func (OperFailedError) domainEvent()          {}
func (ChanOpRequiredError) domainEvent()      {}
func (UnknownModeFlagError) domainEvent()     {}
func (MissingModeParamError) domainEvent()    {}
func (ChannelKeyMismatchError) domainEvent()  {}
func (ChannelInviteOnlyError) domainEvent()   {}
func (ChannelFullError) domainEvent()         {}
func (CannotSendToChannelError) domainEvent() {}
func (UnknownCommandError) domainEvent()      {}
func (UnknownConfigKeyError) domainEvent()    {}
func (InvalidDurationError) domainEvent()     {}
func (UnsupportedModelError) domainEvent()    {}

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
	At      time.Time
}

// Error makes [NotOperatorError] satisfy the `error` interface.
// The message follows the IRC numeric-reply convention.
func (e NotOperatorError) Error() string {
	if e.Command == "" {
		return "permission denied: not an operator"
	}

	return fmt.Sprintf("permission denied: %s requires operator privileges", e.Command)
}

// OperFailedError reports that an `OPER` attempt failed the
// session's authenticator (RFC 2812 numeric 464 ERR_PASSWDMISMATCH).
// The authenticator decides what counts as a match; this type
// carries no detail beyond the rejection itself.
type OperFailedError struct {
	At time.Time
}

// Error makes [OperFailedError] satisfy the `error` interface.
func (OperFailedError) Error() string {
	return "OPER rejected: invalid credentials"
}

// ChanOpRequiredError refuses a channel-op-gated command when the
// issuing client lacks `@` in the target channel (RFC 2812 numeric
// 482 ERR_CHANOPRIVSNEEDED).
type ChanOpRequiredError struct {
	Command string
	Channel ChannelName
	At      time.Time
}

func (e ChanOpRequiredError) Error() string {
	return fmt.Sprintf("%s requires channel operator in %s", e.Command, e.Channel)
}

// UnknownModeFlagError reports a `MODE` flag the dispatcher does
// not recognise (RFC 2812 numeric 472 ERR_UNKNOWNMODE).
type UnknownModeFlagError struct {
	Flag Mode
	At   time.Time
}

func (e UnknownModeFlagError) Error() string {
	return fmt.Sprintf("unknown mode flag %q", rune(e.Flag))
}

// MissingModeParamError reports a parametric `MODE` change without
// its required argument: `+o` / `+v` without a target nick, `+l`
// on add without a positive integer, `+k` on add without a key
// (analogue of RFC 2812 numeric 461 ERR_NEEDMOREPARAMS for the
// MODE form).
type MissingModeParamError struct {
	Flag Mode
	At   time.Time
}

func (e MissingModeParamError) Error() string {
	return fmt.Sprintf("mode %q is missing its parameter", rune(e.Flag))
}

// ChannelKeyMismatchError refuses a JOIN against a `+k` channel
// when the supplied key doesn't match (RFC 2812 numeric 475
// ERR_BADCHANNELKEY).
type ChannelKeyMismatchError struct {
	Channel ChannelName
	At      time.Time
}

func (e ChannelKeyMismatchError) Error() string {
	return fmt.Sprintf("cannot join %s: bad channel key", e.Channel)
}

// ChannelInviteOnlyError refuses a JOIN against a `+i` channel
// when the joiner's nick isn't in the channel's pending invite
// list (RFC 2812 numeric 473 ERR_INVITEONLYCHAN).
type ChannelInviteOnlyError struct {
	Channel ChannelName
	At      time.Time
}

func (e ChannelInviteOnlyError) Error() string {
	return fmt.Sprintf("cannot join %s: invite-only channel", e.Channel)
}

// ChannelFullError refuses a JOIN against a `+l` channel when the
// member count is already at the limit (RFC 2812 numeric 471
// ERR_CHANNELISFULL).
type ChannelFullError struct {
	Channel ChannelName
	At      time.Time
}

func (e ChannelFullError) Error() string {
	return fmt.Sprintf("cannot join %s: channel is full", e.Channel)
}

// CannotSendToChannelError refuses a PRIVMSG / Action against a
// channel mode that forbids it (RFC 2812 numeric 404
// ERR_CANNOTSENDTOCHAN). `Reason` distinguishes which mode
// triggered the refusal — moderated (`+m`), no-external (`+n`),
// or quiet (`+q`).
type CannotSendToChannelError struct {
	Channel ChannelName
	Reason  SendBlockReason
	At      time.Time
}

func (e CannotSendToChannelError) Error() string {
	return fmt.Sprintf("cannot send to %s: %s", e.Channel, e.Reason)
}

// SendBlockReason names the channel mode that caused
// [CannotSendToChannelError]. The renderer reads this rather than
// parsing a free-form string out of the error message.
type SendBlockReason int

const (
	// SendBlockModerated names `+m`: only voice/op may speak.
	SendBlockModerated SendBlockReason = iota + 1
	// SendBlockNoExternal names `+n`: sender must be a member.
	SendBlockNoExternal
	// SendBlockQuiet names `+q`: only op may speak.
	SendBlockQuiet
)

func (r SendBlockReason) String() string {
	switch r {
	case SendBlockModerated:
		return "channel is moderated (+m)"
	case SendBlockNoExternal:
		return "no external messages (+n)"
	case SendBlockQuiet:
		return "channel is quiet (+q)"
	}
	return "blocked"
}
