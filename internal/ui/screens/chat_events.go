// Event handlers update ChatScreen's internal state (channels, active,
// topic, etc.) synchronously, then return tea.Cmd messages to notify
// child components of the changes.
package screens

import (
	"slices"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
)

func (s *ChatScreen) handleInitialLoad(msg domain.InitialLoadEvent) (ui.Model, tea.Cmd) {
	s.channels = msg.Channels
	s.instances = msg.Instances
	s.active = msg.Active
	s.topic = msg.Topic
	s.channelCount = len(msg.Channels)

	var cmds []tea.Cmd

	cmds = append(cmds, msgCmd(components.SetChannelMsg{
		Channel: msg.Active,
		Topic:   msg.Topic,
		Lines:   components.MessagesToLines(msg.Messages),
	}))

	if s.channelCount == 0 {
		cmds = append(cmds, msgCmd(components.SetPlaceholderMsg{
			Text: welcomeText(s.sess.UserNick()),
		}))
	} else {
		cmds = append(cmds, msgCmd(components.SetPlaceholderMsg{}))
	}

	cmds = append(cmds, msgCmd(components.ChannelsUpdatedMsg{
		Channels: msg.Channels,
		Active:   msg.Active,
		Unread:   msg.Unread,
	}))
	if msg.Active != "" && msg.Topic != "" {
		for _, ch := range s.channels {
			if ch.Name == msg.Active {
				cmds = append(cmds, msgCmd(components.TopicInfo{Channel: ch}))

				break
			}
		}
	}

	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: msg.Members}))
	cmds = append(cmds, msgCmd(components.CommandStateMsg{
		Commands: s.Commands(),
	}))
	cmds = append(cmds, msgCmd(components.HighlightWordsMsg{
		Words:    s.sess.HighlightWords(),
		UserNick: s.sess.UserNick(),
	}))

	return s, tea.Batch(cmds...)
}

func (s *ChatScreen) handleSessionEvent(msg sessionEventMsg) (ui.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch evt := msg.event.(type) {
	case domain.DispatchStartedEvent:
		_, cmd = s.handleDispatchStarted(evt)
	case domain.ModelReplyEvent:
		_, cmd = s.handleModelReplyEvent(evt)
	case domain.DispatchDoneEvent:
		_, cmd = s.handleDispatchDone(evt)
	case domain.ErrorEvent:
		_, cmd = s.handleErrorEvent(evt)
	}

	return s, tea.Batch(cmd, s.listenForEvents())
}

func (s *ChatScreen) handleJoinEvent(msg domain.JoinEvent) (ui.Model, tea.Cmd) {
	s.active = msg.Channel

	channels, _ := s.sess.ListChannels(s.ctx)
	s.channels = channels
	s.channelCount = len(channels)

	messages, _ := s.sess.Messages(s.ctx, msg.Channel)

	var topic string
	var members []domain.Member

	if ch, err := s.sess.GetChannel(s.ctx, msg.Channel); err == nil {
		topic = ch.Topic
		members = s.sortedMembers(ch.Members)
	}

	s.topic = topic

	lines := components.MessagesToLines(messages)
	lines = append(lines, components.Join{JoinEvent: msg})
	unread := s.unreadCounts(s.ctx, channels)

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.SetPlaceholderMsg{}))
	cmds = append(cmds, msgCmd(components.SetChannelMsg{
		Channel: msg.Channel,
		Topic:   topic,
		Lines:   lines,
	}))
	cmds = append(cmds, msgCmd(components.ChannelsUpdatedMsg{
		Channels: channels,
		Active:   msg.Channel,
		Unread:   unread,
	}))
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))
	cmds = append(cmds, msgCmd(components.CommandStateMsg{
		Commands: s.Commands(),
	}))

	return s, tea.Batch(cmds...)
}

func (s *ChatScreen) handlePartEvent(msg domain.PartEvent) (ui.Model, tea.Cmd) {
	channels, _ := s.sess.ListChannels(s.ctx)
	s.channels = channels
	s.channelCount = len(channels)

	leavingActive := s.active == msg.Channel

	var cmds []tea.Cmd

	if leavingActive {
		if len(channels) > 0 {
			s.active = channels[0].Name
			s.topic = channels[0].Topic
		} else {
			s.active = ""
			s.topic = ""
		}

		var lines []tea.Msg

		if s.active != "" {
			messages, _ := s.sess.Messages(s.ctx, s.active)
			lines = components.MessagesToLines(messages)
		}

		cmds = append(cmds, msgCmd(components.SetChannelMsg{
			Channel: s.active,
			Topic:   s.topic,
			Lines:   lines,
		}))
	}

	unread := s.unreadCounts(s.ctx, channels)

	var members []domain.Member

	if ch, err := s.sess.GetChannel(s.ctx, s.active); err == nil {
		members = s.sortedMembers(ch.Members)
	}

	cmds = append(cmds, msgCmd(components.ChannelsUpdatedMsg{
		Channels: channels,
		Active:   s.active,
		Unread:   unread,
	}))
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))
	cmds = append(cmds, msgCmd(components.CommandStateMsg{
		Commands: s.Commands(),
	}))

	if !leavingActive && s.active == msg.Channel {
		cmds = append(cmds, msgCmd(components.Part{PartEvent: msg}))
	}

	return s, tea.Batch(cmds...)
}

func (s *ChatScreen) handleTopicChangeEvent(msg domain.TopicChangeEvent) (ui.Model, tea.Cmd) {
	if msg.Channel == s.active {
		s.topic = msg.Topic
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.CommandStateMsg{
		Commands: s.Commands(),
	}))

	if s.active == msg.Channel {
		cmds = append(cmds, msgCmd(components.TopicUpdatedMsg{
			Topic: msg.Topic,
		}))
		cmds = append(cmds, msgCmd(components.TopicChange{TopicChangeEvent: msg}))
	}

	return s, tea.Batch(cmds...)
}

func (s *ChatScreen) handleNickChangeEvent(msg domain.NickChangeEvent) (ui.Model, tea.Cmd) {
	var members []domain.Member

	if ch, err := s.sess.GetChannel(s.ctx, s.active); err == nil {
		members = s.sortedMembers(ch.Members)
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))
	cmds = append(cmds, msgCmd(components.CommandStateMsg{
		Commands: s.Commands(),
	}))
	cmds = append(cmds, msgCmd(components.HighlightWordsMsg{
		Words:    s.sess.HighlightWords(),
		UserNick: s.sess.UserNick(),
	}))

	if s.active != "" {
		cmds = append(cmds, msgCmd(components.NickChange{NickChangeEvent: msg}))
	}

	return s, tea.Batch(cmds...)
}

func (s *ChatScreen) handleModelInvitedEvent(msg domain.ModelInvitedEvent) (ui.Model, tea.Cmd) {
	instances, _ := s.sess.ListInstances(s.ctx)
	s.instances = instances

	var members []domain.Member

	if ch, err := s.sess.GetChannel(s.ctx, s.active); err == nil {
		members = s.sortedMembers(ch.Members)
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))
	cmds = append(cmds, msgCmd(components.CommandStateMsg{
		Commands: s.Commands(),
	}))

	if s.active == msg.Channel {
		cmds = append(cmds, msgCmd(components.ModelInvited{ModelInvitedEvent: msg}))
	}

	return s, tea.Batch(cmds...)
}

func (s *ChatScreen) handleModelKickedEvent(msg domain.ModelKickedEvent) (ui.Model, tea.Cmd) {
	instances, _ := s.sess.ListInstances(s.ctx)
	s.instances = instances

	var members []domain.Member

	if ch, err := s.sess.GetChannel(s.ctx, s.active); err == nil {
		members = s.sortedMembers(ch.Members)
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))
	cmds = append(cmds, msgCmd(components.CommandStateMsg{
		Commands: s.Commands(),
	}))

	if s.active == msg.Channel {
		cmds = append(cmds, msgCmd(components.ModelKicked{ModelKickedEvent: msg}))
	}

	return s, tea.Batch(cmds...)
}

func (s *ChatScreen) handleMessageEvent(msg domain.MessageEvent) (ui.Model, tea.Cmd) {
	_, cmd := s.handleNewMessage(msg.Message.Channel)

	return s, cmd
}

func (s *ChatScreen) handleModelReplyEvent(msg domain.ModelReplyEvent) (ui.Model, tea.Cmd) {
	s.replyQueue = append(s.replyQueue, msg)

	// If this is the only queued reply, deliver it immediately.
	if len(s.replyQueue) == 1 {
		return s, s.deliverNextReplyCmd()
	}

	return s, nil
}

func (s *ChatScreen) handleNewMessage(channel domain.ChannelName) (ui.Model, tea.Cmd) {
	if channel == s.active {
		messages, _ := s.sess.Messages(s.ctx, channel)
		lines := components.MessagesToLines(messages)

		return s, msgCmd(components.SetLinesMsg{Lines: lines})
	}

	channels, _ := s.sess.ListChannels(s.ctx)
	s.channels = channels
	unread := s.unreadCounts(s.ctx, channels)

	return s, msgCmd(components.ChannelsUpdatedMsg{
		Channels: channels,
		Active:   s.active,
		Unread:   unread,
	})
}

func (s *ChatScreen) handleDMOpenedEvent(msg domain.DMOpenedEvent) (ui.Model, tea.Cmd) {
	s.active = msg.Channel.Name

	channels, _ := s.sess.ListChannels(s.ctx)
	s.channels = channels
	s.channelCount = len(channels)

	messages, _ := s.sess.Messages(s.ctx, msg.Channel.Name)
	s.topic = msg.Channel.Topic

	lines := components.MessagesToLines(messages)
	lines = append(lines, components.DMOpened{Nick: msg.Nick})
	unread := s.unreadCounts(s.ctx, channels)

	var members []domain.Member

	if ch, err := s.sess.GetChannel(s.ctx, msg.Channel.Name); err == nil {
		members = s.sortedMembers(ch.Members)
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.SetPlaceholderMsg{}))
	cmds = append(cmds, msgCmd(components.SetChannelMsg{
		Channel: msg.Channel.Name,
		Topic:   msg.Channel.Topic,
		Lines:   lines,
	}))
	cmds = append(cmds, msgCmd(components.ChannelsUpdatedMsg{
		Channels: channels,
		Active:   msg.Channel.Name,
		Unread:   unread,
	}))
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))
	cmds = append(cmds, msgCmd(components.CommandStateMsg{
		Commands: s.Commands(),
	}))

	return s, tea.Batch(cmds...)
}

func (s *ChatScreen) handleConfigChangedEvent(msg domain.ConfigChangedEvent) (ui.Model, tea.Cmd) {
	if s.active == "" {
		return s, nil
	}

	return s, msgCmd(components.ConfigChanged{Operation: msg.Operation})
}

func (s *ChatScreen) handleErrorEvent(msg domain.ErrorEvent) (ui.Model, tea.Cmd) {
	var cmds []tea.Cmd

	cmds = append(cmds, msgCmd(components.BackendError{
		Operation: msg.Operation,
		Err:       msg.Err,
	}))
	cmds = append(cmds, msgCmd(components.PendingResponseMsg{Pending: false}))
	cmds = append(cmds, msgCmd(components.NickListThinkingMsg{}))

	return s, tea.Batch(cmds...)
}

func (s *ChatScreen) handleDispatchStarted(msg domain.DispatchStartedEvent) (ui.Model, tea.Cmd) {
	thinking := make(map[domain.Nick]bool, len(msg.Nicks))
	for _, nick := range msg.Nicks {
		thinking[nick] = true
	}

	return s, tea.Batch(
		msgCmd(components.PendingResponseMsg{Pending: true}),
		msgCmd(components.NickListThinkingMsg{Nicks: thinking}),
	)
}

func (s *ChatScreen) handleDispatchDone(_ domain.DispatchDoneEvent) (ui.Model, tea.Cmd) {
	if len(s.replyQueue) > 0 {
		return s, nil
	}

	return s, tea.Batch(
		msgCmd(components.NickListThinkingMsg{}),
		msgCmd(components.PendingResponseMsg{Pending: false}),
	)
}

const replyPaceInterval = 400 * time.Millisecond

func (s *ChatScreen) scheduleNextReply() tea.Cmd {
	return tea.Tick(replyPaceInterval, func(time.Time) tea.Msg {
		return deliverNextReplyMsg{}
	})
}

// deliverNextReplyCmd returns a tea.Cmd that delivers the next reply
// from the queue immediately (without pacing delay).
func (s *ChatScreen) deliverNextReplyCmd() tea.Cmd {
	return func() tea.Msg { return deliverNextReplyMsg{} }
}

func (s *ChatScreen) deliverNextReply() (ui.Model, tea.Cmd) {
	if len(s.replyQueue) == 0 {
		return s, tea.Batch(
			msgCmd(components.NickListThinkingMsg{}),
			msgCmd(components.PendingResponseMsg{Pending: false}),
		)
	}

	next := s.replyQueue[0]
	s.replyQueue = s.replyQueue[1:]

	_, cmd := s.showReply(next)

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

// showReply displays a model reply by refreshing the message list.
func (s *ChatScreen) showReply(msg domain.ModelReplyEvent) (ui.Model, tea.Cmd) {
	return s.handleNewMessage(msg.Message.Channel)
}

func (s *ChatScreen) handleLiveModelsLoaded(msg liveModelsLoadedMsg) (ui.Model, tea.Cmd) {
	s.liveModels = msg.models

	return s, msgCmd(components.CommandStateMsg{
		Commands: s.Commands(),
	})
}

func (s *ChatScreen) activeMembers() []domain.Nick {
	for _, ch := range s.channels {
		if ch.Name != s.active {
			continue
		}

		return slices.Collect(ch.Members.Sorted())
	}

	return nil
}
