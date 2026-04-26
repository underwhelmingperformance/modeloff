package domain

import "time"

// Window is the addressable-by-name behaviour shared by every kind
// of chat target the user can switch into: the per-session status
// window, real IRC channels, and DM streams. The set of
// implementations is fixed (`*StatusWindow`, `*ChannelWindow`,
// `*DMWindow`) and lives in this package; per-kind state lives on
// the matching concrete type so invariants like "modes don't apply
// to status" and "DMs don't have a member list" are compile-time
// facts rather than runtime kind-checks.
//
// Code that addresses a window only by name (rendering, scrollback,
// `last_read`, focus) operates against this interface. Code that
// updates per-kind state (member list, topic, modes) downcasts to
// the matching concrete type at the receiving handler — by which
// point session-side propagation has already guaranteed the cast
// will succeed.
type Window interface {
	// Name returns the addressable name of the window. For
	// channels this is the `#`-prefixed name; for DMs it is the
	// counterpart's nick; for the status window it is the
	// reserved [StatusChannelName].
	Name() ChannelName

	// Created returns the time the window was first opened.
	Created() time.Time

	// Kind reports which leaf concrete type this window is. The
	// sidebar uses this for its pin-status-first sort and the
	// system-notice render branch keys off it; the rest of the
	// codebase prefers a typed downcast.
	Kind() ChannelKind

	// DisplayName returns the window name formatted for display.
	// Channels keep their `#` prefix; DMs prefix the nick with
	// `@`; the status window renders as its reserved name.
	DisplayName() string
}
