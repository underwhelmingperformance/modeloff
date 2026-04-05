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

func (s *ChatScreen) handleEventBatch(msg eventBatchMsg) (ui.Model, tea.Cmd) {
	var cmds []tea.Cmd

	for _, evt := range msg.events {
		var cmd tea.Cmd

		switch e := evt.(type) {
		case domain.InitialLoadEvent:
			_, cmd = s.handleInitialLoad(e)
		case domain.JoinEvent:
			_, cmd = s.handleJoinEvent(e)
		case domain.PartEvent:
			_, cmd = s.handlePartEvent(e)
		case domain.TopicChangeEvent:
			_, cmd = s.handleTopicChangeEvent(e)
		case domain.NickChangeEvent:
			_, cmd = s.handleNickChangeEvent(e)
		case domain.ModelInvitedEvent:
			_, cmd = s.handleModelInvitedEvent(e)
		case domain.ModelKickedEvent:
			_, cmd = s.handleModelKickedEvent(e)
		case domain.MessageEvent:
			_, cmd = s.handleMessageEvent(e)
		case domain.ModelReplyEvent:
			_, cmd = s.handleModelReplyEvent(e)
		case domain.DMOpenedEvent:
			_, cmd = s.handleDMOpenedEvent(e)
		case domain.ConfigChangedEvent:
			_, cmd = s.handleConfigChangedEvent(e)
		case domain.ErrorEvent:
			_, cmd = s.handleErrorEvent(e)
		}

		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	cmds = append(cmds, msgCmd(components.NickListThinkingMsg{}))

	return s, tea.Batch(cmds...)
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
	isLocal := msg.Message.From == s.sess.UserNick()

	_, cmd := s.handleNewMessage(msg.Message.Channel, isLocal)

	// If the message is from the local user, dispatch to model
	// instances asynchronously so the message appears immediately.
	// The pending indicator stays active until dispatch completes.
	if isLocal {
		cmd = tea.Batch(cmd, msgCmd(dispatchMsg{
			channel: msg.Message.Channel,
			message: msg.Message,
		}))
	}

	return s, cmd
}

func (s *ChatScreen) handleModelReplyEvent(msg domain.ModelReplyEvent) (ui.Model, tea.Cmd) {
	return s.handleNewMessage(msg.Message.Channel, false)
}

func (s *ChatScreen) handleNewMessage(channel domain.ChannelName, keepPending bool) (ui.Model, tea.Cmd) {
	var cmds []tea.Cmd

	if !keepPending {
		cmds = append(cmds, msgCmd(components.PendingResponseMsg{Pending: false}))
	}

	if channel == s.active {
		messages, _ := s.sess.Messages(s.ctx, channel)
		lines := components.MessagesToLines(messages)

		cmds = append(cmds, msgCmd(components.SetLinesMsg{Lines: lines}))
	} else {
		channels, _ := s.sess.ListChannels(s.ctx)
		s.channels = channels
		unread := s.unreadCounts(s.ctx, channels)

		cmds = append(cmds, msgCmd(components.ChannelsUpdatedMsg{
			Channels: channels,
			Active:   s.active,
			Unread:   unread,
		}))
	}

	return s, tea.Batch(cmds...)
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

func (s *ChatScreen) handleDispatchDone(msg dispatchDoneMsg) (ui.Model, tea.Cmd) {
	if len(msg.replies) == 0 {
		return s, tea.Batch(
			msgCmd(components.NickListThinkingMsg{}),
			msgCmd(components.PendingResponseMsg{Pending: false}),
		)
	}

	// Show the first reply immediately, queue the rest for paced delivery.
	first := msg.replies[0]
	s.replyQueue = append(s.replyQueue[:0], msg.replies[1:]...)

	_, cmd := s.handleModelReplyEvent(first)

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

const replyPaceInterval = 400 * time.Millisecond

func (s *ChatScreen) scheduleNextReply() tea.Cmd {
	return tea.Tick(replyPaceInterval, func(time.Time) tea.Msg {
		return deliverNextReplyMsg{}
	})
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

	_, cmd := s.handleModelReplyEvent(next)

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

func (s *ChatScreen) handleLiveModelsLoaded(msg liveModelsLoadedMsg) (ui.Model, tea.Cmd) {
	s.liveModels = msg.models

	return s, msgCmd(components.CommandStateMsg{
		Commands: s.Commands(),
	})
}

func (s *ChatScreen) modelNicksInChannel(ch domain.ChannelName) map[domain.Nick]bool {
	userNick := s.sess.UserNick()
	nicks := make(map[domain.Nick]bool)

	for _, channel := range s.channels {
		if channel.Name != ch {
			continue
		}

		for nick := range channel.Members.Sorted() {
			if nick != userNick {
				nicks[nick] = true
			}
		}

		break
	}

	return nicks
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
