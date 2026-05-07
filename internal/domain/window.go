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

	// Less defines the sidebar / sorted-set ordering: status
	// pinned at the top, then channels, then DMs, alphabetical
	// within each group.
	Less(other Window) bool
}

// windowKindRank maps a [ChannelKind] to its sort position for
// sidebar ordering — status pinned at the top, then channels,
// then DMs.
func windowKindRank(kind ChannelKind) int {
	switch kind {
	case KindStatus:
		return 0
	case KindChannel:
		return 1
	case KindDM:
		return 2
	}

	return 3
}

// windowLess is the shared `Window.Less` body used by every
// concrete implementer.
func windowLess(a, b Window) bool {
	if a.Kind() != b.Kind() {
		return windowKindRank(a.Kind()) < windowKindRank(b.Kind())
	}

	return a.Name() < b.Name()
}

// ChannelDirectoryEntry is one row in the result of `/list`. It
// holds just the fields a `ListReply` needs to be assembled
// from; clients construct the persistable event around their
// own `At` timestamp.
type ChannelDirectoryEntry struct {
	Channel ChannelName
	Members int
	Topic   string
}

// WindowKey builds a placeholder `Window` suitable only for
// keyed lookup in a sorted set whose comparator reads
// `Name()` and `Kind()`. The returned value carries no per-kind
// state — it must not be used as a real window. The DM case
// returns a `*DMWindow` whose `Counterpart` is nil; that is safe
// for lookup because the comparator does not touch it, and a
// caller that mistakes a key for a stored value will fail fast
// when it tries to read the counterpart.
func WindowKey(name ChannelName) Window {
	switch InferChannelKind(name) {
	case KindStatus:
		return &StatusWindow{}
	case KindChannel:
		return &ChannelWindow{name: name}
	case KindDM:
		return &DMWindow{name: name}
	}

	return nil
}
