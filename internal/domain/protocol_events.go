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

// Wire-shaped events delivered on the protocol bus: channel activity,
// issuer replies, and the protocol-only `UserModeChange`,
// `ListEnd` and `JoinedChannel`.
func (Message) isProtocolEvent()           {}
func (Join) isProtocolEvent()              {}
func (Part) isProtocolEvent()              {}
func (Quit) isProtocolEvent()              {}
func (TopicChange) isProtocolEvent()       {}
func (ChannelModeChange) isProtocolEvent() {}
func (UserModeChange) isProtocolEvent()    {}
func (Invited) isProtocolEvent()           {}
func (Kicked) isProtocolEvent()            {}
func (NickChange) isProtocolEvent()        {}
func (TopicInfo) isProtocolEvent()         {}
func (Whois) isProtocolEvent()             {}
func (ListReply) isProtocolEvent()         {}
func (ListEnd) isProtocolEvent()           {}
func (JoinedChannel) isProtocolEvent()     {}
func (CommandError) isProtocolEvent()      {}
func (SystemNotice) isProtocolEvent()      {}
func (PersonasList) isProtocolEvent()      {}

// Pure-live events. Order matches the seal block in `events.go`.
func (PokeEvent) isProtocolEvent()             {}
func (ModelDispatchStarted) isProtocolEvent()  {}
func (ModelDispatchDone) isProtocolEvent()     {}
func (NamesReplyEvent) isProtocolEvent()       {}
func (NamesEnd) isProtocolEvent()              {}
func (Welcome) isProtocolEvent()               {}
func (Reconnected) isProtocolEvent()           {}
func (ModelUnavailableError) isProtocolEvent() {}

// Typed errors that double as protocol events. They satisfy both
// the `error` interface (for `errors.As` extraction at the
// emission boundary) and the protocol-event seal (so the session
// can `emit` them like any other wire event).
func (UnknownNickError) isProtocolEvent()          {}
func (NoSuchChannelError) isProtocolEvent()        {}
func (NickInUseError) isProtocolEvent()            {}
func (NotOnChannelError) isProtocolEvent()         {}
func (UserNotInChannelError) isProtocolEvent()     {}
func (UserOnChannelError) isProtocolEvent()        {}
func (NotOperatorError) isProtocolEvent()          {}
func (OperFailedError) isProtocolEvent()           {}
func (ChanOpRequiredError) isProtocolEvent()       {}
func (UnknownModeFlagError) isProtocolEvent()      {}
func (MissingModeParamError) isProtocolEvent()     {}
func (ChannelKeyMismatchError) isProtocolEvent()   {}
func (ChannelInviteOnlyError) isProtocolEvent()    {}
func (ChannelFullError) isProtocolEvent()          {}
func (TooManyJoinTargetsError) isProtocolEvent()   {}
func (ErroneousChannelNameError) isProtocolEvent() {}
func (ErroneousNicknameError) isProtocolEvent()    {}
func (ErroneousPersonaError) isProtocolEvent()     {}
func (ErroneousTopicError) isProtocolEvent()       {}
func (CannotSendToChannelError) isProtocolEvent()  {}
func (UnknownCommandError) isProtocolEvent()       {}
func (UnknownConfigKeyError) isProtocolEvent()     {}
func (InvalidDurationError) isProtocolEvent()      {}
func (UnsupportedModelError) isProtocolEvent()     {}

func (UnknownNickError) domainEvent()          {}
func (NoSuchChannelError) domainEvent()        {}
func (NickInUseError) domainEvent()            {}
func (NotOnChannelError) domainEvent()         {}
func (UserNotInChannelError) domainEvent()     {}
func (UserOnChannelError) domainEvent()        {}
func (NotOperatorError) domainEvent()          {}
func (OperFailedError) domainEvent()           {}
func (ChanOpRequiredError) domainEvent()       {}
func (UnknownModeFlagError) domainEvent()      {}
func (MissingModeParamError) domainEvent()     {}
func (ChannelKeyMismatchError) domainEvent()   {}
func (ChannelInviteOnlyError) domainEvent()    {}
func (ChannelFullError) domainEvent()          {}
func (TooManyJoinTargetsError) domainEvent()   {}
func (ErroneousChannelNameError) domainEvent() {}
func (ErroneousNicknameError) domainEvent()    {}
func (ErroneousPersonaError) domainEvent()     {}
func (ErroneousTopicError) domainEvent()       {}
func (CannotSendToChannelError) domainEvent()  {}
func (UnknownCommandError) domainEvent()       {}
func (UnknownConfigKeyError) domainEvent()     {}
func (InvalidDurationError) domainEvent()      {}
func (UnsupportedModelError) domainEvent()     {}

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

// TooManyJoinTargetsError refuses a JOIN naming more channels than
// [protocol.MaxJoinTargets] allows in one command (RFC 2812
// ISUPPORT TARGMAX). The dispatcher checks the whole list before
// joining any of it, so a client that names too many channels gets
// one clear refusal and no partial join.
type TooManyJoinTargetsError struct {
	Requested int
	Max       int
	At        time.Time
}

func (e TooManyJoinTargetsError) Error() string {
	return fmt.Sprintf("JOIN names %d channels, more than the %d a single command may name", e.Requested, e.Max)
}

// ErroneousChannelNameError refuses a channel name that fails the
// RFC 2812 §1.3 grammar (numeric 479 ERR_BADCHANNAME). `Reason`
// carries which part of the grammar it failed, so renderers and
// tool-result formatters can say so without reparsing the message.
// The dispatcher checks the name itself: it does not trust a client
// to have refused the value first.
type ErroneousChannelNameError struct {
	Channel ChannelName
	Reason  ChannelNameRejection
	At      time.Time
}

func (e ErroneousChannelNameError) Error() string {
	return fmt.Sprintf("%q is not a valid channel name: %s", e.Channel, e.Reason)
}

// ErroneousPersonaError refuses a persona description that fails
// [ValidatePersona]. A persona is app-supplied instruction, carried
// in the model's system prompt, so the server checks it where it
// enters and does not trust the caller to have checked it first.
// `Reason` carries which bound it failed, so renderers and
// tool-result formatters can say so without reparsing the message.
type ErroneousPersonaError struct {
	Reason PersonaRejection
	At     time.Time
}

func (e ErroneousPersonaError) Error() string {
	return fmt.Sprintf("persona is not allowed: %s", e.Reason)
}

// ErroneousNicknameError refuses a nick that fails the RFC 2812
// §2.3.1 grammar (numeric 432 ERR_ERRONEUSNICKNAME). It is the
// separate answer from [NickInUseError] (433): 432 says the nick
// could never be taken by anyone, 433 says this nick is taken now
// and another client may free it.
type ErroneousNicknameError struct {
	Nick   Nick
	Reason NickRejection
	At     time.Time
}

func (e ErroneousNicknameError) Error() string {
	return fmt.Sprintf("nick %q is not allowed: %s", string(e.Nick), e.Reason)
}

// ErroneousTopicError refuses a topic body that fails
// [ValidateTopic]. RFC 1459 and RFC 2812 place no bound on a topic;
// TOPICLEN is the convention modern ircds advertise via ISUPPORT, and
// this server enforces it because a channel's topic is repeated into
// every dispatch turn's prompt for that channel. `Reason` carries
// which bound it failed, so renderers and tool-result formatters can
// say so without reparsing the message.
type ErroneousTopicError struct {
	Channel ChannelName
	Reason  TopicRejection
	At      time.Time
}

func (e ErroneousTopicError) Error() string {
	return fmt.Sprintf("topic for %s is not allowed: %s", e.Channel, e.Reason)
}

// CannotSendToChannelError refuses a PRIVMSG / Action against a
// channel mode that forbids it (RFC 2812 numeric 404
// ERR_CANNOTSENDTOCHAN). `Reason` distinguishes which mode
// triggered the refusal — moderated (`+m`), no-external (`+n`),
// quiet (`+q`), or the channel's flood limit (`+f`).
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
	// SendBlockFlood names `+f`: the channel has already carried as
	// many messages this flood window as its limit allows.
	SendBlockFlood
)

func (r SendBlockReason) String() string {
	switch r {
	case SendBlockModerated:
		return "channel is moderated (+m)"
	case SendBlockNoExternal:
		return "no external messages (+n)"
	case SendBlockQuiet:
		return "channel is quiet (+q)"
	case SendBlockFlood:
		return "channel message limit reached (+f)"
	}
	return "blocked"
}
