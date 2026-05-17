package screens

import (
	"github.com/laney/modeloff/internal/domain"
)

// Window is the chat-screen's per-window state container. It wraps
// a [domain.Window] (the addressable identity — status, channel,
// or DM) and adds the chat-screen-side fields that used to live
// in parallel `map[ChannelName]X` slots on `ChatScreen`. Sidebar
// indicators (`Unread`, `Mentions`, `Activity`) and the scrollback
// history live here so a window's view of itself is a single
// value, not a fan-out across maps that have to be kept in sync.
type Window struct {
	domain.Window

	// Scrollback is the per-window in-memory event history the
	// message list renders. The chat-screen appends here from
	// the protocol bus; the message list reads through a closure
	// the chat-screen passes at construction.
	Scrollback []domain.StoredEvent

	// Unread is the count of messages addressed at this window
	// the user has not yet seen. The sidebar surfaces it as a
	// `(n)` suffix and a bold style. Cleared on focus.
	Unread int

	// Mentions is true when at least one unread message body
	// contained a highlight word. The sidebar surfaces it as
	// magenta-bold (overrides plain bold). Cleared on focus.
	Mentions bool

	// Activity is true when at least one unseen actor-scoped
	// event (a peer's QUIT or NICK rename) has appended to the
	// window's scrollback. The sidebar surfaces it as italic-dim
	// — the lowest-priority indicator, only shown when `Unread`
	// and `Mentions` are clear. Cleared on focus.
	Activity bool
}

// newWindow wraps the given domain window. The returned `*Window`
// is the canonical chat-screen handle for that window's lifetime;
// callers store it in `Session.channels` and mutate its fields in
// place from `Update`.
func newWindow(w domain.Window) *Window {
	return &Window{Window: w}
}

// Less implements [set.Lesser] for `*Window`, delegating to the
// underlying [domain.Window.Less].
func (w *Window) Less(other *Window) bool {
	return w.Window.Less(other.Window)
}

// windowKey returns a placeholder `*Window` suitable only for
// lookup in a `*set.Sorted[*Window]` whose comparator reads
// `Name()` and `Kind()` from the embedded [domain.Window]. The
// returned value's per-kind state and chat-screen fields are
// zero — it must not be used as a real window. Mirrors
// [domain.WindowKey].
func windowKey(name domain.ChannelName) *Window {
	return &Window{Window: domain.WindowKey(name)}
}
