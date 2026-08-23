package screens

import (
	"fmt"
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
	"github.com/laney/modeloff/internal/userclient"
)

// dmCounterpart carries the identity needed to build a DM window.
type dmCounterpart struct {
	id   domain.InstanceID
	nick domain.Nick
}

func (s ChatScreen) currentDMCounterpart(counterpart dmCounterpart) dmCounterpart {
	nick, err := s.sess.ResolveInstanceByID(s.baseContext(), counterpart.id)
	if err == nil {
		counterpart.nick = nick
	}

	return counterpart
}

// dmWindowResolvedMsg carries the counterpart identity a held-back DM
// window was waiting on. `window` is the window key the events were
// held under, and `at` the time of the first of them, which the
// window takes as its creation time. This fallback handles legacy or
// unidentified messages; current identified messages open directly
// from their observed source.
type dmWindowResolvedMsg struct {
	window      domain.ChannelName
	counterpart *dmCounterpart
	at          time.Time
}

// dmWindowsRestoredMsg carries the counterparts of the DM windows
// the user had open when the process last ran, resolved from the
// client-owned record the user-client keeps. `landing` is the window
// the user left open, read in the same command so the Update
// goroutine waits on no store read; it names one of these windows
// when that is where the user left off, and something else when the
// user left off in a channel. `landingAt` is captured before the
// reads, so a focus change made while they run remains newer.
type dmWindowsRestoredMsg struct {
	counterparts []dmCounterpart
	landing      domain.Window
	landingAt    time.Time
	snapshot     userclient.UIStateSnapshot
}

type dmLandingRestoredMsg struct {
	channel domain.ChannelName
	at      time.Time
}

// openDMWindow puts a DM window in the sidebar cache and records it
// for the next run. Both operations are skipped when the window is
// already open.
func (s ChatScreen) openDMWindow(dm *dmWindow) (*Window, tea.Cmd) {
	w, opened, added := s.materialiseDMWindow(dm)
	if !added {
		return w, opened
	}

	return w, tea.Batch(opened, s.recordDMWindowCmd(dm.Name()))
}

func (s ChatScreen) materialiseDMWindow(dm *dmWindow) (*Window, tea.Cmd, bool) {
	if w, open := s.windowByName(dm.Name()); open {
		current, ok := w.Window.(*dmWindow)
		if ok && current.observeNick(dm.nick) {
			return w, msgCmd(components.ChannelAddedMsg{Channel: current}), false
		}

		return w, nil, false
	}

	w := newWindow(dm)
	s.channels.Insert(w)

	return w, msgCmd(components.ChannelAddedMsg{Channel: dm}), true
}

// recordDMWindowCmd records a DM window in the client-owned set the
// user-client keeps, so a later run reopens it. Best-effort: the
// window is already open on screen, and a failed write costs a
// window the next run does not restore, which is worth a log line
// and nothing more.
func (s ChatScreen) recordDMWindowCmd(name domain.ChannelName) tea.Cmd {
	peer := domain.InstanceID(name)
	write := s.user.RecordDMWindowOpen(peer)

	return func() tea.Msg {
		ctx := s.baseContext()

		if err := write.Wait(); err != nil {
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
	peer := domain.InstanceID(name)
	write := s.user.RecordDMWindowClosed(peer)

	return func() tea.Msg {
		ctx := s.baseContext()

		if err := write.Wait(); err != nil {
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

		nick, err := s.sess.ResolveInstanceByID(ctx, domain.InstanceID(name))
		if err != nil {
			slog.Default().WarnContext(ctx, "resolve dm counterpart",
				"component", "ui",
				"screen", "chat",
				"window", name,
				"error", err,
			)
		}

		if err != nil {
			return dmWindowResolvedMsg{window: name, at: at}
		}

		return dmWindowResolvedMsg{
			window:      name,
			counterpart: &dmCounterpart{id: domain.InstanceID(name), nick: nick},
			at:          at,
		}
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
	counterpart := s.currentDMCounterpart(*msg.counterpart)

	w, opened := s.openDMWindow(newDMWindow(
		counterpart.id,
		counterpart.nick,
		msg.at,
	))

	// The held lines are older than anything already in the window:
	// they were buffered before it existed, and one goroutine drains
	// the bus. A `/query` for the same counterpart while the lookup
	// was running is what puts anything there to go in front of.
	w.prependToScrollback(held)

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
	snapshot := s.user.UIStateSnapshot()
	landingAt := time.Now()

	return func() tea.Msg {
		ctx := s.baseContext()
		landing := s.loadRestoredWindow()

		open, err := s.user.DMWindows(ctx)
		if err != nil {
			slog.Default().WarnContext(ctx, "list open dm windows",
				"component", "ui",
				"screen", "chat",
				"error", err,
			)

			return dmWindowsRestoredMsg{
				landing:   landing,
				landingAt: landingAt,
				snapshot:  snapshot,
			}
		}

		counterparts := make([]dmCounterpart, 0, len(open))

		for _, id := range open {
			nick, err := s.sess.ResolveInstanceByID(ctx, id)
			if err != nil {
				slog.Default().WarnContext(ctx, "resolve dm counterpart",
					"component", "ui",
					"screen", "chat",
					"window", id,
					"error", err,
				)

				continue
			}

			counterparts = append(counterparts, dmCounterpart{id: id, nick: nick})
		}

		return dmWindowsRestoredMsg{
			counterparts: counterparts,
			landing:      landing,
			landingAt:    landingAt,
			snapshot:     snapshot,
		}
	}
}

// handleDMWindowsRestored reopens the recorded DM windows in the
// sidebar. If the window that the user left open is restored, it
// receives focus. If it cannot be restored, channel bootstrap takes
// over after autojoin completes.
//
// The commands run in sequence, so the sidebar has the entry before
// the focus event asks it to mark that entry active.
func (s ChatScreen) handleDMWindowsRestored(msg dmWindowsRestoredMsg) (ChatScreen, tea.Cmd) {
	var cmds []tea.Cmd
	landingRestored := false
	s.dmRestoreDone = true
	s.restoredLanding = msg.landing
	s.restoredLandingAt = msg.landingAt

	for _, counterpart := range msg.counterparts {
		if !msg.snapshot.DMWindowUnchanged(counterpart.id) {
			if msg.landing != nil && msg.landing.Kind() == domain.KindDM &&
				domain.ChannelName(counterpart.id) == msg.landing.Name() {
				if _, open := s.windowByName(msg.landing.Name()); open {
					landingRestored = true
					cmds = append(cmds, msgCmd(dmLandingRestoredMsg{
						channel: msg.landing.Name(), at: msg.landingAt,
					}))
				}
			}

			continue
		}
		counterpart = s.currentDMCounterpart(counterpart)

		dm := newDMWindow(counterpart.id, counterpart.nick, s.sess.ConnectedAt())

		_, opened, _ := s.materialiseDMWindow(dm)
		cmds = append(cmds, opened)

		if msg.landing != nil && msg.landing.Kind() == domain.KindDM &&
			dm.Name() == msg.landing.Name() {
			landingRestored = true
			cmds = append(cmds, msgCmd(dmLandingRestoredMsg{
				channel: dm.Name(), at: msg.landingAt,
			}))
		}
	}

	if !landingRestored && !s.autojoinPending {
		cmds = append(cmds, tea.Sequence(s.bootstrapFromSession(true)...))
	}

	return s, tea.Sequence(cmds...)
}

func (s ChatScreen) handleDMLandingRestored(msg dmLandingRestoredMsg) (ChatScreen, tea.Cmd) {
	if !s.focusWins(msg.at) {
		return s, nil
	}

	return s.handleChannelFocus(chatcmd.ChannelFocusMsg{
		Channel: msg.channel,
		At:      msg.at,
	})
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
	counterpart := s.currentDMCounterpart(dmCounterpart{
		id:   msg.CounterpartID,
		nick: msg.CounterpartNick,
	})
	dm := newDMWindow(counterpart.id, counterpart.nick, msg.At)
	name := dm.Name()

	window, opened := s.openDMWindow(dm)

	cmds := []tea.Cmd{opened}

	if msg.Focus {
		var focused tea.Cmd
		s, focused = s.handleChannelFocus(chatcmd.ChannelFocusMsg{
			Channel: name,
			At:      msg.At,
		})
		cmds = append(cmds, focused)
	} else {
		window.Revision++
	}

	if msg.Body != "" {
		cmds = append(cmds, s.sendMessageCmd("msg", window, msg.Body))
	}

	return s, tea.Sequence(cmds...)
}

// activeDMWith returns the open DM whose counterpart is `actor`,
// if any.
func (s ChatScreen) activeDMWith(actor domain.InstanceID, identified bool) (*dmWindow, bool) {
	if !identified {
		return nil, false
	}

	for w := range s.channels.All() {
		dm, ok := w.Window.(*dmWindow)
		if !ok {
			continue
		}

		if dm.peer == actor {
			return dm, true
		}
	}

	return nil, false
}
