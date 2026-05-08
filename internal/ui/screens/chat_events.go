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

	return s, tea.Batch(cmd, s.listenForEvents())
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

	s.bufferEvent(msg.event)

	switch evt := msg.event.(type) {
	case domain.Join:
		updated, cmd = s.handleJoinEvent(evt)
	case domain.Part:
		updated, cmd = s.handlePartEvent(evt)
	case domain.Quit:
		updated, cmd = s.handleQuitEvent(evt)
	case domain.ModeChange:
		updated, cmd = s.handleModeChangeEvent(evt)
	case domain.Message:
		updated, cmd = s.handleMessageEvent(evt)
	case domain.TopicChange:
		updated, cmd = s.handleTopicChangeEvent(evt)
	case domain.NickChange:
		updated, cmd = s.handleNickChangeEvent(evt)
	case domain.ModelInvited:
		updated, cmd = s.handleModelInvitedEvent(evt)
	case domain.ModelKicked:
		updated, cmd = s.handleModelKickedEvent(evt)
	case domain.TopicInfo:
		updated, cmd = s.handleTopicInfoEvent(evt)
	case domain.DispatchStartedEvent:
		updated, cmd = s.handleDispatchStarted(evt)
	case domain.DispatchDoneEvent:
		updated, cmd = s.handleDispatchDone(evt)
	case domain.FocusChannelEvent:
		updated, cmd = s.handleFocusChannelEvent(evt)
	case domain.NamesReplyEvent:
		updated, cmd = s.handleNamesReply(evt)
	case domain.StatusOpenedEvent:
		updated, cmd = s.handleStatusOpened(evt)
	}

	if updated != nil {
		s = updated.(ChatScreen)
	}

	return s, tea.Batch(cmd, s.listenForProtocolEvents())
}

// bufferEvent appends a persistable event to the scrollback of
// the window(s) it belongs to. Live-event-driven: a focus change
// later is a pure buffer swap. `Message` routes via
// [domain.Message.RoutingKey] so DM traffic in either direction
// lands in the per-peer scrollback. `Quit` and `NickChange` fan
// into each channel in `Channels` plus any open DM with the
// actor. Other events are channel-keyed by their `Target`.
// SystemNoticeEvent unwraps its already-persisted inner event.
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
	case domain.Quit:
		s.bufferActorEvent(e.Channels, e.Instance, domain.StoredEvent{Event: e})
	case domain.NickChange:
		s.bufferActorEvent(e.Channels, e.Instance, domain.StoredEvent{Event: e})
	case domain.PersistableEvent:
		ch := domain.EventTarget(e)
		if ch == "" {
			return
		}

		s.appendToScrollback(ch, domain.StoredEvent{Event: e})
	}
}

// bufferActorEvent appends `stored` to each channel scrollback
// in `channels` plus any open DM whose counterpart is `actor`.
func (s ChatScreen) bufferActorEvent(channels []domain.ChannelName, actor *domain.Instance, stored domain.StoredEvent) {
	for _, ch := range channels {
		s.appendToScrollback(ch, stored)
	}

	if actor == nil {
		return
	}

	for w := range s.channels.All() {
		dm, ok := w.(*domain.DMWindow)
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
// mirrors `bufferActorEvent`: channels in `channels` plus any
// open DM whose counterpart is `actor`. The active window is
// skipped — the user is already looking at it.
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
		dm, ok := w.(*domain.DMWindow)
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
	s.scrollbackMu.Lock()
	defer s.scrollbackMu.Unlock()

	s.scrollback[ch] = append(s.scrollback[ch], evt)
}

// handleFocusChannelEvent handles a session-driven focus change.
// It delegates to the same path used for direct UI focus switches.
func (s ChatScreen) handleFocusChannelEvent(msg domain.FocusChannelEvent) (ui.Model, tea.Cmd) {
	return s.handleChannelFocus(chatcmd.ChannelFocusMsg{Channel: msg.Channel})
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
		// before — populate the cache with a fresh
		// `*ChannelWindow`. Status and DM windows arrive via
		// their own dedicated events and shouldn't hit this path.
		cw := domain.NewChannelWindow(msg.Channel, time.Time{})
		s.channels.Insert(cw)
		w = cw
	}

	*s.active = msg.Channel

	var members domain.MemberList
	if cw, ok := w.(*domain.ChannelWindow); ok {
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
		cmds = append(cmds, msgCmd(components.ChannelAddedMsg{Channel: w}))
	}

	cmds = append(cmds, msgCmd(components.ChannelActiveMsg{Channel: msg.Channel}))
	cmds = append(cmds, s.persistLastChannel(msg.Channel))
	cmds = append(cmds, msgCmd(components.ChannelUnreadMsg{Channel: msg.Channel, Count: 0}))
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))
	cmds = append(cmds, s.scrollbackCmd(msg.Channel))

	return s, tea.Sequence(cmds...)
}

// scrollbackCmd returns a command that hands the focused channel's
// in-memory buffer to the message list. The buffer is built up
// purely from live session events — the persisted event log is the
// models' shared memory and is never read here, so a freshly
// focused channel with no events seen this session shows nothing,
// matching IRC's "you don't see what happened before you joined"
// rule.
//
// The snapshot is taken at execution time (inside the returned
// closure) rather than when the command is constructed, because
// `tea.Sequence` interleaves the focus cmds with `sessionEventMsg`
// arrivals. By the time the message list receives the
// `HistoryLoadedMsg`, more events for `ch` may have landed in the
// buffer; we want to flush the latest contents so the list does
// not lose those late arrivals to the subsequent reset.
func (s ChatScreen) scrollbackCmd(ch domain.ChannelName) tea.Cmd {
	if ch == "" {
		return nil
	}

	return func() tea.Msg {
		s.scrollbackMu.RLock()
		defer s.scrollbackMu.RUnlock()

		return components.HistoryLoadedMsg{Events: s.scrollback[ch]}
	}
}

// persistLastChannel writes the user's currently-active channel to
// the store so a subsequent restart restores them to the same view.
// The chat screen owns this side-effect: the session no longer
// writes `last_channel` itself, keeping the persistent record
// consistent with what the user is actually looking at rather than
// with session-internal join coordination. An empty channel name
// (no active window) is a no-op so a /part that drops the user out
// of every channel does not race against the store.
func (s ChatScreen) persistLastChannel(ch domain.ChannelName) tea.Cmd {
	if ch == "" {
		return nil
	}

	return func() tea.Msg {
		if err := s.sess.SetLastChannel(s.ctx, ch); err != nil {
			slog.Default().ErrorContext(s.ctx, "persist last channel", "channel", ch, "error", err)
		}

		return nil
	}
}

// handleStatusOpened registers `&modeloff` in the sidebar without
// faking a channel-join lifecycle. The status window is a virtual
// server view: no members, no modes, no join/part. The chat screen
// only needs an addressable entry so the user can switch into it and
// see the server-narrated notices the session records there.
func (s ChatScreen) handleStatusOpened(msg domain.StatusOpenedEvent) (ui.Model, tea.Cmd) {
	if _, exists := s.windowByName(msg.Channel); exists {
		return s, nil
	}

	w := domain.NewStatusWindow(msg.At)
	s.channels.Insert(w)

	return s, msgCmd(components.ChannelAddedMsg{Channel: w})
}

// handleNamesReply applies the joiner-targeted member-list snapshot
// to the local channel cache and refreshes the nick list when the
// affected channel is the active one. Pre-existing members of the
// channel — models, other users — are otherwise invisible to the
// chat screen's cache; without this handler, switching to a freshly-
// joined channel would show only the user's own name.
func (s ChatScreen) handleNamesReply(msg domain.NamesReplyEvent) (ui.Model, tea.Cmd) {
	cw, ok := s.channelWindowByName(msg.Channel)
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

	cw.Members = msg.Members
	s.channels.Insert(cw)

	if msg.Channel != *s.active {
		return s, nil
	}

	return s, msgCmd(components.NickListUpdatedMsg{Members: cw.Members})
}

func (s ChatScreen) handleJoinEvent(msg domain.Join) (ui.Model, tea.Cmd) {
	isUser := msg.Instance == s.sess.UserInstance()

	cw, channelKnown := s.channelWindowByName(msg.Target)

	if !isUser && !channelKnown {
		return s, nil
	}

	if !channelKnown {
		// First user-join into this channel — populate the
		// chat-screen cache as the authoritative side, since
		// the session-emitted Join is what the chat screen
		// learns about the channel from.
		cw = domain.NewChannelWindow(msg.Target, msg.At)
	}

	if !cw.Members.HasInstance(msg.Instance) {
		cw.Members.Add(msg.Instance)
	}

	s.channels.Insert(cw)

	if !isUser {
		var cmds []tea.Cmd
		if msg.Target == *s.active {
			cmds = append(cmds,
				msgCmd(components.NickListUpdatedMsg{Members: cw.Members}),
				msgCmd(domain.StoredEvent{Event: msg}),
			)
		}

		return s, tea.Batch(cmds...)
	}

	s.checklist.channelCount = s.channels.Len()

	// For user joins, update the sidebar and member list. The
	// ChannelFocusEvent from switchChannel is the authoritative
	// source for active-channel switches, avoiding races when the
	// user switches channels rapidly. When the user joins their
	// already-active channel, also render the join inline so
	// scrollbackCmd's buffer-flush isn't the only path that surfaces
	// it (the live `bufferEvent` append already happened upstream).
	cmds := []tea.Cmd{
		msgCmd(components.ChannelAddedMsg{Channel: cw}),
		msgCmd(components.ChannelUnreadMsg{Channel: msg.Target, Count: 0}),
	}

	if msg.Target == *s.active {
		cmds = append(cmds, msgCmd(domain.StoredEvent{Event: msg}))
	}

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleModeChangeEvent(msg domain.ModeChange) (ui.Model, tea.Cmd) {
	cw, ok := s.channelWindowByName(msg.Target)
	if !ok {
		return s, nil
	}

	cw.Members.SetMode(msg.Instance, msg.Mode)
	s.channels.Insert(cw)

	if msg.Target != *s.active {
		return s, nil
	}

	return s, tea.Batch(
		msgCmd(components.NickListUpdatedMsg{Members: cw.Members}),
		msgCmd(domain.StoredEvent{Event: msg}),
	)
}

func (s ChatScreen) handlePartEvent(msg domain.Part) (ui.Model, tea.Cmd) {
	leavingActive := *s.active == msg.Target

	// Remove the member from the channel's member list.
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		if m, mOK := cw.Members.GetByInstance(msg.Instance); mOK {
			cw.Members.Remove(m)
		}

		s.channels.Insert(cw)
	}

	// If the user is leaving, remove the channel and purge any
	// pending paced messages queued for it. Already-scheduled ticks
	// for the parted channel's queue will no-op via
	// deliverNextPaced's empty-queue branch when they fire.
	if msg.Instance == s.sess.UserInstance() {
		s.channels.Remove(domain.WindowKey(msg.Target))
		delete(s.pacedQueue, msg.Target)
		s.checklist.channelCount = s.channels.Len()
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.ChannelRemovedMsg{Channel: msg.Target}))

	if leavingActive {
		if s.channels.Len() > 0 {
			first, _ := s.channels.GetAt(0)
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
	if leavingActive {
		cmds = append(cmds, s.scrollbackCmd(*s.active))
	}

	if !leavingActive && *s.active == msg.Target {
		cmds = append(cmds, msgCmd(domain.StoredEvent{
			Event: domain.Part{
				Target:  msg.Target,
				Nick:    msg.Instance.Nick(),
				Message: msg.Message,
				At:      msg.At,
			},
		}))
	}

	return s, tea.Sequence(cmds...)
}

func (s ChatScreen) handleQuitEvent(msg domain.Quit) (ui.Model, tea.Cmd) {
	// `bufferEvent` has already fanned the line into every
	// affected channel and any open DM with the actor. The
	// handler updates the in-memory `Members` snapshot for each
	// affected channel and fires the active-window UI refresh.

	for _, ch := range msg.Channels {
		cw, ok := s.channelWindowByName(ch)
		if !ok {
			continue
		}

		if m, mOK := cw.Members.GetByInstance(msg.Instance); mOK {
			cw.Members.Remove(m)
			s.channels.Insert(cw)
		}
	}

	var cmds []tea.Cmd

	for _, ch := range msg.Channels {
		if ch != *s.active {
			continue
		}

		if cw, ok := s.channelWindowByName(*s.active); ok {
			cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: cw.Members}))
		}

		cmds = append(cmds, msgCmd(domain.StoredEvent{Event: msg}))
	}

	// Also surface into the active window if it's an open DM
	// with the quitter.
	if dm, ok := s.activeDMWith(msg.Instance); ok && dm.Name() == *s.active {
		cmds = append(cmds, msgCmd(domain.StoredEvent{Event: msg}))
	}

	cmds = append(cmds, s.lifecycleBumps(msg.Channels, msg.Instance)...)

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleTopicChangeEvent(msg domain.TopicChange) (ui.Model, tea.Cmd) {
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		cw.Topic = msg.Topic
		cw.TopicSetBy = msg.By
		cw.TopicSetAt = msg.At
		s.channels.Insert(cw)
	}

	if *s.active != msg.Target {
		return s, nil
	}

	return s, tea.Batch(
		msgCmd(components.TopicUpdatedMsg{Topic: msg.Topic}),
		msgCmd(domain.StoredEvent{
			Event: domain.TopicChange(msg),
		}),
	)
}

func (s ChatScreen) handleTopicInfoEvent(msg domain.TopicInfo) (ui.Model, tea.Cmd) {
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		cw.Topic = msg.Topic
		cw.TopicSetBy = msg.TopicSetBy
		cw.TopicSetAt = msg.TopicSetAt
		s.channels.Insert(cw)
	}

	if *s.active != msg.Target {
		return s, nil
	}

	return s, tea.Batch(
		msgCmd(components.SetChannelMsg{
			Channel: msg.Target,
			Topic:   msg.Topic,
			Kind:    s.activeKind(),
		}),
		s.logAndShow(domain.TopicInfo(msg)),
	)
}

func (s ChatScreen) handleNickChangeEvent(msg domain.NickChange) (ui.Model, tea.Cmd) {
	// `msg.Instance.Nick()` is already the new value — the
	// session renames before emitting. Update the snapshot in
	// each affected channel's member list, then fire the
	// active-window UI side-effects exactly once.
	for _, ch := range msg.Channels {
		cw, ok := s.channelWindowByName(ch)
		if !ok {
			continue
		}

		if cw.Members.HasInstance(msg.Instance) {
			cw.Members.RenameTo(msg.Instance, msg.NewNick)
			s.channels.Insert(cw)
		}
	}

	var cmds []tea.Cmd

	activeIsChannel := slices.Contains(msg.Channels, *s.active)

	activeDM, activeIsDM := s.activeDMWith(msg.Instance)
	activeDMVisible := activeIsDM && activeDM.Name() == *s.active

	if activeIsChannel || activeDMVisible {
		if activeIsChannel {
			if cw, ok := s.channelWindowByName(*s.active); ok {
				cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: cw.Members}))
			}
		}

		cmds = append(cmds, msgCmd(domain.StoredEvent{Event: msg}))

		if msg.Instance == s.sess.UserInstance() {
			cmds = append(cmds, msgCmd(components.UserNickMsg{Nick: msg.NewNick}))
		}

		nickCfg, _ := s.loadConfig()
		cmds = append(cmds, msgCmd(components.HighlightWordsMsg{
			Words:    nickCfg.HighlightWords,
			UserNick: s.sess.UserNick(),
		}))
	}

	cmds = append(cmds, s.lifecycleBumps(msg.Channels, msg.Instance)...)

	return s, tea.Batch(cmds...)
}

// activeDMWith returns the open DM whose counterpart is `actor`,
// if any.
func (s ChatScreen) activeDMWith(actor *domain.Instance) (*domain.DMWindow, bool) {
	if actor == nil {
		return nil, false
	}

	for w := range s.channels.All() {
		dm, ok := w.(*domain.DMWindow)
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

		s.channels.Insert(cw)
	}

	var members domain.MemberList

	if cw, ok := s.channelWindowByName(*s.active); ok {
		members = cw.Members
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))

	if *s.active == msg.Target {
		cmds = append(cmds, msgCmd(domain.StoredEvent{
			Event: domain.ModelInvited{
				Target: msg.Target,
				Nick:   msg.Instance.Nick(),
				By:     msg.By,
				At:     msg.At,
			},
		}))
	}

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleModelKickedEvent(msg domain.ModelKicked) (ui.Model, tea.Cmd) {
	// Remove the kicked member from the channel's member list.
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		if m, mOK := cw.Members.GetByInstance(msg.Instance); mOK {
			cw.Members.Remove(m)
		}

		s.channels.Insert(cw)
	}

	var members domain.MemberList

	if cw, ok := s.channelWindowByName(*s.active); ok {
		members = cw.Members
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))

	if *s.active == msg.Target {
		cmds = append(cmds, msgCmd(domain.StoredEvent{
			Event: domain.ModelKicked{
				Target: msg.Target,
				Nick:   msg.Instance.Nick(),
				By:     msg.By,
				At:     msg.At,
			},
		}))
	}

	return s, tea.Batch(cmds...)
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

// renderMessage produces the tea.Cmd that surfaces a Message in the
// active view (or as an unread badge for an off-channel target).
func (s ChatScreen) renderMessage(msg domain.Message, key domain.ChannelName) tea.Cmd {
	if key == *s.active {
		return msgCmd(domain.StoredEvent{Event: msg})
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

	_, alreadyOpen := s.channels.Get(domain.WindowKey(name))
	s.channels.Insert(dm)

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
		cmds = append(cmds, s.scrollbackCmd(name))
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

	// Status-channel guard refusals are user-fixable contextual
	// errors, not failures: render them as a hint with the
	// command-tagged usage text rather than a red command-error.
	var guard domain.StatusChannelGuardError
	if errors.As(msg.Err, &guard) {
		cmds = append(cmds, s.logAndShow(domain.UsageHint{
			Target:  *s.active,
			Command: guard.Command,
			Usage:   guard.Hint,
			At:      msg.At,
		}))
	} else {
		cmds = append(cmds, s.logAndShow(domain.CommandError{
			Target: *s.active,
			Err:    fmt.Sprintf("%s: %s", msg.Operation, msg.Err),
			At:     msg.At,
		}))
	}

	cmds = append(cmds, msgCmd(components.PendingResponseMsg{Pending: false}))
	cmds = append(cmds, msgCmd(components.NickListThinkingMsg{}))

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleDispatchStarted(msg domain.DispatchStartedEvent) (ui.Model, tea.Cmd) {
	thinking := make(map[domain.Nick]bool, len(msg.Nicks))
	for _, nick := range msg.Nicks {
		thinking[nick] = true
	}

	return s, tea.Batch(
		msgCmd(components.PendingResponseMsg{Pending: true}),
		msgCmd(components.NickListThinkingMsg{Nicks: thinking}),
	)
}

func (s ChatScreen) handleDispatchDone(_ domain.DispatchDoneEvent) (ui.Model, tea.Cmd) {
	if s.hasQueuedPaced() {
		return s, nil
	}

	return s, tea.Batch(
		msgCmd(components.NickListThinkingMsg{}),
		msgCmd(components.PendingResponseMsg{Pending: false}),
	)
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
// load failures: empty the cached suggestions so tab completion
// degrades to known nicks, then surface the reason to the user as a
// channel system notice — routed to the status channel when no
// user-visible channel is focused.
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

// windowByName returns the cached `Window` for the given name.
func (s ChatScreen) windowByName(name domain.ChannelName) (domain.Window, bool) {
	return s.channels.Get(domain.WindowKey(name))
}

// channelWindowByName looks up the cached entry and asserts it
// is a `*ChannelWindow`. Returns false either way for non-
// channel kinds (status / DM) or absent entries; the channel-
// only handlers (`handleJoinEvent`, `handleModeChangeEvent`,
// etc.) use this to read and mutate `Members` / `Topic`
// without going through the legacy `Channel` projection.
func (s ChatScreen) channelWindowByName(name domain.ChannelName) (*domain.ChannelWindow, bool) {
	w, ok := s.windowByName(name)
	if !ok {
		return nil, false
	}

	cw, ok := w.(*domain.ChannelWindow)
	return cw, ok
}
