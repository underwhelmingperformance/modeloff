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
func (s ChatScreen) bufferEvent(evt domain.Event) {
	switch e := evt.(type) {
	case domain.Message:
		key, ok := e.RoutingKey(s.user.Instance().ID())
		if !ok || key == "" {
			return
		}

		s.appendToScrollback(key, e)
	case domain.Welcome:
		s.appendStatusNotice(e.At, fmt.Sprintf("Welcome to %s, %s", e.ServerName, e.Nick))
	case domain.Reconnected:
		s.appendStatusNotice(e.At, "Reconnected after unclean shutdown")
	case domain.ModelUnavailableError:
		s.appendStatusNotice(e.At, e.Error())
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
			return
		}

		s.appendToScrollback(ch, e)
	}
}

// bufferProtocolEvent buffers an event delivered on the protocol
// bus. For actor-scoped events (Quit, NickChange) it consumes
// `targets` — the per-recipient channel list on the
// [protocol.Delivery] — to fan the line into each affected
// channel scrollback plus any open DM whose counterpart is the
// actor; window-scoped events fall through to the shared
// [ChatScreen.bufferEvent] path.
func (s ChatScreen) bufferProtocolEvent(evt domain.Event, targets []domain.ChannelName) {
	switch e := evt.(type) {
	case domain.Quit:
		s.bufferActorEvent(targets, e.Instance, e)
	case domain.NickChange:
		s.bufferActorEvent(targets, e.Instance, e)
	default:
		s.bufferEvent(evt)
	}
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
		if ch == *s.active {
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

		if dm.Name() == *s.active {
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
		// happened during their absence. DM windows can only be
		// constructed with a counterpart instance, so DM events
		// for unknown windows are dropped.
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

	s.scrollbackMu.Lock()
	defer s.scrollbackMu.Unlock()

	w.Scrollback = append(w.Scrollback, evt)
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
