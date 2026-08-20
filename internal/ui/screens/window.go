package screens

import (
	"time"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/set"
	"github.com/laney/modeloff/internal/ui/components"
)

// Window is the chat-screen's per-window state container. It wraps
// a [domain.Window] (the addressable identity — status, channel,
// or DM) and carries the chat-screen-side fields beside it:
// sidebar indicators (`Unread`, `Mentions`, `Activity`) and the
// scrollback history. A window's view of itself is a single value
// the sidebar, message list, and event handlers all read through
// the same pointer.
type Window struct {
	domain.Window

	// Scrollback is the per-window in-memory event history the
	// message list renders. The chat-screen appends here from
	// the protocol bus; the message list reads through a closure
	// the chat-screen passes at construction. It holds the
	// renderable base `domain.Event` so it can carry both persisted
	// channel events and the render-only UI feedback (`Help`,
	// `UsageHint`) the chat-screen raises locally.
	//
	// It is bounded at [components.ScrollbackLimit] events: a window
	// the user leaves open all day would otherwise keep every line
	// the session ever put in it.
	//
	// A scrollback holds its events in a ring it shares with any copy
	// of itself, so the window holds the handle rather than the value
	// and [newWindow] is what makes it.
	Scrollback *components.Scrollback

	// Unread is the count of messages addressed at this window
	// the user has not yet seen. The sidebar surfaces it as a
	// `(n)` suffix and a bold style. Cleared on focus.
	Unread int

	// Mentions is true when at least one unread message body
	// contained a highlight word. The sidebar surfaces it as
	// magenta-bold (overrides plain bold). Cleared on focus.
	Mentions bool

	// Visits counts the times the user has focused this window. An
	// unread count is read from the store on its own goroutine and
	// carries the value it was requested against; the handler drops
	// a count whose value is behind, because focusing the window in
	// the meantime both cleared the badge and moved the read cursor
	// past what that count was counting.
	Visits int

	// Activity is true when at least one unseen actor-scoped
	// event (a peer's QUIT or NICK rename) has appended to the
	// window's scrollback. The sidebar surfaces it as italic-dim
	// — the lowest-priority indicator, only shown when `Unread`
	// and `Mentions` are clear. Cleared on focus.
	Activity bool

	// UserTime stamps the user's most recent deliberate
	// interaction with the window: the join that opened it, a
	// focus-changing keystroke, a typed message, a scroll. Focus
	// arbitration uses it to decide whether an incoming
	// [chatcmd.ChannelFocusMsg] should take over the visible area
	// or merely flag activity on the sidebar: newer beats older.
	// Without it, late events from autojoin races could yank the
	// user away from a channel they just navigated to.
	UserTime time.Time
}

// newWindow wraps the given domain window. The returned `*Window`
// is the canonical chat-screen handle for that window's lifetime;
// callers store it in `Session.channels` and mutate its fields in
// place from `Update`.
func newWindow(w domain.Window) *Window {
	return &Window{Window: w, Scrollback: components.NewScrollback()}
}

// Less implements [set.Lesser] for `*Window`, delegating to the
// underlying [domain.Window.Less].
func (w *Window) Less(other *Window) bool {
	return w.Window.Less(other.Window)
}

// visibleWindow names the window the message list renders. The list
// is constructed once, before any window is focused, and reads its
// events through a closure bound at that moment; this record is what
// that closure resolves, and it outlives the chat-screen value the
// closure captured. `Update` and `View` both run on the Bubble Tea
// event-loop goroutine, so one goroutine reads and writes the record
// and it needs no synchronisation.
type visibleWindow struct {
	channels *set.Sorted[*Window]
	name     domain.ChannelName
}

// content returns the visible window and its event history. It is
// the closure the message list calls on every render, and it answers
// with both together so the list can tell which window's events it
// has: [ChatScreen.focus] moves the name during its own Update, and
// the chat view hears about the switch a message later.
func (v *visibleWindow) content() components.WindowContent {
	w, ok := v.channels.Get(windowKey(v.name))
	if !ok {
		return components.WindowContent{Channel: v.name}
	}

	return components.WindowContent{
		Channel:  v.name,
		Events:   w.Scrollback.Events(),
		FirstSeq: w.Scrollback.FirstSeq(),
	}
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
