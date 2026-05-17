package screens

import (
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
)

// handleSessionEvent dispatches non-protocol UI events that flow
// on `Session.Events()`: error events, system-notice wrappers, and
// config changes. The protocol bus carries every session-emitted
// event whose relative order matters to the chat-screen; those
// are dispatched by [ChatScreen.handleProtocolEvent].
func (s ChatScreen) handleSessionEvent(msg sessionEventMsg) (ui.Model, tea.Cmd) {
	var (
		updated ui.Model
		cmd     tea.Cmd
	)

	// `bufferEvent` is called from both event-bus dispatchers; the
	// body's PersistableEvent switch handles disjoint subsets.
	s.bufferEvent(msg.event)

	switch evt := msg.event.(type) {
	case domain.ConfigChangedEvent:
		updated, cmd = s.handleConfigChangedEvent(evt)
	case domain.ErrorEvent:
		updated, cmd = s.handleErrorEvent(evt)
	case domain.SystemNoticeEvent:
		updated, cmd = s.handleSystemNoticeEvent(evt)
	}

	if updated != nil {
		s = updated.(ChatScreen)
	}

	return s, tea.Batch(cmd, s.scrollbackUpdatedCmd(), s.listenForEvents())
}

// handleProtocolEvent dispatches wire-shaped events plus the
// session-emitted events whose ordering relative to the wire
// sequence matters: joins, parts, messages, mode changes, topic
// info, dispatch lifecycle, names replies, the status-window
// signal, and focus changes.
func (s ChatScreen) handleProtocolEvent(msg protocolEventMsg) (ui.Model, tea.Cmd) {
	var (
		updated ui.Model
		cmd     tea.Cmd
	)

	s.bufferProtocolEvent(msg.event, msg.targets)

	switch evt := msg.event.(type) {
	case domain.Join:
		updated, cmd = s.handleJoinEvent(evt)
	case domain.Part:
		updated, cmd = s.handlePartEvent(evt)
	case domain.Quit:
		updated, cmd = s.handleQuitEvent(evt, msg.targets)
	case domain.ModeChange:
		updated, cmd = s.handleModeChangeEvent(evt)
	case domain.Message:
		updated, cmd = s.handleMessageEvent(evt)
	case domain.TopicChange:
		updated, cmd = s.handleTopicChangeEvent(evt)
	case domain.NickChange:
		updated, cmd = s.handleNickChangeEvent(evt, msg.targets)
	case domain.ModelInvited:
		updated, cmd = s.handleModelInvitedEvent(evt)
	case domain.ModelKicked:
		updated, cmd = s.handleModelKickedEvent(evt)
	case domain.TopicInfo:
		updated, cmd = s.handleTopicInfoEvent(evt)
	case domain.ModelDispatchStarted:
		updated, cmd = s.handleModelDispatchStarted(evt)
	case domain.ModelDispatchDone:
		updated, cmd = s.handleModelDispatchDone(evt)
	case domain.NamesReplyEvent:
		updated, cmd = s.handleNamesReply(evt)
	}

	if updated != nil {
		s = updated.(ChatScreen)
	}

	return s, tea.Batch(cmd, s.scrollbackUpdatedCmd(), s.listenForProtocolEvents())
}

// scrollbackUpdatedCmd nudges the message list to re-evaluate the
// active window's scrollback after an event was buffered. Without
// the nudge the new content would still render on the next View
// because the message list reads through a getter, but the divider
// latch would miss the per-tick growth signal and an off-bottom user
// would never see the "new messages" line.
func (s ChatScreen) scrollbackUpdatedCmd() tea.Cmd {
	if s.active == nil || *s.active == "" {
		return nil
	}

	return msgCmd(components.ScrollbackUpdatedMsg{Channel: *s.active})
}

// bufferEvent appends a session-bus persistable event to the
// scrollback of the window(s) it belongs to. Live-event-driven:
// a focus change later is a pure buffer swap. `Message` routes
// via [domain.Message.RoutingKey] so DM traffic in either
// direction lands in the per-peer scrollback. Other events are
// channel-keyed by their `Target`. SystemNoticeEvent unwraps its
// already-persisted inner event. Actor-scoped events (Quit,
// NickChange) only flow on the protocol bus and are buffered by
// [ChatScreen.bufferProtocolEvent], which has access to the
// per-recipient `Targets` carried on the [protocol.Delivery]
// envelope.
func (s ChatScreen) bufferEvent(evt domain.Event) {
	switch e := evt.(type) {
	case domain.SystemNoticeEvent:
		s.appendToScrollback(e.Channel, e.Stored)
	case domain.Message:
		key, ok := e.RoutingKey(s.sess.UserInstance().ID())
		if !ok || key == "" {
			return
		}

		s.appendToScrollback(key, domain.StoredEvent{Event: e})
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

		s.appendToScrollback(ch, domain.StoredEvent{Event: e})
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
		s.bufferActorEvent(targets, e.Instance, domain.StoredEvent{Event: e})
	case domain.NickChange:
		s.bufferActorEvent(targets, e.Instance, domain.StoredEvent{Event: e})
	default:
		s.bufferEvent(evt)
	}
}

// bufferActorEvent appends `stored` to each channel scrollback
// in `targets` plus any open DM whose counterpart is `actor`.
// `targets` comes from [protocol.Delivery.Targets] — the
// per-recipient intersection the session computed at fan-out
// time, so the chat-screen never reads a channels list off the
// wire payload.
func (s ChatScreen) bufferActorEvent(targets []domain.ChannelName, actor *domain.Instance, stored domain.StoredEvent) {
	for _, ch := range targets {
		s.appendToScrollback(ch, stored)
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
			s.appendToScrollback(dm.Name(), stored)
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

func (s ChatScreen) appendToScrollback(ch domain.ChannelName, evt domain.StoredEvent) {
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
	s.appendToScrollback(domain.StatusChannelName, domain.StoredEvent{
		Event: domain.SystemNotice{
			Target: domain.StatusChannelName,
			Text:   text,
			At:     at,
		},
	})
}

// handleSystemNoticeEvent forwards a freshly-appended system notice
// to the message list when the affected channel is the active one.
// Off-channel notices update only the unread badge.
func (s ChatScreen) handleSystemNoticeEvent(msg domain.SystemNoticeEvent) (ui.Model, tea.Cmd) {
	if msg.Channel == *s.active {
		return s, msgCmd(msg.Stored)
	}

	count, _ := s.sess.UnreadCount(s.ctx, msg.Channel)

	return s, msgCmd(components.ChannelUnreadMsg{Channel: msg.Channel, Count: count})
}

func (s ChatScreen) handleChannelFocus(msg chatcmd.ChannelFocusMsg) (ui.Model, tea.Cmd) {
	w, exists := s.windowByName(msg.Channel)
	if !exists {
		// First focus into a window the chat screen hasn't seen
		// before — populate the cache with a fresh `*Window`
		// wrapping a fresh [domain.ChannelWindow]. Status and DM
		// windows arrive via their own dedicated events and
		// shouldn't hit this path.
		w = newWindow(domain.NewChannelWindow(msg.Channel, time.Time{}))
		s.channels.Insert(w)
	}

	if !s.focusWins(msg.At) {
		// A staler focus event than the user's current
		// interaction. Flag the target as having activity for
		// the sidebar to surface; leave the visible area where
		// the user put it.
		w.Activity = true

		return s, msgCmd(components.ChannelHasLifecycleMsg{Channel: msg.Channel})
	}

	*s.active = msg.Channel
	w.UserTime = msg.At

	var members domain.MemberList
	if cw, ok := w.Window.(*domain.ChannelWindow); ok {
		members = cw.Members
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.SetPlaceholderMsg{}))
	cmds = append(cmds, msgCmd(components.SetChannelMsg{
		Channel: msg.Channel,
		Topic:   s.activeTopic(),
		Kind:    w.Kind(),
	}))

	if !exists {
		cmds = append(cmds, msgCmd(components.ChannelAddedMsg{Channel: w.Window}))
	}

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
	if s.active == nil || *s.active == "" {
		return true
	}

	active, ok := s.windowByName(*s.active)
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
		if err := s.uiState.SetLastChannel(s.ctx, ch); err != nil {
			slog.Default().ErrorContext(s.ctx, "persist last channel", "channel", ch, "error", err)
		}

		return nil
	}
}

// handleNamesReply applies the joiner-targeted member-list snapshot
// to the local channel cache and proposes the freshly-joined
// channel as the focus target. Pre-existing members of the channel
// — models, other users — are otherwise invisible to the chat
// screen's cache; without this handler, switching to a freshly-
// joined channel would show only the user's own name.
//
// The focus proposal carries the window's `UserTime` (the
// join-event timestamp), so the arbiter in `handleChannelFocus`
// keeps the user where they are if they've already navigated past
// this join, and lands them on the freshest autojoin channel
// otherwise.
func (s ChatScreen) handleNamesReply(msg domain.NamesReplyEvent) (ui.Model, tea.Cmd) {
	w, ok := s.windowByName(msg.Channel)
	if !ok {
		// `NamesReplyEvent` only follows a real user-join; the
		// join handler should have populated the cache already.
		// A miss means the upstream sequencing is wrong, but we
		// don't have anything sensible to do here besides log.
		slog.Default().WarnContext(s.ctx, "names reply for unknown channel",
			"component", "chat_screen",
			"channel", msg.Channel,
		)

		return s, nil
	}

	cw, isChannel := w.Window.(*domain.ChannelWindow)
	if isChannel {
		cw.Members = msg.Members
	}

	cmds := []tea.Cmd{
		msgCmd(chatcmd.ChannelFocusMsg{Channel: msg.Channel, At: w.UserTime}),
	}

	if isChannel && msg.Channel == *s.active {
		cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: cw.Members}))
	}

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleJoinEvent(msg domain.Join) (ui.Model, tea.Cmd) {
	isUser := msg.Instance == s.sess.UserInstance()

	w, channelKnown := s.windowByName(msg.Target)

	if !isUser && !channelKnown {
		return s, nil
	}

	var cw *domain.ChannelWindow
	if channelKnown {
		cw, _ = w.Window.(*domain.ChannelWindow)
	} else {
		// First sighting of this channel. The chat-screen learns
		// about it from the join, so it owns the cache
		// population.
		cw = domain.NewChannelWindow(msg.Target, msg.At)
		w = newWindow(cw)
		s.channels.Insert(w)
	}

	if cw != nil && !cw.Members.HasInstance(msg.Instance) {
		cw.Members.Add(msg.Instance)
	}

	if !isUser {
		if msg.Target == *s.active && cw != nil {
			return s, msgCmd(components.NickListUpdatedMsg{Members: cw.Members})
		}

		return s, nil
	}

	// `UserTime` stamps the user's deliberate moment with this
	// window. The user-join is the earliest such moment; later
	// keystrokes and focus events bump it. The window may have
	// been pre-created by `bufferProtocolEvent`'s auto-stamping
	// with a zero `UserTime`, so guard on `IsZero` rather than
	// `!channelKnown` to catch that path.
	if w.UserTime.IsZero() {
		w.UserTime = msg.At
	}

	s.checklist.channelCount = s.realChannelCount()

	return s, tea.Batch(
		msgCmd(components.ChannelAddedMsg{Channel: w.Window}),
		msgCmd(components.ChannelUnreadMsg{Channel: msg.Target, Count: 0}),
	)
}

func (s ChatScreen) handleModeChangeEvent(msg domain.ModeChange) (ui.Model, tea.Cmd) {
	if !msg.ChannelMode() {
		return s.handleUserModeChangeEvent(msg)
	}

	cw, ok := s.channelWindowByName(msg.Target)
	if !ok {
		return s, nil
	}

	cw.Members.SetMode(msg.Instance, domain.NickModeFor(msg.Flag, msg.Add))

	if msg.Target != *s.active {
		return s, nil
	}

	return s, msgCmd(components.NickListUpdatedMsg{Members: cw.Members})
}

// handleUserModeChangeEvent reacts to a user-mode ModeChange (empty
// Target). When the change targets the user-client's own instance,
// the visible command set may have flipped — re-emit CommandsMsg
// from VisibleCommands so the /help slice and the completion
// popover both reflect the new capability state on next render.
func (s ChatScreen) handleUserModeChangeEvent(msg domain.ModeChange) (ui.Model, tea.Cmd) {
	if msg.InstanceID != s.sess.UserInstance().ID() {
		return s, nil
	}

	return s, msgCmd(components.CommandsMsg[chatcmd.CompletionContext]{
		Commands: command.VisibleCommands(s.parser.Set(), s.client.Caps()),
	})
}

func (s ChatScreen) handlePartEvent(msg domain.Part) (ui.Model, tea.Cmd) {
	leavingActive := *s.active == msg.Target

	// Remove the member from the channel's member list.
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		if m, mOK := cw.Members.GetByInstance(msg.Instance); mOK {
			cw.Members.Remove(m)
		}
	}

	// If the user is leaving, remove the channel and purge any
	// pending paced messages queued for it. Already-scheduled ticks
	// for the parted channel's queue will no-op via
	// deliverNextPaced's empty-queue branch when they fire.
	if msg.Instance == s.sess.UserInstance() {
		s.channels.Remove(windowKey(msg.Target))
		delete(s.pacedQueue, msg.Target)
		s.checklist.channelCount = s.realChannelCount()
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.ChannelRemovedMsg{Channel: msg.Target}))

	if leavingActive {
		if first, ok := s.firstRealChannel(); ok {
			*s.active = first.Name()
		} else {
			*s.active = ""
			cmds = append(cmds, msgCmd(components.SetPlaceholderMsg{
				Text: s.checklist.Render(),
			}))
		}

		cmds = append(cmds, msgCmd(components.SetChannelMsg{
			Channel: *s.active,
			Topic:   s.activeTopic(),
			Kind:    s.activeKind(),
		}))
		cmds = append(cmds, msgCmd(components.ChannelActiveMsg{Channel: *s.active}))
		cmds = append(cmds, s.persistLastChannel(*s.active))
	}

	var members domain.MemberList

	if *s.active != "" {
		if cw, ok := s.channelWindowByName(*s.active); ok {
			members = cw.Members
		}
	}

	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleQuitEvent(msg domain.Quit, targets []domain.ChannelName) (ui.Model, tea.Cmd) {
	// `bufferProtocolEvent` has already fanned the line into
	// every channel in `targets` and any open DM with the actor.
	// The handler updates the in-memory `Members` snapshot for
	// each affected channel and fires the active-window UI
	// refresh. `targets` comes from the per-recipient
	// [protocol.Delivery.Targets] computed by the session at
	// fan-out time.

	for _, ch := range targets {
		cw, ok := s.channelWindowByName(ch)
		if !ok {
			continue
		}

		if m, mOK := cw.Members.GetByInstance(msg.Instance); mOK {
			cw.Members.Remove(m)
		}
	}

	var cmds []tea.Cmd

	for _, ch := range targets {
		if ch != *s.active {
			continue
		}

		if cw, ok := s.channelWindowByName(*s.active); ok {
			cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: cw.Members}))
		}
	}

	cmds = append(cmds, s.lifecycleBumps(targets, msg.Instance)...)

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleTopicChangeEvent(msg domain.TopicChange) (ui.Model, tea.Cmd) {
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		cw.Topic = msg.Topic
		cw.TopicSetBy = msg.By
		cw.TopicSetAt = msg.At
	}

	if *s.active != msg.Target {
		return s, nil
	}

	return s, msgCmd(components.TopicUpdatedMsg{Topic: msg.Topic})
}

func (s ChatScreen) handleTopicInfoEvent(msg domain.TopicInfo) (ui.Model, tea.Cmd) {
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		cw.Topic = msg.Topic
		cw.TopicSetBy = msg.TopicSetBy
		cw.TopicSetAt = msg.TopicSetAt
	}

	if *s.active != msg.Target {
		return s, nil
	}

	return s, msgCmd(components.SetChannelMsg{
		Channel: msg.Target,
		Topic:   msg.Topic,
		Kind:    s.activeKind(),
	})
}

func (s ChatScreen) handleNickChangeEvent(msg domain.NickChange, targets []domain.ChannelName) (ui.Model, tea.Cmd) {
	// `msg.Instance.Nick()` is already the new value — the
	// session renames before emitting. Update the snapshot in
	// each affected channel's member list, then fire the
	// active-window UI side-effects exactly once. `targets`
	// comes from the per-recipient [protocol.Delivery.Targets]
	// computed by the session at fan-out time.
	for _, ch := range targets {
		cw, ok := s.channelWindowByName(ch)
		if !ok {
			continue
		}

		if cw.Members.HasInstance(msg.Instance) {
			cw.Members.RenameTo(msg.Instance, msg.NewNick)
		}
	}

	var cmds []tea.Cmd

	activeIsChannel := slices.Contains(targets, *s.active)

	activeDM, activeIsDM := s.activeDMWith(msg.Instance)
	activeDMVisible := activeIsDM && activeDM.Name() == *s.active

	if activeIsChannel || activeDMVisible {
		if activeIsChannel {
			if cw, ok := s.channelWindowByName(*s.active); ok {
				cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: cw.Members}))
			}
		}

		if msg.Instance == s.sess.UserInstance() {
			cmds = append(cmds, msgCmd(components.UserNickMsg{Nick: msg.NewNick}))
		}

		nickCfg, _ := s.loadConfig()
		cmds = append(cmds, msgCmd(components.HighlightWordsMsg{
			Words:    nickCfg.HighlightWords,
			UserNick: s.sess.UserNick(),
		}))
	}

	cmds = append(cmds, s.lifecycleBumps(targets, msg.Instance)...)

	return s, tea.Batch(cmds...)
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

func (s ChatScreen) handleModelInvitedEvent(msg domain.ModelInvited) (ui.Model, tea.Cmd) {
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		if !cw.Members.HasInstance(msg.Instance) {
			cw.Members.Add(msg.Instance)
		}
	}

	var members domain.MemberList

	if cw, ok := s.channelWindowByName(*s.active); ok {
		members = cw.Members
	}

	return s, msgCmd(components.NickListUpdatedMsg{Members: members})
}

func (s ChatScreen) handleModelKickedEvent(msg domain.ModelKicked) (ui.Model, tea.Cmd) {
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		if m, mOK := cw.Members.GetByInstance(msg.Instance); mOK {
			cw.Members.Remove(m)
		}
	}

	var members domain.MemberList

	if cw, ok := s.channelWindowByName(*s.active); ok {
		members = cw.Members
	}

	return s, msgCmd(components.NickListUpdatedMsg{Members: members})
}

// handleMessageEvent renders an incoming Message. User-originated
// Messages (the synthesised echo from the send-cmd path, identified
// by an empty InstanceID matching [protocol.UserClientID]) render
// inline. Model-originated Messages enter the per-channel paced
// queue: the first message in an empty queue delivers immediately,
// subsequent messages drain at [pacedInterval] cadence.
func (s ChatScreen) handleMessageEvent(msg domain.Message) (ui.Model, tea.Cmd) {
	key, ok := msg.RoutingKey(s.sess.UserInstance().ID())
	if !ok {
		// Foreign DM (model-to-model traffic the user is not a
		// party to). Not surfaced in the user's UI.
		return s, nil
	}

	if msg.InstanceID == protocol.UserClientID {
		return s, s.renderMessage(msg, key)
	}

	wasEmpty := len(s.pacedQueue[key]) == 0
	s.pacedQueue[key] = append(s.pacedQueue[key], msg)

	// If this channel had no pending messages, deliver immediately;
	// pacing is per-channel, so unrelated channels keep their own
	// schedules.
	if wasEmpty {
		return s, s.deliverNextPacedCmd(key)
	}

	return s, nil
}

// renderMessage emits the off-channel unread bump for a Message
// not targeting the active window. Active-channel messages render
// on the next frame because the message list reads scrollback
// through a getter and `bufferEvent` has already appended the
// message; no live `StoredEvent` is needed.
func (s ChatScreen) renderMessage(msg domain.Message, key domain.ChannelName) tea.Cmd {
	if key == *s.active {
		return nil
	}

	count, _ := s.sess.UnreadCount(s.ctx, key)
	mention := s.isHighlight(msg.Body)

	return msgCmd(components.ChannelUnreadMsg{Channel: key, Count: count, Mention: mention})
}

// handleDMOpenedMsg materialises the DM window in the sidebar
// (insert is idempotent), optionally focus-switches, and
// optionally sends a trailing body. `/query` sets `Focus`;
// `/msg` does not.
func (s ChatScreen) handleDMOpenedMsg(msg chatcmd.DMOpenedMsg) (ui.Model, tea.Cmd) {
	dm := domain.NewDMWindow(msg.Counterpart, msg.At)
	name := dm.Name()

	_, alreadyOpen := s.channels.Get(windowKey(name))

	if !alreadyOpen {
		s.channels.Insert(newWindow(dm))
	}

	var cmds []tea.Cmd

	if !alreadyOpen {
		cmds = append(cmds, msgCmd(components.ChannelAddedMsg{Channel: dm}))
	}

	if msg.Focus {
		*s.active = name
		cmds = append(cmds, msgCmd(components.SetPlaceholderMsg{}))
		cmds = append(cmds, msgCmd(components.SetChannelMsg{
			Channel: name,
			Topic:   s.activeTopic(),
			Kind:    domain.KindDM,
		}))
		cmds = append(cmds, msgCmd(components.ChannelActiveMsg{Channel: name}))
		cmds = append(cmds, s.persistLastChannel(name))
		cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: domain.MemberList{}}))
	}

	if msg.Body != "" {
		cmds = append(cmds, s.sendMessageCmd(name, msg.Body))
	}

	return s, tea.Sequence(cmds...)
}

// sendMessageCmd fires a user `SendMessage` and returns the
// persisted [domain.Message] as a tea.Msg for local render.
func (s ChatScreen) sendMessageCmd(target domain.ChannelName, body string) tea.Cmd {
	return func() tea.Msg {
		msg, err := s.sess.SendMessage(s.ctx, target, body)
		if err != nil {
			return domain.ErrorEvent{Operation: "msg", Err: err, At: time.Now()}
		}

		return msg
	}
}

func (s ChatScreen) handleConfigChangedEvent(msg domain.ConfigChangedEvent) (ui.Model, tea.Cmd) {
	if *s.active == "" {
		return s, nil
	}

	return s, s.logAndShow(domain.SystemNotice{
		Target: *s.active,
		Text:   msg.Operation,
		At:     msg.At,
	})
}

func (s ChatScreen) handleErrorEvent(msg domain.ErrorEvent) (ui.Model, tea.Cmd) {
	var cmds []tea.Cmd

	cmds = append(cmds, s.logAndShow(domain.CommandError{
		Target: *s.active,
		Err:    fmt.Sprintf("%s: %s", msg.Operation, msg.Err),
		At:     msg.At,
	}))

	cmds = append(cmds, msgCmd(components.PendingResponseMsg{Pending: false}))
	cmds = append(cmds, msgCmd(components.NickListThinkingMsg{}))

	return s, tea.Batch(cmds...)
}

// handleModelDispatchStarted marks `msg.Instance` as currently
// dispatching and updates the pending/thinking indicators. The
// pending spinner is on whenever any tracked instance is in a
// turn (concurrent dispatches from different models in the same
// window survive each other's Done events), and the thinking
// nick list for the active channel surfaces every dispatching
// instance whose membership the active window can see.
func (s ChatScreen) handleModelDispatchStarted(msg domain.ModelDispatchStarted) (ui.Model, tea.Cmd) {
	if msg.Instance == nil {
		return s, nil
	}

	s.dispatching[msg.Instance] = true

	return s, tea.Batch(
		msgCmd(components.PendingResponseMsg{Pending: true}),
		msgCmd(components.NickListThinkingMsg{Nicks: s.thinkingNicks()}),
	)
}

// handleModelDispatchDone clears the dispatching mark for
// `msg.Instance` and re-derives the indicators. Paced model
// replies still in the per-channel queue keep the spinner on:
// the user sees a single continuous "responding…" line across
// dispatch turn and paced reply drain.
func (s ChatScreen) handleModelDispatchDone(msg domain.ModelDispatchDone) (ui.Model, tea.Cmd) {
	if msg.Instance != nil {
		delete(s.dispatching, msg.Instance)
	}

	cmds := []tea.Cmd{
		msgCmd(components.NickListThinkingMsg{Nicks: s.thinkingNicks()}),
	}

	if len(s.dispatching) == 0 && !s.hasQueuedPaced() {
		cmds = append(cmds, msgCmd(components.PendingResponseMsg{Pending: false}))
	}

	return s, tea.Batch(cmds...)
}

// thinkingNicks returns the nicks of every dispatching instance
// that is also a member of the active channel. Models running in
// channels the user is not in stay invisible — RFC 2812 §3.3.1's
// intersection rule applied to the local view.
func (s ChatScreen) thinkingNicks() map[domain.Nick]bool {
	if s.active == nil || *s.active == "" || len(s.dispatching) == 0 {
		return nil
	}

	cw, ok := s.channelWindowByName(*s.active)
	if !ok {
		return nil
	}

	thinking := make(map[domain.Nick]bool, len(s.dispatching))
	for inst := range s.dispatching {
		if !cw.Members.HasInstance(inst) {
			continue
		}

		thinking[inst.Nick()] = true
	}

	return thinking
}

const pacedInterval = 400 * time.Millisecond

// hasQueuedPaced reports whether any channel has pending paced
// messages. The pending/thinking indicators are application-wide,
// so they clear only when every channel's queue has drained. The
// field's pruning invariant (drained channels are deleted from the
// map) makes this an O(1) length check.
func (s ChatScreen) hasQueuedPaced() bool {
	return len(s.pacedQueue) > 0
}

func (s ChatScreen) scheduleNextPaced(ch domain.ChannelName) tea.Cmd {
	return tea.Tick(pacedInterval, func(time.Time) tea.Msg {
		return deliverNextPacedMsg{Channel: ch}
	})
}

// deliverNextPacedCmd returns a tea.Cmd that delivers the next
// paced message from the given channel's queue immediately (without
// pacing delay).
func (s ChatScreen) deliverNextPacedCmd(ch domain.ChannelName) tea.Cmd {
	return func() tea.Msg { return deliverNextPacedMsg{Channel: ch} }
}

func (s ChatScreen) deliverNextPaced(msg deliverNextPacedMsg) (ui.Model, tea.Cmd) {
	queue := s.pacedQueue[msg.Channel]
	if len(queue) == 0 {
		// The channel's queue has drained. If no other channel has
		// pending messages either, clear the application-wide
		// pending/thinking indicators.
		if !s.hasQueuedPaced() {
			return s, tea.Batch(
				msgCmd(components.NickListThinkingMsg{}),
				msgCmd(components.PendingResponseMsg{Pending: false}),
			)
		}

		return s, nil
	}

	next := queue[0]
	queue = queue[1:]

	if len(queue) == 0 {
		delete(s.pacedQueue, msg.Channel)
	} else {
		s.pacedQueue[msg.Channel] = queue
	}

	cmd := s.renderMessage(next, msg.Channel)

	// Schedule the next delivery for this channel if more remain
	// here; otherwise, if every channel has drained, clear the
	// application-wide pending indicators.
	if len(s.pacedQueue[msg.Channel]) > 0 {
		cmd = tea.Batch(cmd, s.scheduleNextPaced(msg.Channel))
	} else if !s.hasQueuedPaced() {
		cmd = tea.Batch(cmd,
			msgCmd(components.NickListThinkingMsg{}),
			msgCmd(components.PendingResponseMsg{Pending: false}),
		)
	}

	return s, cmd
}

func (s ChatScreen) isHighlight(body string) bool {
	cfg, _ := s.loadConfig()

	return components.ContainsHighlightWord(body, cfg.HighlightWords, s.sess.UserNick())
}

func (s ChatScreen) handleLiveModelsLoaded(msg liveModelsLoadedMsg) (ui.Model, tea.Cmd) {
	*s.liveModels = msg.models
	*s.liveModelsState = command.SuggestionStateReady

	return s, nil
}

// handleLiveModelsLoadFailed is the UI-policy home for live-model
// load failures. When `*s.active` is empty — no real channel
// joined yet — the notice is routed to `&modeloff`, the
// chat-screen-owned default landing window.
func (s ChatScreen) handleLiveModelsLoadFailed(msg liveModelsLoadFailedMsg) (ui.Model, tea.Cmd) {
	*s.liveModels = nil

	// ErrNoAPIKey here is a TOCTOU between loadLiveModels' HasAPIKey
	// short-circuit and Session.ListModels' check; treat as silent.
	if errors.Is(msg.err, session.ErrNoAPIKey) {
		*s.liveModelsState = command.SuggestionStateReady
		return s, nil
	}

	*s.liveModelsState = command.SuggestionStateError

	channel := *s.active
	if channel == "" {
		channel = domain.StatusChannelName
	}

	slog.Default().WarnContext(s.ctx, "live models load failed",
		"component", "ui",
		"channel", string(channel),
		"error", msg.err,
	)

	return s, s.logAndShowOn(channel, domain.SystemNotice{
		Target: channel,
		Text:   fmt.Sprintf("Model list unavailable: %s.", msg.err),
		At:     time.Now(),
	})
}

func (s ChatScreen) activeTopic() string {
	if *s.active == "" {
		return ""
	}

	cw, ok := s.channelWindowByName(*s.active)
	if !ok {
		return ""
	}

	return cw.Topic
}

func (s ChatScreen) activeKind() domain.ChannelKind {
	if *s.active == "" {
		return domain.KindChannel
	}

	w, ok := s.windowByName(*s.active)
	if !ok {
		return domain.InferChannelKind(*s.active)
	}

	return w.Kind()
}

func (s ChatScreen) activeMemberNicks() iter.Seq[domain.Nick] {
	cw, ok := s.channelWindowByName(*s.active)
	if !ok {
		return func(func(domain.Nick) bool) {}
	}

	return cw.Members.Nicks()
}

// activeChannelInstances iterates the `*Instance` handles for every
// member of the currently-active channel. Tab completion sources this
// iterator: the user only gets completions for nicks they can already
// see in their nick list, matching IRC semantics.
func (s ChatScreen) activeChannelInstances() iter.Seq[*domain.Instance] {
	return func(yield func(*domain.Instance) bool) {
		cw, ok := s.channelWindowByName(*s.active)
		if !ok {
			return
		}

		for m := range cw.Members.All() {
			if !yield(m.Instance) {
				return
			}
		}
	}
}

// windowByName returns the cached `*Window` for the given name.
func (s ChatScreen) windowByName(name domain.ChannelName) (*Window, bool) {
	return s.channels.Get(windowKey(name))
}

// scrollbackOf returns the in-memory scrollback for the named
// window, or nil if the chat-screen has no entry for it. Test-only
// helper; production reads go through the message-list's getter
// closure.
func (s ChatScreen) scrollbackOf(name domain.ChannelName) []domain.StoredEvent {
	w, ok := s.windowByName(name)
	if !ok {
		return nil
	}

	return w.Scrollback
}

// channelWindowByName looks up the cached entry and asserts its
// embedded [domain.Window] is a `*ChannelWindow`. Returns false
// either way for non-channel kinds (status / DM) or absent entries;
// the channel-only handlers (`handleJoinEvent`,
// `handleModeChangeEvent`, etc.) use this to read and mutate
// `Members` / `Topic` without going through the legacy `Channel`
// projection.
func (s ChatScreen) channelWindowByName(name domain.ChannelName) (*domain.ChannelWindow, bool) {
	w, ok := s.windowByName(name)
	if !ok {
		return nil, false
	}

	cw, ok := w.Window.(*domain.ChannelWindow)
	return cw, ok
}
