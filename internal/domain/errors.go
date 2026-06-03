package domain

import (
	"errors"
	"fmt"
	"time"
)

// ErrNotChannelWindow is returned by callers that expected a
// `*ChannelWindow` but received a different concrete `Window`
// kind (status or DM). It is a sentinel rather than a typed
// struct because the carrying error already names the offending
// row; this just lets callers branch on the "wrong kind" axis
// with `errors.Is`.
var ErrNotChannelWindow = errors.New("not a channel window")

// UnknownCommandError indicates the user entered an unrecognised
// slash command. Wire-shape equivalent of RFC 2812 numeric 421
// (ERR_UNKNOWNCOMMAND).
type UnknownCommandError struct {
	Name string
	At   time.Time
}

func (e UnknownCommandError) Error() string {
	return fmt.Sprintf("unknown command: /%s", e.Name)
}

// UnknownNickError indicates a nick could not be found. Wire-shape
// equivalent of RFC 2812 numeric 401 (ERR_NOSUCHNICK).
type UnknownNickError struct {
	Nick Nick
	At   time.Time
}

func (e UnknownNickError) Error() string {
	return fmt.Sprintf("no such nick: %s", e.Nick)
}

// NoSuchChannelError indicates a channel name could not be
// resolved. Wire-shape equivalent of RFC 2812 numeric 403
// (ERR_NOSUCHCHANNEL).
type NoSuchChannelError struct {
	Channel ChannelName
	At      time.Time
}

func (e NoSuchChannelError) Error() string {
	return fmt.Sprintf("no such channel: %s", e.Channel)
}

// NotOnChannelError refuses a channel command issued by an actor
// that is not a member of the target channel (RFC 2812 numeric 442
// ERR_NOTONCHANNEL). PART is the issuing command today; `Command`
// carries it so renderers and tool-result formatters can name the
// rejected call without reparsing the error string.
type NotOnChannelError struct {
	Channel ChannelName
	Command string
	At      time.Time
}

func (e NotOnChannelError) Error() string {
	return fmt.Sprintf("%s: you are not on %s", e.Command, e.Channel)
}

// UserNotInChannelError refuses a command whose target nick is not
// a member of the channel it names (RFC 2812 numeric 441
// ERR_USERNOTINCHANNEL). KICK is the issuing command today.
type UserNotInChannelError struct {
	Nick    Nick
	Channel ChannelName
	Command string
	At      time.Time
}

func (e UserNotInChannelError) Error() string {
	return fmt.Sprintf("%s: %s is not on %s", e.Command, e.Nick, e.Channel)
}

// UserOnChannelError refuses an INVITE whose target nick is already
// a member of the channel (RFC 2812 numeric 443 ERR_USERONCHANNEL).
type UserOnChannelError struct {
	Nick    Nick
	Channel ChannelName
	At      time.Time
}

func (e UserOnChannelError) Error() string {
	return fmt.Sprintf("%s is already on %s", e.Nick, e.Channel)
}

// UnknownConfigKeyError indicates an unrecognised configuration key.
type UnknownConfigKeyError struct {
	Key string
	At  time.Time
}

func (e UnknownConfigKeyError) Error() string {
	return fmt.Sprintf("unknown config key: %s", e.Key)
}

// InvalidDurationError indicates a duration string could not be
// parsed.
type InvalidDurationError struct {
	Input string
	Err   error
	At    time.Time
}

func (e InvalidDurationError) Error() string {
	return fmt.Sprintf("invalid duration %q: %v", e.Input, e.Err)
}

// UnsupportedModelError indicates a model cannot satisfy the app's
// strict response contract.
type UnsupportedModelError struct {
	ModelID ModelID
	At      time.Time
}

func (e UnsupportedModelError) Error() string {
	return fmt.Sprintf("model does not support structured outputs: %s", e.ModelID)
}

// NickInUseError indicates a nickname change was refused because
// the target nick is already held by another instance (a model
// or the user). Wire-shape equivalent of RFC 2812 numeric 433
// (ERR_NICKNAMEINUSE). Carries the conflicting nick so the UI
// can surface it without re-parsing the error string.
type NickInUseError struct {
	Nick Nick
	At   time.Time
}

func (e NickInUseError) Error() string {
	return fmt.Sprintf("nick %q is already in use", string(e.Nick))
}

// MissingDMCounterpartError indicates a stored DM row references
// an `InstanceID` that no longer resolves to an instance — the
// counterpart's row was deleted but the DM row outlived it. The
// `InstanceID` field carries the dangling reference so the
// store can drop the row and log it.
type MissingDMCounterpartError struct {
	InstanceID InstanceID
}

func (e MissingDMCounterpartError) Error() string {
	return fmt.Sprintf("dm window %q: counterpart instance has no backing row", string(e.InstanceID))
}

// UnknownChannelKindError indicates a stored row carries a
// `ChannelKind` outside the known set (status / channel / dm).
// This is unexpected on a healthy database — the only way to hit
// it is a forward-incompatible row from a newer schema or a
// corrupted on-disk value.
type UnknownChannelKindError struct {
	Kind ChannelKind
}

func (e UnknownChannelKindError) Error() string {
	return fmt.Sprintf("unknown channel kind: %d", int(e.Kind))
}
