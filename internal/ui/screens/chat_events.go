package screens

import (
	"errors"
	"fmt"
	"iter"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/domain"
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
	ch, exists := s.channels.Get(domain.Channel{Name: msg.Channel})
	if !exists {
		ch = domain.Channel{
			Name:    msg.Channel,
			Kind:    domain.KindChannel,
			Members: domain.NewMemberList(),
		}
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
	isUser := msg.Nick == s.sess.UserNick()
	_, channelKnown := s.channels.Get(domain.Channel{Name: msg.Channel})

	if !isUser && !channelKnown {
		return s, nil
	}

	ch, exists := s.channels.Get(domain.Channel{Name: msg.Channel})
	if !exists {
		ch = domain.Channel{
			Name:    msg.Channel,
			Kind:    domain.KindChannel,
			Members: domain.NewMemberList(),
			Created: msg.At,
		}
	}

	if !ch.Members.Has(msg.Nick) {
		ch.Members.Add(msg.Nick)
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
	ch, ok := s.channels.Get(domain.Channel{Name: msg.Channel})
	if !ok {
		return s, nil
	}

	ch.Members.SetMode(msg.Nick, msg.Mode)
	s.channels.Insert(ch)

	if msg.Channel != *s.active {
		return s, nil
	}

	return s, msgCmd(components.NickListUpdatedMsg{Members: ch.Members})
}

func (s ChatScreen) handlePartEvent(msg domain.PartEvent) (ui.Model, tea.Cmd) {
	leavingActive := *s.active == msg.Channel

	// Remove the nick from the channel's member list.
	if ch, ok := s.channels.Get(domain.Channel{Name: msg.Channel}); ok {
		if m, mOK := ch.Members.Get(msg.Nick); mOK {
			ch.Members.Remove(m)
		}

		s.channels.Insert(ch)
	}

	// If the user is leaving, remove the channel.
	if msg.Nick == s.sess.UserNick() {
		s.channels.Remove(domain.Channel{Name: msg.Channel})
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
		if ch, ok := s.channels.Get(domain.Channel{Name: *s.active}); ok {
			members = ch.Members
		}
	}

	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))
	if leavingActive {
		cmds = append(cmds, s.fetchHistoryAfter(*s.active, s.sess.UserJoinedAt(*s.active)))
	}

	if !leavingActive && *s.active == msg.Channel {
		cmds = append(cmds, msgCmd(domain.StoredEvent{
			Event: domain.ChannelPart(msg),
		}))
	}

	return s, tea.Sequence(cmds...)
}

func (s ChatScreen) handleQuitEvent(msg domain.QuitEvent) (ui.Model, tea.Cmd) {
	// Remove the nick from all channels' member lists.
	for ch := range s.channels.All() {
		if m, ok := ch.Members.Get(msg.Nick); ok {
			ch.Members.Remove(m)
			s.channels.Insert(ch)
		}
	}

	// Remove the instance.
	if inst, ok := s.instances.Get(domain.Instance{Nick: msg.Nick}); ok {
		s.instances.Remove(inst)
	}

	var cmds []tea.Cmd

	// Update nick list for the active channel.
	var members domain.MemberList

	if *s.active != "" {
		if ch, ok := s.channels.Get(domain.Channel{Name: *s.active}); ok {
			members = ch.Members
		}
	}

	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))

	// Show the quit event in the active channel.
	if *s.active != "" {
		cmds = append(cmds, s.logAndShow(domain.ChannelQuit{
			Channel: *s.active,
			Nick:    msg.Nick,
			Message: msg.Message,
			At:      msg.At,
		}))
	}

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleTopicChangeEvent(msg domain.TopicChangeEvent) (ui.Model, tea.Cmd) {
	if ch, ok := s.channels.Get(domain.Channel{Name: msg.Channel}); ok {
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
	if ch, ok := s.channels.Get(domain.Channel{Name: msg.Channel}); ok {
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
	// Update the nick in this channel's local member list.
	if ch, ok := s.channels.Get(domain.Channel{Name: msg.Channel}); ok {
		if old, mOK := ch.Members.Get(msg.OldNick); mOK {
			ch.Members.Remove(old)
			ch.Members.Add(msg.NewNick)
			ch.Members.SetMode(msg.NewNick, old.Mode)
			s.channels.Insert(ch)
		}
	}

	if msg.Channel != *s.active {
		return s, nil
	}

	var cmds []tea.Cmd

	if ch, ok := s.channels.Get(domain.Channel{Name: *s.active}); ok {
		cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: ch.Members}))
	}

	cmds = append(cmds, msgCmd(domain.StoredEvent{
		Event: domain.ChannelNickChange(msg),
	}))

	if msg.NewNick == s.sess.UserNick() {
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
	s.instances.Insert(msg.Instance)

	if ch, ok := s.channels.Get(domain.Channel{Name: msg.Channel}); ok {
		if !ch.Members.Has(msg.Instance.Nick) {
			ch.Members.Add(msg.Instance.Nick)
		}

		s.channels.Insert(ch)
	}

	var members domain.MemberList

	if ch, ok := s.channels.Get(domain.Channel{Name: *s.active}); ok {
		members = ch.Members
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))

	if *s.active == msg.Channel {
		cmds = append(cmds, msgCmd(domain.StoredEvent{
			Event: domain.ChannelModelInvited{
				Channel: msg.Channel,
				Nick:    msg.Instance.Nick,
				By:      msg.By,
				At:      msg.At,
			},
		}))
	}

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleModelKickedEvent(msg domain.ModelKickedEvent) (ui.Model, tea.Cmd) {
	// Remove the nick from the channel's member list.
	if ch, ok := s.channels.Get(domain.Channel{Name: msg.Channel}); ok {
		if m, mOK := ch.Members.Get(msg.Nick); mOK {
			ch.Members.Remove(m)
		}

		s.channels.Insert(ch)
	}

	// Update the instance's channel list.
	if inst, ok := s.instances.Get(domain.Instance{Nick: msg.Nick}); ok {
		inst.Channels.Delete(msg.Channel)
		s.instances.Insert(inst)
	}

	var members domain.MemberList

	if ch, ok := s.channels.Get(domain.Channel{Name: *s.active}); ok {
		members = ch.Members
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))

	if *s.active == msg.Channel {
		cmds = append(cmds, msgCmd(domain.StoredEvent{
			Event: domain.ChannelModelKicked(msg),
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
	s.replyQueue = append(s.replyQueue, msg)

	// If this is the only queued reply, deliver it immediately.
	if len(s.replyQueue) == 1 {
		return s, s.deliverNextReplyCmd()
	}

	return s, nil
}

func (s ChatScreen) handleDMOpenedEvent(msg domain.DMOpenedEvent) (ui.Model, tea.Cmd) {
	*s.active = msg.Channel.Name
	s.channels.Insert(msg.Channel)

	var members domain.MemberList

	if ch, ok := s.channels.Get(domain.Channel{Name: msg.Channel.Name}); ok {
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
	if len(s.replyQueue) > 0 {
		return s, nil
	}

	return s, tea.Batch(
		msgCmd(components.NickListThinkingMsg{}),
		msgCmd(components.PendingResponseMsg{Pending: false}),
	)
}

const replyPaceInterval = 400 * time.Millisecond

func (s ChatScreen) scheduleNextReply() tea.Cmd {
	return tea.Tick(replyPaceInterval, func(time.Time) tea.Msg {
		return deliverNextReplyMsg{}
	})
}

// deliverNextReplyCmd returns a tea.Cmd that delivers the next reply
// from the queue immediately (without pacing delay).
func (s ChatScreen) deliverNextReplyCmd() tea.Cmd {
	return func() tea.Msg { return deliverNextReplyMsg{} }
}

func (s ChatScreen) deliverNextReply() (ui.Model, tea.Cmd) {
	if len(s.replyQueue) == 0 {
		return s, tea.Batch(
			msgCmd(components.NickListThinkingMsg{}),
			msgCmd(components.PendingResponseMsg{Pending: false}),
		)
	}

	next := s.replyQueue[0]
	s.replyQueue = s.replyQueue[1:]

	updated, cmd := s.showReply(next)
	s = updated.(ChatScreen)

	if len(s.replyQueue) > 0 {
		cmd = tea.Batch(cmd, s.scheduleNextReply())
	} else {
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

	return s, nil
}

func (s ChatScreen) activeTopic() string {
	if *s.active == "" {
		return ""
	}

	ch, ok := s.channels.Get(domain.Channel{Name: *s.active})
	if !ok {
		return ""
	}

	return ch.Topic
}

func (s ChatScreen) activeKind() domain.ChannelKind {
	if *s.active == "" {
		return domain.KindChannel
	}

	ch, ok := s.channels.Get(domain.Channel{Name: *s.active})
	if !ok {
		return domain.KindChannel
	}

	return ch.Kind
}

func (s ChatScreen) activeMemberNicks() iter.Seq[domain.Nick] {
	ch, ok := s.channels.Get(domain.Channel{Name: *s.active})
	if !ok {
		return func(func(domain.Nick) bool) {}
	}

	return ch.Members.Nicks()
}
