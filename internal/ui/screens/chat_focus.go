package screens

import (
	"log/slog"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
)

// routeWindows answers everything that decides which window the user
// is in and which windows exist: a focus proposal, a sidebar click,
// and the four steps of a DM window's life (opened by `/query` or by
// an unsolicited message, resolved once its counterpart is known,
// restored at bootstrap, closed by `/close`). The DM handlers
// themselves live in chat_dm.go.
func (s ChatScreen) routeWindows(msg tea.Msg) (ChatScreen, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case chatcmd.ChannelFocusMsg:
		next, cmd := s.handleChannelFocus(msg)
		return next, cmd, true

	case components.ChannelSelectedMsg:
		return s, s.switchChannel(msg.Channel), true

	case chatcmd.DMOpenedMsg:
		next, cmd := s.handleDMOpenedMsg(msg)
		return next, cmd, true

	case chatcmd.DMClosedMsg:
		next, cmd := s.handleDMClosedMsg(msg)
		return next, cmd, true

	case dmWindowResolvedMsg:
		next, cmd := s.handleDMWindowResolved(msg)
		return next, cmd, true

	case dmWindowsRestoredMsg:
		next, cmd := s.handleDMWindowsRestored(msg)
		return next, cmd, true
	}

	return s, nil, false
}

// focus moves the user to `ch` and returns the updated screen
// alongside the completer rebind that keeps tab-completion in step —
// the completion context resolves the active window and its kind, so
// suggestions bind to the window in view. An empty `ch` leaves the
// user with no window, which is the state the welcome checklist
// renders against.
//
// This is the only writer of the focused window: the `active` value
// every handler reads and every command closure captures, and the
// render-side handle the message list resolves, move together. It is
// therefore also where the read cursor moves. Both ends of the switch
// are marked read: what the window being left received while it was
// in view, and what the window being entered was already holding when
// the user arrived. Between them, the badge counts what landed in a
// window while the user was somewhere else.
func (s ChatScreen) focus(ch domain.ChannelName) (ChatScreen, tea.Cmd) {
	leaving := s.active

	s.active = ch
	s.visible.name = ch

	if w, ok := s.windowByName(ch); ok {
		w.Visits++
	}

	cmds := []tea.Cmd{s.rebindCompleter(), s.markReadCmd(ch)}
	if leaving != ch {
		cmds = append(cmds, s.markReadCmd(leaving))
	}

	return s, tea.Batch(cmds...)
}

// markReadCmd persists the user's read position in `ch`. The write
// runs off the Update goroutine; the badge the sidebar shows is
// cleared by the focus change itself, so nothing on screen waits for
// it. A failure costs a badge that over-counts until the window is
// next read, which is worth a log line and nothing more.
func (s ChatScreen) markReadCmd(ch domain.ChannelName) tea.Cmd {
	if ch == "" {
		return nil
	}

	return func() tea.Msg {
		ctx := s.baseContext()

		if err := s.user.MarkRead(ctx, ch); err != nil {
			slog.Default().WarnContext(ctx, "mark channel read",
				"component", "ui",
				"screen", "chat",
				"channel", ch,
				"error", err,
			)
		}

		return nil
	}
}

// setChannelCmd tells the chat view which window it is rendering,
// with that window's topic and kind. Every switch goes through here,
// after [ChatScreen.focus] has moved the user.
func (s ChatScreen) setChannelCmd() tea.Cmd {
	return msgCmd(components.SetChannelMsg{
		Channel: s.active,
		Topic:   s.activeTopic(),
		Kind:    s.activeKind(),
	})
}

func (s ChatScreen) switchChannel(ch domain.ChannelName) tea.Cmd {
	_, exists := s.windowByName(ch)

	return func() tea.Msg {
		// Existing channels: pure frontend state transition. The
		// session call is needed only to create/join a brand-new
		// channel; for ones already in our local cache, switching
		// view is a buffer swap, not a backend round-trip.
		if !exists {
			if err := s.user.Join(s.baseContext(), ch); err != nil {
				return domain.ErrorEvent{Operation: "switch", Err: err, Target: s.active, At: time.Now()}
			}
		}

		return chatcmd.ChannelFocusMsg{Channel: ch, At: time.Now()}
	}
}

func (s ChatScreen) handleChannelFocus(msg chatcmd.ChannelFocusMsg) (ChatScreen, tea.Cmd) {
	w, exists := s.windowByName(msg.Channel)
	if !exists {
		// A focus event for a window the chat screen doesn't
		// track is either a startup race (cache not yet populated
		// by `bootstrapFromSession` or by a JOIN handler) or a
		// stale event for a channel the user has just parted.
		// The latter must not resurrect the parted channel as
		// the new active, so we don't auto-create here — the
		// JOIN/bootstrap paths own cache population and the
		// focus path defers to whatever they install.
		return s, nil
	}

	if !s.focusWins(msg.At) {
		// A staler focus event than the user's current
		// interaction. Flag the target as having activity for
		// the sidebar to surface; leave the visible area where
		// the user put it.
		w.Activity = true

		return s, msgCmd(components.ChannelHasLifecycleMsg{Channel: msg.Channel})
	}

	s, rebind := s.focus(msg.Channel)
	w.UserTime = msg.At

	var members domain.MemberList
	if cw, ok := w.Window.(*domain.ChannelWindow); ok {
		members = cw.Members
	}

	cmds := []tea.Cmd{rebind}
	cmds = append(cmds, msgCmd(components.SetPlaceholderMsg{}))
	cmds = append(cmds, s.setChannelCmd())

	cmds = append(cmds, msgCmd(components.ChannelActiveMsg{Channel: msg.Channel}))
	cmds = append(cmds, s.persistLastChannel(msg.Channel))
	cmds = append(cmds, msgCmd(components.ChannelUnreadMsg{Channel: msg.Channel, Count: 0}))
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))

	return s, tea.Batch(cmds...)
}

// focusWins decides whether an incoming focus event should take
// over the visible area. The arbiter compares the event's
// timestamp against the active window's `UserTime`: a strictly
// newer event wins, anything stamped at or before the user's last
// interaction with the current active is treated as background
// activity and surfaces on the sidebar instead. An empty active —
// the startup case — accepts any event.
func (s ChatScreen) focusWins(at time.Time) bool {
	if s.active == "" {
		return true
	}

	active, ok := s.windowByName(s.active)
	if !ok {
		return true
	}

	return at.After(active.UserTime)
}

// persistLastChannel writes the user's currently-active channel
// to the store so a subsequent restart restores them to the same
// view. An empty channel name and a nil store are no-ops.
func (s ChatScreen) persistLastChannel(ch domain.ChannelName) tea.Cmd {
	if ch == "" || s.uiState == nil {
		return nil
	}

	return func() tea.Msg {
		if err := s.uiState.SetLastChannel(s.baseContext(), ch); err != nil {
			slog.Default().ErrorContext(s.baseContext(), "persist last channel", "channel", ch, "error", err)
		}

		return nil
	}
}

// closeWindow drops a window the user has left: the sidebar entry,
// the cached window, the message list's record of how much of it the
// user had read, and any paced messages still queued for it.
// Already-scheduled ticks for the queue no-op via deliverNextPaced's
// empty-queue branch when they fire. When the closed window was the
// visible one the user lands on the first remaining channel, or on
// the welcome checklist when none is left. `at` is the moment of the
// departure, which the new visible window takes as its `UserTime`:
// the part is the user's freshest deliberate action, so [focusWins]
// keeps them here against any focus event still in flight from before
// it, such as a buffered `NamesReply` for the window just closed.
func (s ChatScreen) closeWindow(ch domain.ChannelName, at time.Time) (ChatScreen, tea.Cmd) {
	wasVisible := s.active == ch

	s.channels.Remove(windowKey(ch))
	delete(s.pacedQueue, ch)
	s.checklist.channelCount = s.realChannelCount()

	cmds := []tea.Cmd{
		msgCmd(components.ChannelRemovedMsg{Channel: ch}),
		msgCmd(components.ScrollbackClearedMsg{Channel: ch}),
	}

	if !wasVisible {
		return s, tea.Batch(cmds...)
	}

	var rebind tea.Cmd

	if first, ok := s.firstRealChannel(); ok {
		s, rebind = s.focus(first.Name())
		first.UserTime = at
	} else {
		s, rebind = s.focus("")
		cmds = append(cmds, msgCmd(components.SetPlaceholderMsg{
			Text: s.checklist.Render(),
		}))
	}

	cmds = append(cmds,
		rebind,
		s.setChannelCmd(),
		msgCmd(components.ChannelActiveMsg{Channel: s.active}),
		s.persistLastChannel(s.active),
	)

	return s, tea.Batch(cmds...)
}

// realChannelCount returns the number of sidebar entries that are
// not the local `&modeloff` server view. The chat-screen owns
// `&modeloff` for the whole session, so it does not count against
// the "the user has joined nothing yet" check that drives the
// welcome checklist.
func (s ChatScreen) realChannelCount() int {
	n := s.channels.Len()
	if _, ok := s.windowByName(domain.StatusChannelName); ok {
		n--
	}

	return n
}

// firstRealChannel returns the first non-`&modeloff` window in
// sidebar order, used by post-part focus fallback. When no real
// channel remains, the caller falls through to the "no channels"
// branch which renders the welcome checklist.
func (s ChatScreen) firstRealChannel() (*Window, bool) {
	for i := range s.channels.Len() {
		w, ok := s.channels.GetAt(i)
		if !ok {
			continue
		}

		if w.Name() == domain.StatusChannelName {
			continue
		}

		return w, true
	}

	return nil, false
}

// windowByName returns the cached `*Window` for the given name.
func (s ChatScreen) windowByName(name domain.ChannelName) (*Window, bool) {
	return s.channels.Get(windowKey(name))
}

// channelWindowByName looks up the cached entry and asserts its
// embedded [domain.Window] is a `*ChannelWindow`. Returns false
// either way for non-channel kinds (status / DM) or absent entries;
// the channel-only handlers (`handleJoinEvent`,
// `handleChannelModeChangeEvent`, etc.) use this to read and mutate
// `Members` / `Topic` off the typed handle.
func (s ChatScreen) channelWindowByName(name domain.ChannelName) (*domain.ChannelWindow, bool) {
	w, ok := s.windowByName(name)
	if !ok {
		return nil, false
	}

	cw, ok := w.Window.(*domain.ChannelWindow)
	return cw, ok
}

// scrollbackOf returns the in-memory scrollback for the named
// window, or nil if the chat-screen has no entry for it. Test-only
// helper; production reads go through the message-list's getter
// closure.
func (s ChatScreen) scrollbackOf(name domain.ChannelName) []domain.Event {
	w, ok := s.windowByName(name)
	if !ok {
		return nil
	}

	return w.Scrollback.Events()
}

func (s ChatScreen) activeTopic() string {
	if s.active == "" {
		return ""
	}

	cw, ok := s.channelWindowByName(s.active)
	if !ok {
		return ""
	}

	return cw.Topic
}

func (s ChatScreen) activeKind() domain.ChannelKind {
	if s.active == "" {
		return domain.KindChannel
	}

	w, ok := s.windowByName(s.active)
	if !ok {
		return domain.InferChannelKind(s.active)
	}

	return w.Kind()
}
