package config

import (
	"fmt"
	"strings"
)

// ReflectionMode controls whether persona reflection is disabled, evaluated
// without mutation, or allowed to commit validated proposals.
//
// The zero value is [ReflectionUnset], which says the setting is absent.
// [ReflectionMode.Resolved] answers what an absent setting runs as.
type ReflectionMode string

const (
	// ReflectionUnset is the zero value. It is not a mode: it is what an
	// installation that has never written the setting holds.
	ReflectionUnset ReflectionMode = ""
	// ReflectionDisabled runs no reflection.
	ReflectionDisabled ReflectionMode = "disabled"
	// ReflectionShadow records validated proposals without changing state.
	ReflectionShadow ReflectionMode = "shadow"
	// ReflectionActive commits validated proposals.
	ReflectionActive ReflectionMode = "active"
)

// DefaultReflectionMode is the mode an unconfigured installation runs in.
// An instance that never reflects keeps the description it was created
// with, which is the whole of what the persona lineage would hold.
const DefaultReflectionMode = ReflectionActive

// InvalidReflectionModeError reports a reflection mode outside the closed set
// accepted by [ParseReflectionMode].
type InvalidReflectionModeError struct {
	Mode string
}

func (e *InvalidReflectionModeError) Error() string {
	return fmt.Sprintf("invalid reflection mode %q", e.Mode)
}

// ParseReflectionMode reads a persisted or command-supplied mode. An empty
// value gives [ReflectionUnset], because a setting nobody has written is
// absent. A value outside the closed set is refused, and the mode returned
// alongside the error is [ReflectionUnset], so a caller that warns and
// carries on runs [DefaultReflectionMode].
func ParseReflectionMode(value string) (ReflectionMode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return ReflectionUnset, nil
	case string(ReflectionDisabled):
		return ReflectionDisabled, nil
	case string(ReflectionShadow):
		return ReflectionShadow, nil
	case string(ReflectionActive):
		return ReflectionActive, nil
	default:
		return ReflectionUnset, &InvalidReflectionModeError{Mode: value}
	}
}

// Resolved returns the mode in force. [ReflectionUnset] resolves to
// [DefaultReflectionMode], and every other mode is its own answer.
func (m ReflectionMode) Resolved() ReflectionMode {
	if m == ReflectionUnset {
		return DefaultReflectionMode
	}

	return m
}

// String returns the user-facing spelling of the mode in force, so an unset
// setting reads as the default it runs under.
func (m ReflectionMode) String() string {
	return string(m.Resolved())
}
