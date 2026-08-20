package screens

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/components"
)

// bufferEvent appends a window-scoped event to the scrollback of
// the window(s) it belongs to. Live-event-driven: a focus change
// later is a pure buffer swap. `Message` routes via
// [domain.Message.RoutingKey] so DM traffic in either direction
// lands in the per-peer scrollback. Other events are channel-keyed
// by their `Target`. Actor-scoped events (Quit, NickChange) are
// handled by [ChatScreen.bufferProtocolEvent], which has access to
// the per-recipient `Targets` carried on the [protocol.Delivery]
// envelope.
//
// The returned command is the counterpart lookup a first line from
// an unopened DM needs; every other event buffers outright and
// returns nil.
func (s ChatScreen) bufferEvent(evt domain.Event) tea.Cmd {
	switch e := evt.(type) {
	case domain.Message:
		key, ok := e.RoutingKey(s.user.Instance().ID())
		if !ok || key == "" {
			return nil
		}

		return s.appendMessage(key, e)
	case domain.Welcome:
		s.appendStatusNotice(e.At, fmt.Sprintf("Welcome to %s, %s", e.ServerName, e.Nick))
	case domain.Reconnected:
		s.appendStatusNotice(e.At, "Reconnected after unclean shutdown")
	case domain.ModelUnavailableError:
		s.appendDispatchFailure(e)
	case domain.UnknownNickError:
		s.appendStatusNotice(time.Now(), e.Error())
	case domain.NoSuchChannelError:
		s.appendStatusNotice(time.Now(), e.Error())
	case domain.NickInUseError:
		s.appendStatusNotice(time.Now(), e.Error())
	case domain.NotOperatorError:
		s.appendStatusNotice(time.Now(), e.Error())
	case domain.PersistableEvent:
		ch := domain.EventTarget(e)
		if ch == "" {
			return nil
		}

		s.appendToScrollback(ch, e)
	}

	return nil
}

// appendMessage files a message in the window its routing key names.
// A key naming a DM the chat-screen has no window for is first
// contact: a model messaging the user out of the blue, which every
// IRC client answers by opening the query window for it. The window is built around the counterpart's
// instance handle and the key carries only its id, so the line waits
// in `pendingDM` while the returned command looks the handle up;
// [ChatScreen.handleDMWindowResolved] moves the queue into the new
// window.
func (s ChatScreen) appendMessage(key domain.ChannelName, msg domain.Message) tea.Cmd {
	_, open := s.windowByName(key)
	if open || domain.InferChannelKind(key) != domain.KindDM {
		s.appendToScrollback(key, msg)

		return nil
	}

	held := s.pendingDM[key]
	s.pendingDM[key] = append(held, msg)

	if len(held) > 0 {
		return nil
	}

	return s.resolveDMWindow(key, msg.At)
}

// bufferProtocolEvent buffers an event delivered on the protocol
// bus. For actor-scoped events (Quit, NickChange) it consumes
// `targets` — the per-recipient channel list on the
// [protocol.Delivery] — to fan the line into each affected
// channel scrollback plus any open DM whose counterpart is the
// actor; window-scoped events fall through to the shared
// [ChatScreen.bufferEvent] path and the caller runs the command it
// returns.
func (s ChatScreen) bufferProtocolEvent(evt domain.Event, targets []domain.ChannelName) tea.Cmd {
	switch e := evt.(type) {
	case domain.Quit:
		s.bufferActorEvent(targets, e.Instance, e)
	case domain.NickChange:
		s.bufferActorEvent(targets, e.Instance, e)
	default:
		return s.bufferEvent(evt)
	}

	return nil
}

// bufferActorEvent appends `event` to each channel scrollback
// in `targets` plus any open DM whose counterpart is `actor`.
// `targets` comes from [protocol.Delivery.Targets] — the
// per-recipient intersection the session computed at fan-out
// time, so the chat-screen never reads a channels list off the
// wire payload.
func (s ChatScreen) bufferActorEvent(targets []domain.ChannelName, actor *domain.Instance, event domain.Event) {
	for _, ch := range targets {
		s.appendToScrollback(ch, event)
	}

	if actor == nil {
		return
	}

	for w := range s.channels.All() {
		dm, ok := w.Window.(*domain.DMWindow)
		if !ok {
			continue
		}

		if dm.Counterpart == actor {
			s.appendToScrollback(dm.Name(), event)
		}
	}
}

// lifecycleBumps returns the sidebar messages flagging unseen
// actor-scoped lifecycle activity for every off-active window
// that received `stored` via [bufferActorEvent]. Iteration shape
// mirrors `bufferActorEvent`: every channel in `channels` (the
// per-recipient [protocol.Delivery.Targets]) plus any open DM
// whose counterpart is `actor`. The active window is skipped —
// the user is already looking at it.
func (s ChatScreen) lifecycleBumps(channels []domain.ChannelName, actor *domain.Instance) []tea.Cmd {
	var cmds []tea.Cmd

	for _, ch := range channels {
		if ch == s.active {
			continue
		}

		cmds = append(cmds, msgCmd(components.ChannelHasLifecycleMsg{Channel: ch}))
	}

	if actor == nil {
		return cmds
	}

	for w := range s.channels.All() {
		dm, ok := w.Window.(*domain.DMWindow)
		if !ok {
			continue
		}

		if dm.Counterpart != actor {
			continue
		}

		if dm.Name() == s.active {
			continue
		}

		cmds = append(cmds, msgCmd(components.ChannelHasLifecycleMsg{Channel: dm.Name()}))
	}

	return cmds
}

func (s ChatScreen) appendToScrollback(ch domain.ChannelName, evt domain.Event) {
	w, ok := s.windowByName(ch)
	if !ok {
		// Channels for events that arrive before the chat-screen
		// has seen a join for the target are placeholder-created
		// here so scrollback never drops live traffic: the user
		// may focus this channel later and expect to see what
		// happened during their absence. A DM window needs its
		// counterpart's instance handle, which this cannot
		// synthesise; chat traffic takes the lookup path in
		// [ChatScreen.appendMessage] instead, and anything else
		// naming an unopened DM is dropped.
		switch domain.InferChannelKind(ch) {
		case domain.KindChannel:
			w = newWindow(domain.NewChannelWindow(ch, time.Time{}))
		case domain.KindStatus:
			w = newWindow(domain.NewStatusWindow(time.Time{}))
		default:
			return
		}

		s.channels.Insert(w)
	}

	w.Scrollback.Append(evt)
}

// appendDispatchFailure renders a failed model turn in the window it
// ran in — fallbackTarget(e.Channel), or the status window if
// neither that window nor the active one resolves — as
// chat-screen-local content, the same closed-window fallback
// handleErrorEvent applies to a command failure. A dispatch turn can
// fail for a channel the user is not currently looking at, or has
// since parted, so this must not always land in `&modeloff`
// regardless of which window the turn was actually running in, and
// must never resurrect a window the user has left.
//
// The empty-target guard here is its own, not a call into
// handleErrorEvent's logAndShow fallback: this method has no tea.Cmd
// to carry a focus change through, and a background dispatch failure
// is not something the user asked to see, so it never moves focus the
// way a command failure's empty-target case does. Without the guard,
// appendToScrollback would drop the notice outright when fallbackTarget
// also comes back empty — reachable only in a fixture built with no
// window ever focused, since a running session always lands on one
// once any channel exists (see bootstrapFromSession).
func (s ChatScreen) appendDispatchFailure(e domain.ModelUnavailableError) {
	target := s.fallbackTarget(e.Channel)
	if target == "" {
		target = domain.StatusChannelName
	}

	s.appendToScrollback(target, domain.SystemNotice{
		Target: target,
		Text:   e.Error(),
		At:     e.At,
	})
}

// appendStatusNotice records a server-narrated line in the local
// `&modeloff` scrollback. New protocol events that have no
// channel target (welcome, reconnect notices, error replies)
// reach the chat-screen as wire events; wrapping them in a
// [domain.SystemNotice] lets the existing renderer style them
// as `*** <text>` without growing the event-render switch.
func (s ChatScreen) appendStatusNotice(at time.Time, text string) {
	s.appendToScrollback(domain.StatusChannelName, domain.SystemNotice{
		Target: domain.StatusChannelName,
		Text:   text,
		At:     at,
	})
}
