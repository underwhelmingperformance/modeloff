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

	switch evt := msg.event.(type) {
	case domain.JoinEvent:
		updated, cmd = s.handleJoinEvent(evt)
	case domain.PartEvent:
		updated, cmd = s.handlePartEvent(evt)
	case domain.QuitEvent:
		updated, cmd = s.handleQuitEvent(evt)
	case domain.ModeChangeEvent:
		updated, cmd = s.handleModeChangeEvent(evt)
	case domain.MessageEvent:
		updated, cmd = s.handleMessageEvent(evt)
	case domain.TopicChangeEvent:
		updated, cmd = s.handleTopicChangeEvent(evt)
	case domain.NickChangeEvent:
		updated, cmd = s.handleNickChangeEvent(evt)
	case domain.ModelInvitedEvent:
		updated, cmd = s.handleModelInvitedEvent(evt)
	case domain.ModelKickedEvent:
		updated, cmd = s.handleModelKickedEvent(evt)
	case domain.TopicInfoEvent:
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
	}

	if updated != nil {
		s = updated.(ChatScreen)
	}

	return s, tea.Batch(cmd, s.listenForEvents())
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
		s.channels.Insert(ch)
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
	cmds = append(cmds, msgCmd(components.ChannelUnreadMsg{Channel: msg.Channel, Count: 0}))
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: ch.Members}))
	cmds = append(cmds, s.fetchHistoryAfter(msg.Channel, s.sess.UserJoinedAt(msg.Channel)))

	return s, tea.Sequence(cmds...)
}

func (s ChatScreen) handleJoinEvent(msg domain.JoinEvent) (ui.Model, tea.Cmd) {
	isUser := msg.Instance == s.sess.UserInstance()
	_, channelKnown := s.channelByName(msg.Channel)

	if !isUser && !channelKnown {
		return s, nil
	}

	ch, exists := s.channelByName(msg.Channel)
	if !exists {
		ch = s.syntheticChannel(msg.Channel)
		ch.Created = msg.At
	}

	if !ch.Members.HasInstance(msg.Instance) {
		ch.Members.Add(msg.Instance)
	}

	s.channels.Insert(ch)

	if !isUser {
		if msg.Channel == *s.active {
			return s, msgCmd(components.NickListUpdatedMsg{Members: ch.Members})
		}

		return s, nil
	}

	s.checklist.channelCount = s.channels.Len()

	// For user joins, update the sidebar and member list only. The
	// ChannelFocusEvent from switchChannel is the authoritative
	// source for active-channel switches, avoiding races when the
	// user switches channels rapidly.
	return s, tea.Batch(
		msgCmd(components.ChannelAddedMsg{Channel: ch}),
		msgCmd(components.ChannelUnreadMsg{Channel: msg.Channel, Count: 0}),
	)
}

func (s ChatScreen) handleModeChangeEvent(msg domain.ModeChangeEvent) (ui.Model, tea.Cmd) {
	ch, ok := s.channelByName(msg.Channel)
	if !ok {
		return s, nil
	}

	ch.Members.SetMode(msg.Instance, msg.Mode)
	s.channels.Insert(ch)

	if msg.Channel != *s.active {
		return s, nil
	}

	return s, msgCmd(components.NickListUpdatedMsg{Members: ch.Members})
}

func (s ChatScreen) handlePartEvent(msg domain.PartEvent) (ui.Model, tea.Cmd) {
	leavingActive := *s.active == msg.Channel

	// Remove the member from the channel's member list.
	if ch, ok := s.channelByName(msg.Channel); ok {
		if m, mOK := ch.Members.GetByInstance(msg.Instance); mOK {
			ch.Members.Remove(m)
		}

		s.channels.Insert(ch)
	}

	// If the user is leaving, remove the channel and purge any
	// pending replies queued for it. Already-scheduled ticks for the
	// parted channel's queue will no-op via deliverNextReply's
	// empty-queue branch when they fire.
	if msg.Instance == s.sess.UserInstance() {
		s.channels.Remove(s.channelKey(msg.Channel))
		delete(s.replyQueue, msg.Channel)
		s.checklist.channelCount = s.channels.Len()
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.ChannelRemovedMsg{Channel: msg.Channel}))

	if leavingActive {
		if s.channels.Len() > 0 {
			first, _ := s.channels.GetAt(0)
			*s.active = first.Name
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
	}

	var members domain.MemberList

	if *s.active != "" {
		if ch, ok := s.channelByName(*s.active); ok {
			members = ch.Members
		}
	}

	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))
	if leavingActive {
		cmds = append(cmds, s.fetchHistoryAfter(*s.active, s.sess.UserJoinedAt(*s.active)))
	}

	if !leavingActive && *s.active == msg.Channel {
		cmds = append(cmds, msgCmd(domain.StoredEvent{
			Event: domain.ChannelPart{
				Channel: msg.Channel,
				Nick:    msg.Instance.Nick(),
				Message: msg.Message,
				At:      msg.At,
			},
		}))
	}

	return s, tea.Sequence(cmds...)
}

func (s ChatScreen) handleQuitEvent(msg domain.QuitEvent) (ui.Model, tea.Cmd) {
	// Remove the quitter from every channel's member list.
	for ch := range s.channels.All() {
		if m, ok := ch.Members.GetByInstance(msg.Instance); ok {
			ch.Members.Remove(m)
			// re-insert to preserve sort order; the Members
			// mutation is already visible via the shared map.
			s.channels.Insert(ch)
		}
	}

	var cmds []tea.Cmd

	// Update nick list for the active channel.
	var members domain.MemberList

	if *s.active != "" {
		if ch, ok := s.channelByName(*s.active); ok {
			members = ch.Members
		}
	}

	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))

	// Show the quit event in the active channel.
	if *s.active != "" {
		cmds = append(cmds, s.logAndShow(domain.ChannelQuit{
			Channel: *s.active,
			Nick:    msg.Instance.Nick(),
			Message: msg.Message,
			At:      msg.At,
		}))
	}

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleTopicChangeEvent(msg domain.TopicChangeEvent) (ui.Model, tea.Cmd) {
	if ch, ok := s.channelByName(msg.Channel); ok {
		ch.Topic = msg.Topic
		ch.TopicSetBy = msg.By
		ch.TopicSetAt = msg.At
		s.channels.Insert(ch)
	}

	if *s.active != msg.Channel {
		return s, nil
	}

	return s, tea.Batch(
		msgCmd(components.TopicUpdatedMsg{Topic: msg.Topic}),
		msgCmd(domain.StoredEvent{
			Event: domain.ChannelTopicChange(msg),
		}),
	)
}

func (s ChatScreen) handleTopicInfoEvent(msg domain.TopicInfoEvent) (ui.Model, tea.Cmd) {
	if ch, ok := s.channelByName(msg.Channel); ok {
		ch.Topic = msg.Topic
		ch.TopicSetBy = msg.TopicSetBy
		ch.TopicSetAt = msg.TopicSetAt
		s.channels.Insert(ch)
	}

	if *s.active != msg.Channel {
		return s, nil
	}

	return s, tea.Batch(
		msgCmd(components.SetChannelMsg{
			Channel: msg.Channel,
			Topic:   msg.Topic,
			Kind:    s.activeKind(),
		}),
		s.logAndShow(domain.ChannelTopicInfo(msg)),
	)
}

func (s ChatScreen) handleNickChangeEvent(msg domain.NickChangeEvent) (ui.Model, tea.Cmd) {
	// Update the nick snapshot in this channel's local member list.
	// The instance's own Nick() is already the new value — the
	// session mutated it before emitting the event.
	if ch, ok := s.channelByName(msg.Channel); ok {
		if ch.Members.HasInstance(msg.Instance) {
			ch.Members.RenameTo(msg.Instance, msg.NewNick)
			s.channels.Insert(ch)
		}
	}

	if msg.Channel != *s.active {
		return s, nil
	}

	var cmds []tea.Cmd

	if ch, ok := s.channelByName(*s.active); ok {
		cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: ch.Members}))
	}

	cmds = append(cmds, msgCmd(domain.StoredEvent{
		Event: domain.ChannelNickChange{
			Channel: msg.Channel,
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

func (s ChatScreen) handleModelInvitedEvent(msg domain.ModelInvitedEvent) (ui.Model, tea.Cmd) {
	if ch, ok := s.channelByName(msg.Channel); ok {
		if !ch.Members.HasInstance(msg.Instance) {
			ch.Members.Add(msg.Instance)
		}

		s.channels.Insert(ch)
	}

	var members domain.MemberList

	if ch, ok := s.channelByName(*s.active); ok {
		members = ch.Members
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))

	if *s.active == msg.Channel {
		cmds = append(cmds, msgCmd(domain.StoredEvent{
			Event: domain.ChannelModelInvited{
				Channel: msg.Channel,
				Nick:    msg.Instance.Nick(),
				By:      msg.By,
				At:      msg.At,
			},
		}))
	}

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleModelKickedEvent(msg domain.ModelKickedEvent) (ui.Model, tea.Cmd) {
	// Remove the kicked member from the channel's member list.
	if ch, ok := s.channelByName(msg.Channel); ok {
		if m, mOK := ch.Members.GetByInstance(msg.Instance); mOK {
			ch.Members.Remove(m)
		}

		s.channels.Insert(ch)
	}

	var members domain.MemberList

	if ch, ok := s.channelByName(*s.active); ok {
		members = ch.Members
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))

	if *s.active == msg.Channel {
		cmds = append(cmds, msgCmd(domain.StoredEvent{
			Event: domain.ChannelModelKicked{
				Channel: msg.Channel,
				Nick:    msg.Instance.Nick(),
				By:      msg.By,
				At:      msg.At,
			},
		}))
	}

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleMessageEvent(msg domain.MessageEvent) (ui.Model, tea.Cmd) {
	event := domain.StoredEvent{Event: msg.Event}

	if msg.Event.Channel == *s.active {
		return s, msgCmd(event)
	}

	count, _ := s.sess.UnreadCount(s.ctx, msg.Event.Channel)
	mention := s.isHighlight(msg.Event.Body)

	return s, msgCmd(components.ChannelUnreadMsg{Channel: msg.Event.Channel, Count: count, Mention: mention})
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
	*s.active = msg.Channel.Name
	s.channels.Insert(msg.Channel)

	var members domain.MemberList

	if ch, ok := s.channelByName(msg.Channel.Name); ok {
		members = ch.Members
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.SetPlaceholderMsg{}))
	cmds = append(cmds, msgCmd(components.SetChannelMsg{
		Channel: msg.Channel.Name,
		Topic:   s.activeTopic(),
		Kind:    msg.Channel.Kind,
	}))
	cmds = append(cmds, msgCmd(components.ChannelAddedMsg{Channel: msg.Channel}))
	cmds = append(cmds, msgCmd(components.ChannelActiveMsg{Channel: msg.Channel.Name}))
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))
	cmds = append(cmds, s.fetchHistoryAfter(msg.Channel.Name, s.sess.UserJoinedAt(msg.Channel.Name)))
	cmds = append(cmds, s.logAndShow(domain.ChannelSystemNotice{
		Channel: msg.Channel.Name,
		Text:    fmt.Sprintf("Opened direct message with %s", msg.Nick),
		At:      msg.At,
	}))

	return s, tea.Sequence(cmds...)
}

func (s ChatScreen) handleConfigChangedEvent(msg domain.ConfigChangedEvent) (ui.Model, tea.Cmd) {
	if *s.active == "" {
		return s, nil
	}

	return s, s.logAndShow(domain.ChannelSystemNotice{
		Channel: *s.active,
		Text:    msg.Operation,
		At:      msg.At,
	})
}

func (s ChatScreen) handleErrorEvent(msg domain.ErrorEvent) (ui.Model, tea.Cmd) {
	var cmds []tea.Cmd

	// Status-channel guard refusals are user-fixable contextual
	// errors, not failures: render them as a hint with the
	// command-tagged usage text rather than a red command-error.
	var guard domain.StatusChannelGuardError
	if errors.As(msg.Err, &guard) {
		cmds = append(cmds, s.logAndShow(domain.ChannelUsageHint{
			Channel: *s.active,
			Command: guard.Command,
			Usage:   guard.Hint,
			At:      msg.At,
		}))
	} else {
		cmds = append(cmds, s.logAndShow(domain.ChannelCommandError{
			Channel: *s.active,
			Err:     fmt.Sprintf("%s: %s", msg.Operation, msg.Err),
			At:      msg.At,
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

	return s, s.logAndShowOn(channel, domain.ChannelSystemNotice{
		Channel: channel,
		Text:    fmt.Sprintf("Model list unavailable: %s.", msg.err),
		At:      time.Now(),
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

func (s ChatScreen) channelByName(name domain.ChannelName) (domain.Channel, bool) {
	return s.channels.Get(s.channelKey(name))
}

func (s ChatScreen) channelKey(name domain.ChannelName) domain.Channel {
	return domain.Channel{
		Name: name,
		Kind: domain.InferChannelKind(name),
	}
}

func (s ChatScreen) syntheticChannel(name domain.ChannelName) domain.Channel {
	return domain.Channel{
		Name:    name,
		Kind:    domain.InferChannelKind(name),
		Members: domain.NewMemberList(),
	}
}
