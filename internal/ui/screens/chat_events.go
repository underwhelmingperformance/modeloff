package screens

import (
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
)

func (s ChatScreen) handleSessionEvent(msg sessionEventMsg) (ui.Model, tea.Cmd) {
	var (
		updated ui.Model
		cmd     tea.Cmd
	)

	// Persist persistable events into the per-channel scrollback
	// before the type-specific handler runs. The handler may emit a
	// `domain.StoredEvent` for the active channel; the buffer
	// captures the same payload regardless of focus, so a switch to
	// a non-active channel later renders the events in order.
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
	case domain.ConfigChangedEvent:
		updated, cmd = s.handleConfigChangedEvent(evt)
	case domain.DMOpenedEvent:
		updated, cmd = s.handleDMOpenedEvent(evt)
	case domain.DispatchStartedEvent:
		updated, cmd = s.handleDispatchStarted(evt)
	case domain.ModelReplyEvent:
		updated, cmd = s.handleModelReplyEvent(evt)
	case domain.DispatchDoneEvent:
		updated, cmd = s.handleDispatchDone(evt)
	case domain.ErrorEvent:
		updated, cmd = s.handleErrorEvent(evt)
	case domain.FocusChannelEvent:
		updated, cmd = s.handleFocusChannelEvent(evt)
	case domain.SystemNoticeEvent:
		updated, cmd = s.handleSystemNoticeEvent(evt)
	case domain.NamesReplyEvent:
		updated, cmd = s.handleNamesReply(evt)
	case domain.StatusOpenedEvent:
		updated, cmd = s.handleStatusOpened(evt)
	}

	if updated != nil {
		s = updated.(ChatScreen)
	}

	return s, tea.Batch(cmd, s.listenForEvents())
}

// bufferEvent appends a persistable event to the scrollback for its
// target channel. The buffer is purely live-event-driven: every
// persistable event the user's session sees during this run lands
// here, regardless of which channel is active, so a later focus
// change is a pure swap. The persisted event log is the models'
// shared memory and is never read into this buffer — the user only
// sees events from this session's join onward, mirroring IRC's
// "you don't see what happened before you joined" rule.
//
// SystemNoticeEvent is special-cased because it is a UI carrier
// for an already-persisted SystemNotice; the wrapped
// Stored.Event lands in the buffer.
func (s ChatScreen) bufferEvent(evt domain.Event) {
	switch e := evt.(type) {
	case domain.SystemNoticeEvent:
		s.appendToScrollback(e.Channel, e.Stored)
	case domain.PersistableEvent:
		ch := domain.EventTarget(e)
		if ch == "" {
			return
		}

		s.appendToScrollback(ch, domain.StoredEvent{Event: e})
	}
}

func (s ChatScreen) appendToScrollback(ch domain.ChannelName, evt domain.StoredEvent) {
	s.scrollback[ch] = append(s.scrollback[ch], evt)
}

// handleFocusChannelEvent handles a session-driven focus change.
// It delegates to the same path used for direct UI focus switches.
func (s ChatScreen) handleFocusChannelEvent(msg domain.FocusChannelEvent) (ui.Model, tea.Cmd) {
	return s.handleChannelFocus(domain.ChannelFocusEvent{Channel: msg.Channel})
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

func (s ChatScreen) handleChannelFocus(msg domain.ChannelFocusEvent) (ui.Model, tea.Cmd) {
	ch, exists := s.channelByName(msg.Channel)
	if !exists {
		ch = s.syntheticChannel(msg.Channel)
		s.insertChannelCache(ch)
	}

	*s.active = msg.Channel

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.SetPlaceholderMsg{}))
	cmds = append(cmds, msgCmd(components.SetChannelMsg{
		Channel: msg.Channel,
		Topic:   s.activeTopic(),
		Kind:    ch.Kind,
	}))

	if !exists {
		cmds = append(cmds, msgCmd(components.ChannelAddedMsg{Channel: ch}))
	}

	cmds = append(cmds, msgCmd(components.ChannelActiveMsg{Channel: msg.Channel}))
	cmds = append(cmds, s.persistLastChannel(msg.Channel))
	cmds = append(cmds, msgCmd(components.ChannelUnreadMsg{Channel: msg.Channel, Count: 0}))
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: ch.Members}))
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
	if _, exists := s.channelByName(msg.Channel); exists {
		return s, nil
	}

	ch := domain.Channel{
		Name:    msg.Channel,
		Kind:    domain.KindStatus,
		Members: domain.NewMemberList(),
		Created: msg.At,
	}
	s.insertChannelCache(ch)

	return s, msgCmd(components.ChannelAddedMsg{Channel: ch})
}

// handleNamesReply applies the joiner-targeted member-list snapshot
// to the local channel cache and refreshes the nick list when the
// affected channel is the active one. Pre-existing members of the
// channel — models, other users — are otherwise invisible to the
// chat screen's cache; without this handler, switching to a freshly-
// joined channel would show only the user's own name.
func (s ChatScreen) handleNamesReply(msg domain.NamesReplyEvent) (ui.Model, tea.Cmd) {
	ch, exists := s.channelByName(msg.Channel)
	if !exists {
		ch = s.syntheticChannel(msg.Channel)
	}

	ch.Members = msg.Members
	s.insertChannelCache(ch)

	if msg.Channel != *s.active {
		return s, nil
	}

	return s, msgCmd(components.NickListUpdatedMsg{Members: ch.Members})
}

func (s ChatScreen) handleJoinEvent(msg domain.Join) (ui.Model, tea.Cmd) {
	isUser := msg.Instance == s.sess.UserInstance()
	_, channelKnown := s.channelByName(msg.Target)

	if !isUser && !channelKnown {
		return s, nil
	}

	ch, exists := s.channelByName(msg.Target)
	if !exists {
		ch = s.syntheticChannel(msg.Target)
		ch.Created = msg.At
	}

	if !ch.Members.HasInstance(msg.Instance) {
		ch.Members.Add(msg.Instance)
	}

	s.insertChannelCache(ch)

	if !isUser {
		var cmds []tea.Cmd
		if msg.Target == *s.active {
			cmds = append(cmds,
				msgCmd(components.NickListUpdatedMsg{Members: ch.Members}),
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
		msgCmd(components.ChannelAddedMsg{Channel: ch}),
		msgCmd(components.ChannelUnreadMsg{Channel: msg.Target, Count: 0}),
	}

	if msg.Target == *s.active {
		cmds = append(cmds, msgCmd(domain.StoredEvent{Event: msg}))
	}

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleModeChangeEvent(msg domain.ModeChange) (ui.Model, tea.Cmd) {
	ch, ok := s.channelByName(msg.Target)
	if !ok {
		return s, nil
	}

	ch.Members.SetMode(msg.Instance, msg.Mode)
	s.insertChannelCache(ch)

	if msg.Target != *s.active {
		return s, nil
	}

	return s, tea.Batch(
		msgCmd(components.NickListUpdatedMsg{Members: ch.Members}),
		msgCmd(domain.StoredEvent{Event: msg}),
	)
}

func (s ChatScreen) handlePartEvent(msg domain.Part) (ui.Model, tea.Cmd) {
	leavingActive := *s.active == msg.Target

	// Remove the member from the channel's member list.
	if ch, ok := s.channelByName(msg.Target); ok {
		if m, mOK := ch.Members.GetByInstance(msg.Instance); mOK {
			ch.Members.Remove(m)
		}

		s.insertChannelCache(ch)
	}

	// If the user is leaving, remove the channel and purge any
	// pending replies queued for it. Already-scheduled ticks for the
	// parted channel's queue will no-op via deliverNextReply's
	// empty-queue branch when they fire.
	if msg.Instance == s.sess.UserInstance() {
		s.channels.Remove(s.channelKey(msg.Target))
		delete(s.replyQueue, msg.Target)
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
		if ch, ok := s.channelByName(*s.active); ok {
			members = ch.Members
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
	// The session emits one quit event per channel the actor was
	// in (the IRC intersection rule, denormalised per-channel for
	// modeloff's persistence shape). `bufferEvent` has already
	// appended this event to its target's scrollback. The
	// handler's job is the in-memory Members update for that
	// target, plus the live nick-list and message-list refresh
	// when the target is the active window.

	if ch, ok := s.channelByName(msg.Target); ok {
		if m, mOK := ch.Members.GetByInstance(msg.Instance); mOK {
			ch.Members.Remove(m)
			s.insertChannelCache(ch)
		}
	}

	if msg.Target != *s.active {
		return s, nil
	}

	var cmds []tea.Cmd

	if ch, ok := s.channelByName(*s.active); ok {
		cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: ch.Members}))
	}

	cmds = append(cmds, msgCmd(domain.StoredEvent{Event: msg}))

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleTopicChangeEvent(msg domain.TopicChange) (ui.Model, tea.Cmd) {
	if ch, ok := s.channelByName(msg.Target); ok {
		ch.Topic = msg.Topic
		ch.TopicSetBy = msg.By
		ch.TopicSetAt = msg.At
		s.insertChannelCache(ch)
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
	if ch, ok := s.channelByName(msg.Target); ok {
		ch.Topic = msg.Topic
		ch.TopicSetBy = msg.TopicSetBy
		ch.TopicSetAt = msg.TopicSetAt
		s.insertChannelCache(ch)
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
	// Update the nick snapshot in this channel's local member list.
	// The instance's own Nick() is already the new value — the
	// session mutated it before emitting the event. The session
	// emits one nick-change per channel the actor was in (the IRC
	// intersection rule), so the handler only needs to update the
	// target window's snapshot, not iterate.
	if ch, ok := s.channelByName(msg.Target); ok {
		if ch.Members.HasInstance(msg.Instance) {
			ch.Members.RenameTo(msg.Instance, msg.NewNick)
			s.insertChannelCache(ch)
		}
	}

	var cmds []tea.Cmd

	// The active-window UI updates (nick list, message list,
	// user-nick banner, highlight words) only need to fire once
	// even though the session emits one event per channel the
	// actor was in. Gate them on the target being the active
	// window so they fire exactly once per rename — for the
	// active-targeted event in the per-channel iteration —
	// regardless of how many channels the actor was in.
	if msg.Target != *s.active {
		return s, nil
	}

	if ch, ok := s.channelByName(*s.active); ok {
		cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: ch.Members}))
	}

	cmds = append(cmds, msgCmd(domain.StoredEvent{
		Event: domain.NickChange{
			Target:  msg.Target,
			OldNick: msg.OldNick,
			NewNick: msg.NewNick,
			At:      msg.At,
		},
	}))

	if msg.Instance == s.sess.UserInstance() {
		cmds = append(cmds, msgCmd(components.UserNickMsg{Nick: msg.NewNick}))
	}

	nickCfg, _ := s.loadConfig()
	cmds = append(cmds, msgCmd(components.HighlightWordsMsg{
		Words:    nickCfg.HighlightWords,
		UserNick: s.sess.UserNick(),
	}))

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleModelInvitedEvent(msg domain.ModelInvited) (ui.Model, tea.Cmd) {
	if ch, ok := s.channelByName(msg.Target); ok {
		if !ch.Members.HasInstance(msg.Instance) {
			ch.Members.Add(msg.Instance)
		}

		s.insertChannelCache(ch)
	}

	var members domain.MemberList

	if ch, ok := s.channelByName(*s.active); ok {
		members = ch.Members
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
	if ch, ok := s.channelByName(msg.Target); ok {
		if m, mOK := ch.Members.GetByInstance(msg.Instance); mOK {
			ch.Members.Remove(m)
		}

		s.insertChannelCache(ch)
	}

	var members domain.MemberList

	if ch, ok := s.channelByName(*s.active); ok {
		members = ch.Members
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

func (s ChatScreen) handleMessageEvent(msg domain.Message) (ui.Model, tea.Cmd) {
	event := domain.StoredEvent{Event: msg}

	if msg.Target == *s.active {
		return s, msgCmd(event)
	}

	count, _ := s.sess.UnreadCount(s.ctx, msg.Target)
	mention := s.isHighlight(msg.Body)

	return s, msgCmd(components.ChannelUnreadMsg{Channel: msg.Target, Count: count, Mention: mention})
}

func (s ChatScreen) handleModelReplyEvent(msg domain.ModelReplyEvent) (ui.Model, tea.Cmd) {
	ch := msg.Channel
	wasEmpty := len(s.replyQueue[ch]) == 0
	s.replyQueue[ch] = append(s.replyQueue[ch], msg)

	// If this channel had no pending replies, deliver immediately;
	// pacing is per-channel, so unrelated channels keep their own
	// schedules.
	if wasEmpty {
		return s, s.deliverNextReplyCmd(ch)
	}

	return s, nil
}

func (s ChatScreen) handleDMOpenedEvent(msg domain.DMOpenedEvent) (ui.Model, tea.Cmd) {
	name := msg.DM.Name()
	*s.active = name

	// The chat-screen's channel cache is still keyed by the
	// `domain.Channel` projection while the rest of the UI is on
	// the legacy shape. Insert the projection here so the sidebar
	// finds the DM by name; the projection's `Members` is empty
	// (DMs carry their counterpart by handle, not via the member
	// list) and that's fine — the nick-list pane is a no-op for
	// DM windows.
	ch := domain.ChannelFromWindow(msg.DM)
	s.insertChannelCache(ch)

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.SetPlaceholderMsg{}))
	cmds = append(cmds, msgCmd(components.SetChannelMsg{
		Channel: name,
		Topic:   s.activeTopic(),
		Kind:    domain.KindDM,
	}))
	cmds = append(cmds, msgCmd(components.ChannelAddedMsg{Channel: ch}))
	cmds = append(cmds, msgCmd(components.ChannelActiveMsg{Channel: name}))
	cmds = append(cmds, s.persistLastChannel(name))
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: domain.MemberList{}}))
	cmds = append(cmds, s.scrollbackCmd(name))
	cmds = append(cmds, s.logAndShow(domain.SystemNotice{
		Target: name,
		Text:   fmt.Sprintf("Opened direct message with %s", msg.DM.Counterpart.Nick()),
		At:     msg.At,
	}))

	return s, tea.Sequence(cmds...)
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
	if s.hasQueuedReplies() {
		return s, nil
	}

	return s, tea.Batch(
		msgCmd(components.NickListThinkingMsg{}),
		msgCmd(components.PendingResponseMsg{Pending: false}),
	)
}

const replyPaceInterval = 400 * time.Millisecond

// hasQueuedReplies reports whether any channel has pending replies.
// The pending/thinking indicators are application-wide, so they
// clear only when every channel's queue has drained. The field's
// pruning invariant (drained channels are deleted from the map)
// makes this an O(1) length check.
func (s ChatScreen) hasQueuedReplies() bool {
	return len(s.replyQueue) > 0
}

func (s ChatScreen) scheduleNextReply(ch domain.ChannelName) tea.Cmd {
	return tea.Tick(replyPaceInterval, func(time.Time) tea.Msg {
		return deliverNextReplyMsg{Channel: ch}
	})
}

// deliverNextReplyCmd returns a tea.Cmd that delivers the next reply
// from the given channel's queue immediately (without pacing delay).
func (s ChatScreen) deliverNextReplyCmd(ch domain.ChannelName) tea.Cmd {
	return func() tea.Msg { return deliverNextReplyMsg{Channel: ch} }
}

func (s ChatScreen) deliverNextReply(msg deliverNextReplyMsg) (ui.Model, tea.Cmd) {
	queue := s.replyQueue[msg.Channel]
	if len(queue) == 0 {
		// The channel's queue has drained. If no other channel has
		// pending replies either, clear the application-wide
		// pending/thinking indicators.
		if !s.hasQueuedReplies() {
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
		delete(s.replyQueue, msg.Channel)
	} else {
		s.replyQueue[msg.Channel] = queue
	}

	updated, cmd := s.showReply(next)
	s = updated.(ChatScreen)

	// Schedule the next delivery for this channel if more remain
	// here; otherwise, if every channel has drained, clear the
	// application-wide pending indicators.
	if len(s.replyQueue[msg.Channel]) > 0 {
		cmd = tea.Batch(cmd, s.scheduleNextReply(msg.Channel))
	} else if !s.hasQueuedReplies() {
		cmd = tea.Batch(cmd,
			msgCmd(components.NickListThinkingMsg{}),
			msgCmd(components.PendingResponseMsg{Pending: false}),
		)
	}

	return s, cmd
}

func (s ChatScreen) showReply(msg domain.ModelReplyEvent) (ui.Model, tea.Cmd) {
	event := domain.StoredEvent{Event: msg.Event}

	if msg.Channel == *s.active {
		return s, msgCmd(event)
	}

	count, _ := s.sess.UnreadCount(s.ctx, msg.Channel)
	mention := s.isHighlight(msg.Event.Body)

	return s, msgCmd(components.ChannelUnreadMsg{Channel: msg.Channel, Count: count, Mention: mention})
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

	ch, ok := s.channelByName(*s.active)
	if !ok {
		return ""
	}

	return ch.Topic
}

func (s ChatScreen) activeKind() domain.ChannelKind {
	if *s.active == "" {
		return domain.KindChannel
	}

	ch, ok := s.channelByName(*s.active)
	if !ok {
		return domain.InferChannelKind(*s.active)
	}

	return ch.Kind
}

func (s ChatScreen) activeMemberNicks() iter.Seq[domain.Nick] {
	ch, ok := s.channelByName(*s.active)
	if !ok {
		return func(func(domain.Nick) bool) {}
	}

	return ch.Members.Nicks()
}

// activeChannelInstances iterates the `*Instance` handles for every
// member of the currently-active channel. Tab completion sources this
// iterator: the user only gets completions for nicks they can already
// see in their nick list, matching IRC semantics.
func (s ChatScreen) activeChannelInstances() iter.Seq[*domain.Instance] {
	return func(yield func(*domain.Instance) bool) {
		ch, ok := s.channelByName(*s.active)
		if !ok {
			return
		}

		for _, m := range ch.Members.All() {
			if !yield(m.Instance) {
				return
			}
		}
	}
}

// windowByName returns the cached `Window` for the given name,
// projected back to a `Channel` for the legacy callers in this
// file that still read `Members` / `Topic` off the struct shape.
// During the in-flight migration the chat screen's storage is
// `Window`-typed but the per-handler logic still mutates a
// `Channel` projection; the caller writes the mutated projection
// back via `s.channels.Insert(...)`, which adapts via
// `domain.WindowFromChannel`.
func (s ChatScreen) channelByName(name domain.ChannelName) (domain.Channel, bool) {
	w, ok := s.channels.Get(domain.WindowKey(name))
	if !ok {
		return domain.Channel{}, false
	}

	return domain.ChannelFromWindow(w), true
}

func (s ChatScreen) channelKey(name domain.ChannelName) domain.Window {
	return domain.WindowKey(name)
}

func (s ChatScreen) syntheticChannel(name domain.ChannelName) domain.Channel {
	return domain.Channel{
		Name:    name,
		Kind:    domain.InferChannelKind(name),
		Members: domain.NewMemberList(),
	}
}

// insertChannelCache projects a `domain.Channel` back to a typed
// `Window` and inserts it into the chat-screen's cache. The
// chat-screen handlers still operate on `Channel` projections
// for legacy reasons; this is the single bridge so callers don't
// have to repeat the projection at every site.
func (s ChatScreen) insertChannelCache(ch domain.Channel) {
	w, err := domain.WindowFromChannel(ch, func(nick domain.Nick) *domain.Instance {
		resolved, err := s.sess.ResolveNick(s.ctx, nick)
		if err != nil {
			return nil
		}

		return resolved
	})
	if err != nil {
		// Fall back to a key window — the cache still works for
		// lookup-by-name even if the per-kind state is empty.
		w = domain.WindowKey(ch.Name)
	}

	s.channels.Insert(w)
}
