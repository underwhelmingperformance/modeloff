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
