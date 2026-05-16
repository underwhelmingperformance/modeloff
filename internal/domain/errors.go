package domain

import (
	"errors"
	"fmt"
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
}

func (e UnknownCommandError) Error() string {
	return fmt.Sprintf("unknown command: /%s", e.Name)
}

// UnknownNickError indicates a nick could not be found. Wire-shape
// equivalent of RFC 2812 numeric 401 (ERR_NOSUCHNICK).
type UnknownNickError struct {
	Nick Nick
}

func (e UnknownNickError) Error() string {
	return fmt.Sprintf("no such nick: %s", e.Nick)
}

// NoSuchChannelError indicates a channel name could not be
// resolved. Wire-shape equivalent of RFC 2812 numeric 403
// (ERR_NOSUCHCHANNEL).
type NoSuchChannelError struct {
	Channel ChannelName
}

func (e NoSuchChannelError) Error() string {
	return fmt.Sprintf("no such channel: %s", e.Channel)
}

// UnknownConfigKeyError indicates an unrecognised configuration key.
type UnknownConfigKeyError struct {
	Key string
}

func (e UnknownConfigKeyError) Error() string {
	return fmt.Sprintf("unknown config key: %s", e.Key)
}

// InvalidDurationError indicates a duration string could not be
// parsed.
type InvalidDurationError struct {
	Input string
	Err   error
}

func (e InvalidDurationError) Error() string {
	return fmt.Sprintf("invalid duration %q: %v", e.Input, e.Err)
}

// UnsupportedModelError indicates a model cannot satisfy the app's
// strict response contract.
type UnsupportedModelError struct {
	ModelID ModelID
}

func (e UnsupportedModelError) Error() string {
	return fmt.Sprintf("model does not support structured outputs: %s", e.ModelID)
}

// StatusChannelGuardError indicates a request was refused because it
// targeted the per-session status channel (`&modeloff`). The status
// channel is server-narrated only — it accepts no chat messages,
// invites, kicks, or topic changes. `Hint` is a *user-facing*
// suggestion the chat screen renders as a `UsageHint` (it can name
// slash-command alternatives like `/msg` or `/join`); it is not
// included in `Error()` because that string is also what surfaces
// to model tool callers, who reason in tool-call terms and have no
// notion of slash syntax.
type StatusChannelGuardError struct {
	Command string
	Hint    string
}

func (e StatusChannelGuardError) Error() string {
	return fmt.Sprintf("the status window does not accept %s requests", e.Command)
}

// NickInUseError indicates a nickname change was refused because
// the target nick is already held by another instance (a model
// or the user). Wire-shape equivalent of RFC 2812 numeric 433
// (ERR_NICKNAMEINUSE). Carries the conflicting nick so the UI
// can surface it without re-parsing the error string.
type NickInUseError struct {
	Nick Nick
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
