package screens

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
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
func (s ChatScreen) bufferEvent(evt domain.Event) (ChatScreen, tea.Cmd) {
	switch e := evt.(type) {
	case domain.Message:
		key, ok := e.RoutingKey(s.user.ID())
		if !ok {
			return s, nil
		}

		return s.appendMessage(key, e)
	case domain.Welcome:
		s.appendStatusNotice(e.At, fmt.Sprintf("Welcome to %s, %s", e.ServerName, e.Nick))
	case domain.Reconnected:
		s.appendStatusNotice(e.At, "Reconnected after unclean shutdown")
	case domain.ModelUnavailableError:
		s.appendDispatchFailure(e, nil)
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
			return s, nil
		}

		s.appendToScrollback(ch, e)
	}

	return s, nil
}

// appendMessage files a message in the window its routing key names.
// An identified direct message carries enough observed identity to
// open its client-owned window immediately. Messages from legacy or
// unidentified sources wait in `pendingDM` while the client resolves
// the counterpart.
func (s ChatScreen) appendMessage(key domain.ChannelName, msg domain.Message) (ChatScreen, tea.Cmd) {
	w, open := s.windowByName(key)
	if open || domain.InferChannelKind(key) != domain.KindDM {
		s.appendToScrollback(key, msg)

		if !open {
			return s, nil
		}

		dm, direct := w.Window.(*dmWindow)
		actor, identified := msg.Source.InstanceID()
		if !direct || !identified || actor != dm.peer || !dm.observeNick(msg.Source.Nick()) {
			return s, nil
		}

		cmds := []tea.Cmd{msgCmd(components.ChannelAddedMsg{Channel: dm})}
		if s.active != nil && s.active.Name() == dm.Name() {
			var nickListUpdated tea.Cmd
			s, nickListUpdated = s.nickListUpdatedCmd()
			cmds = append(cmds,
				s.setChannelCmd(),
				nickListUpdated,
			)
		}

		return s, tea.Batch(cmds...)
	}

	if actor, identified := msg.Source.InstanceID(); identified && domain.ChannelName(actor) == key {
		dm := newDMWindow(actor, msg.Source.Nick(), msg.At)
		_, opened := s.openDMWindow(dm)
		s.appendToScrollback(key, msg)

		return s, opened
	}

	held := s.pendingDM[key]
	s.pendingDM[key] = append(held, msg)

	if len(held) > 0 {
		return s, nil
	}

	return s, s.resolveDMWindow(key, msg.At)
}

// bufferProtocolEvent buffers an event delivered on the protocol
// bus. For actor-scoped events (Quit, NickChange) it consumes
// `targets`, the per-recipient channel list on the
// [protocol.Delivery], and files the line in each affected channel
// plus any open DM whose counterpart is the actor. A model failure
// uses the delivery's recipient-relative `window`. Other
// window-scoped events pass through [ChatScreen.bufferEvent], and
// the caller runs the command it returns.
func (s ChatScreen) bufferProtocolEvent(
	evt domain.Event,
	targets []domain.ChannelName,
	window protocol.WindowTarget,
) (ChatScreen, tea.Cmd) {
	switch e := evt.(type) {
	case domain.Quit:
		id, identified := e.Source.InstanceID()
		s.bufferActorEvent(targets, id, identified, e)
	case domain.NickChange:
		id, identified := e.Source.InstanceID()
		s.bufferActorEvent(targets, id, identified, e)
	case domain.ModelUnavailableError:
		s.appendDispatchFailure(e, window)
	default:
		return s.bufferEvent(evt)
	}

	return s, nil
}

// bufferActorEvent appends `event` to each channel scrollback
// in `targets` plus any open DM whose counterpart is `actor`.
// `targets` comes from [protocol.Delivery.Targets] — the
// per-recipient intersection the session computed at fan-out
// time, so the chat-screen never reads a channels list off the
// wire payload.
func (s ChatScreen) bufferActorEvent(
	targets []domain.ChannelName,
	actor domain.InstanceID,
	identified bool,
	event domain.Event,
) {
	for _, ch := range targets {
		s.appendToScrollback(ch, event)
	}

	if !identified {
		return
	}

	for w := range s.channels.All() {
		dm, ok := w.Window.(*dmWindow)
		if !ok {
			continue
		}

		if dm.peer == actor {
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
func (s ChatScreen) lifecycleBumps(
	channels []domain.ChannelName,
	actor domain.InstanceID,
	identified bool,
) []tea.Cmd {
	var cmds []tea.Cmd

	for _, ch := range channels {
		if s.active != nil && ch == s.active.Name() {
			continue
		}

		cmds = append(cmds, msgCmd(components.ChannelHasLifecycleMsg{Channel: ch}))
	}

	if !identified {
		return cmds
	}

	for w := range s.channels.All() {
		dm, ok := w.Window.(*dmWindow)
		if !ok {
			continue
		}

		if dm.peer != actor {
			continue
		}

		if s.active != nil && dm.Name() == s.active.Name() {
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

	w.appendToScrollback(evt)
}

// appendDispatchFailure renders a failed model turn in the
// recipient-relative window from its delivery envelope. The selected
// key goes through [ChatScreen.fallbackTarget], because the user may
// have closed the window while the turn was running. A delivery with
// no recipient-relative window belongs in the status window.
//
// The empty-target guard here is its own. A background dispatch
// failure is not something the user asked to see, so it renders in
// `&modeloff` and leaves the focus alone, where a reply with no window
// to render in moves the user to `&modeloff` to show them the answer.
// The guard is reachable only in a fixture built with no window ever
// focused, since a running session always lands on one once any
// channel exists (see bootstrapFromSession).
func (s ChatScreen) appendDispatchFailure(
	e domain.ModelUnavailableError,
	window protocol.WindowTarget,
) {
	target := domain.StatusChannelName
	if window != nil {
		var ok bool
		target, ok = s.fallbackTarget(protocol.WindowKey(window))
		if !ok {
			target = domain.StatusChannelName
		}
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
