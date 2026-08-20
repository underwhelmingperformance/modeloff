package screens

import (
	"fmt"
	"log/slog"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
)

// dmWindowResolvedMsg carries the counterpart handle a held-back DM
// window was waiting on. `window` is the window key the events were
// held under, and `at` the time of the first of them, which the
// window takes as its creation time. A nil `counterpart` means the
// lookup failed and the held events are dropped: without the
// instance there is no window to render them in.
type dmWindowResolvedMsg struct {
	window      domain.ChannelName
	counterpart *domain.Instance
	at          time.Time
}

// dmWindowsRestoredMsg carries the counterparts of the DM windows
// the user had open when the process last ran, resolved from the
// client-owned record the user-client keeps. `landing` is the window
// the user left open, read in the same command so the Update
// goroutine waits on no store read; it names one of these windows
// when that is where the user left off, and something else when the
// user left off in a channel.
type dmWindowsRestoredMsg struct {
	counterparts []*domain.Instance
	landing      domain.ChannelName
}

// openDMWindow puts a DM window in the sidebar cache and returns the
// commands that follow from opening one: the sidebar insert, and the
// write that records the window as open so the next run reopens it.
// Both are skipped for a window that is already open, which is the
// case every caller has: `/query` on a window in view, a second line
// from a counterpart who already has one.
//
// A restore goes through here too, and records what it just read.
// The write is idempotent, and one path that both opens a window and
// records it is what keeps the record equal to the set of open
// windows however the window came to be open.
func (s ChatScreen) openDMWindow(dm *domain.DMWindow) (*Window, tea.Cmd) {
	if w, open := s.windowByName(dm.Name()); open {
		return w, nil
	}

	w := newWindow(dm)
	s.channels.Insert(w)

	return w, tea.Batch(
		msgCmd(components.ChannelAddedMsg{Channel: dm}),
		s.recordDMWindowCmd(dm.Name()),
	)
}

// recordDMWindowCmd records a DM window in the client-owned set the
// user-client keeps, so a later run reopens it. Best-effort: the
// window is already open on screen, and a failed write costs a
// window the next run does not restore, which is worth a log line
// and nothing more.
func (s ChatScreen) recordDMWindowCmd(name domain.ChannelName) tea.Cmd {
	return func() tea.Msg {
		ctx := s.baseContext()

		if err := s.user.OpenDMWindow(ctx, domain.InstanceID(name)); err != nil {
			slog.Default().WarnContext(ctx, "record open dm window",
				"component", "ui",
				"screen", "chat",
				"window", name,
				"error", err,
			)
		}

		return nil
	}
}

// forgetDMWindowCmd is [ChatScreen.recordDMWindowCmd]'s counterpart,
// dropping a closed window from the set the next run reopens.
func (s ChatScreen) forgetDMWindowCmd(name domain.ChannelName) tea.Cmd {
	return func() tea.Msg {
		ctx := s.baseContext()

		if err := s.user.CloseDMWindow(ctx, domain.InstanceID(name)); err != nil {
			slog.Default().WarnContext(ctx, "forget closed dm window",
				"component", "ui",
				"screen", "chat",
				"window", name,
				"error", err,
			)
		}

		return nil
	}
}

// resolveDMWindow looks up the counterpart a held-back DM window is
// waiting on. The window key is the counterpart's instance id and
// the answer is the canonical handle for it, which is what the
// window, the nick it renders under, and the actor-scoped event
// routing all compare against.
func (s ChatScreen) resolveDMWindow(name domain.ChannelName, at time.Time) tea.Cmd {
	return func() tea.Msg {
		ctx := s.baseContext()

		counterpart, err := s.sess.ResolveInstanceByID(ctx, domain.InstanceID(name))
		if err != nil {
			slog.Default().WarnContext(ctx, "resolve dm counterpart",
				"component", "ui",
				"screen", "chat",
				"window", name,
				"error", err,
			)
		}

		return dmWindowResolvedMsg{window: name, counterpart: counterpart, at: at}
	}
}

// handleDMWindowResolved opens the window the held-back lines were
// waiting for and files them in it, in the order they arrived. The
// unread count is re-read here because the lines reached the badge
// path before the window existed, and [ChatScreen.deliverUnreadCount]
// drops a count for a window it cannot find.
//
// A counterpart the lookup could not answer for leaves the lines
// nowhere to go. They are discarded along with the queue, which is
// also what frees the key for a later attempt, and the discard is
// reported.
func (s ChatScreen) handleDMWindowResolved(msg dmWindowResolvedMsg) (ChatScreen, tea.Cmd) {
	held := s.pendingDM[msg.window]
	delete(s.pendingDM, msg.window)

	if msg.counterpart == nil {
		// A DM window is built around the counterpart's handle, and
		// this id names no instance the store still holds, which
		// happens when a KILL deletes it in the moment between the
		// message arriving and this lookup. The discard is narrated
		// in `&modeloff`, the home for the client's own diagnostics,
		// so no line the server delivered goes missing in silence.
		return s, s.logAndShowOn(domain.StatusChannelName, domain.SystemNotice{
			Target: domain.StatusChannelName,
			Text:   fmt.Sprintf("Dropped %d line(s) from %s: no such instance.", len(held), msg.window),
			At:     time.Now(),
		})
	}

	w, opened := s.openDMWindow(domain.NewDMWindow(msg.counterpart, msg.at))

	// The held lines are older than anything already in the window:
	// they were buffered before it existed, and one goroutine drains
	// the bus. A `/query` for the same counterpart while the lookup
	// was running is what puts anything there to go in front of.
	w.Scrollback.Prepend(held)

	return s, tea.Batch(opened, s.unreadCountCmd(msg.window, s.mentionsUser(held), w.Visits))
}

// mentionsUser reports whether any of the given lines carries a
// highlight word.
func (s ChatScreen) mentionsUser(events []domain.Event) bool {
	for _, evt := range events {
		msg, ok := evt.(domain.Message)
		if !ok {
			continue
		}

		if s.isHighlight(msg) {
			return true
		}
	}

	return false
}

// restoreDMWindows reads the DM windows the user had open when the
// process last ran and resolves each counterpart to its instance
// handle. A channel returns through autojoin, which announces itself
// with a JOIN; nothing on the wire brings a DM window back, so this
// is what puts the user's open conversations back in the sidebar.
//
// The read runs off the Update goroutine, so a slow store never
// delays the first frame. A counterpart the store no longer holds is
// skipped: the instance is gone and there is no window to build.
func (s ChatScreen) restoreDMWindows() tea.Cmd {
	return func() tea.Msg {
		ctx := s.baseContext()

		open, err := s.user.DMWindows(ctx)
		if err != nil {
			slog.Default().WarnContext(ctx, "list open dm windows",
				"component", "ui",
				"screen", "chat",
				"error", err,
			)

			return nil
		}

		counterparts := make([]*domain.Instance, 0, len(open))

		for _, id := range open {
			counterpart, err := s.sess.ResolveInstanceByID(ctx, id)
			if err != nil || counterpart == nil {
				slog.Default().WarnContext(ctx, "resolve dm counterpart",
					"component", "ui",
					"screen", "chat",
					"window", id,
					"error", err,
				)

				continue
			}

			counterparts = append(counterparts, counterpart)
		}

		landing, _ := s.restoredChannel()

		return dmWindowsRestoredMsg{counterparts: counterparts, landing: landing}
	}
}

// handleDMWindowsRestored reopens the recorded DM windows in the
// sidebar. Focus stays where the bootstrap put it, except for the
// window the user left open last time: that is their standing
// preference, and `bootstrapFromSession` could not land on it
// because a DM window is not one of the channels it reads.
//
// The commands run in sequence, so the sidebar has the entry before
// the focus event asks it to mark that entry active.
func (s ChatScreen) handleDMWindowsRestored(msg dmWindowsRestoredMsg) (ChatScreen, tea.Cmd) {
	var cmds []tea.Cmd

	for _, counterpart := range msg.counterparts {
		dm := domain.NewDMWindow(counterpart, s.sess.ConnectedAt())

		_, opened := s.openDMWindow(dm)
		cmds = append(cmds, opened)

		if dm.Name() == msg.landing {
			cmds = append(cmds, msgCmd(chatcmd.ChannelFocusMsg{Channel: dm.Name(), At: time.Now()}))
		}
	}

	return s, tea.Sequence(cmds...)
}

// handleDMClosedMsg closes a query window on the user's `/close`.
// The window, its scrollback and its sidebar entry go, and the
// record of it goes with them. The conversation is untouched: it
// lives in the event log, and messaging the counterpart again opens
// a window on it.
func (s ChatScreen) handleDMClosedMsg(msg chatcmd.DMClosedMsg) (ChatScreen, tea.Cmd) {
	if _, open := s.windowByName(msg.Window); !open {
		return s, nil
	}

	s, closed := s.closeWindow(msg.Window, msg.At)

	return s, tea.Batch(closed, s.forgetDMWindowCmd(msg.Window))
}

// handleDMOpenedMsg materialises the DM window in the sidebar
// (insert is idempotent), optionally focus-switches, and
// optionally sends a trailing body. `/query` sets `Focus`;
// `/msg` does not.
func (s ChatScreen) handleDMOpenedMsg(msg chatcmd.DMOpenedMsg) (ChatScreen, tea.Cmd) {
	dm := domain.NewDMWindow(msg.Counterpart, msg.At)
	name := dm.Name()

	_, opened := s.openDMWindow(dm)

	cmds := []tea.Cmd{opened}

	if msg.Focus {
		var rebind tea.Cmd
		s, rebind = s.focus(name)

		cmds = append(cmds, rebind)
		cmds = append(cmds, msgCmd(components.SetPlaceholderMsg{}))
		cmds = append(cmds, s.setChannelCmd())
		cmds = append(cmds, msgCmd(components.ChannelActiveMsg{Channel: name}))
		cmds = append(cmds, s.persistLastChannel(name))
		cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: domain.MemberList{}}))
	}

	if msg.Body != "" {
		cmds = append(cmds, s.sendMessageCmd("msg", name, msg.Body))
	}

	return s, tea.Sequence(cmds...)
}

// activeDMWith returns the open DM whose counterpart is `actor`,
// if any.
func (s ChatScreen) activeDMWith(actor *domain.Instance) (*domain.DMWindow, bool) {
	if actor == nil {
		return nil, false
	}

	for w := range s.channels.All() {
		dm, ok := w.Window.(*domain.DMWindow)
		if !ok {
			continue
		}

		if dm.Counterpart == actor {
			return dm, true
		}
	}

	return nil, false
}
