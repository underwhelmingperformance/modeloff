package domain

import "fmt"

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
// ChannelUsageHint rather than a hard error.
type StatusChannelGuardError struct {
	Command string
	Hint    string
}

func (e StatusChannelGuardError) Error() string {
	return e.Hint
}
