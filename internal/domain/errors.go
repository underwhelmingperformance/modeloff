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
// slash command.
type UnknownCommandError struct {
	Name string
}

func (e UnknownCommandError) Error() string {
	return fmt.Sprintf("unknown command: /%s", e.Name)
}

// UnknownNickError indicates a nick could not be found.
type UnknownNickError struct {
	Nick Nick
}

func (e UnknownNickError) Error() string {
	return fmt.Sprintf("no such nick: %s", e.Nick)
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
// targeted the per-session status channel (`&modeloff`). It carries a
// command tag and a human-friendly hint that the UI surfaces as a
// UsageHint rather than a hard error.
type StatusChannelGuardError struct {
	Command string
	Hint    string
}

func (e StatusChannelGuardError) Error() string {
	return e.Hint
}

// NickInUseError indicates a nickname change was refused because the
// target nick is already held by another instance (a model or the
// user). Carries the conflicting nick so the UI can surface it
// without re-parsing the error string.
type NickInUseError struct {
	Nick Nick
}

func (e NickInUseError) Error() string {
	return fmt.Sprintf("nick %q is already in use", string(e.Nick))
}

// MissingDMCounterpartError indicates a stored DM row references a
// counterpart nick that no longer resolves to an instance — the
// counterpart's row was deleted but the DM row outlived it. The
// `Nick` field carries the dangling counterpart so the store can
// drop the row and log it.
type MissingDMCounterpartError struct {
	Nick Nick
}

func (e MissingDMCounterpartError) Error() string {
	return fmt.Sprintf("dm window %q: counterpart nick has no backing instance", string(e.Nick))
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
