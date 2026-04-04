package screens

import (
	"slices"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
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
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: msg.Members}))
	cmds = append(cmds, msgCmd(components.CommandStateMsg{
		Commands: s.Commands(),
		Context:  s.commandContext(),
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
		Context:  s.commandContext(),
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

		var lines []components.ChatLine

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
		Context:  s.commandContext(),
	}))

	if !leavingActive && s.active == msg.Channel {
		cmds = append(cmds, msgCmd(components.AppendLinesMsg{
			Channel: msg.Channel,
			Lines: []components.ChatLine{
				components.Part{PartEvent: msg},
			},
		}))
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
		Context:  s.commandContext(),
	}))

	if s.active == msg.Channel {
		cmds = append(cmds, msgCmd(components.AppendLinesMsg{
			Channel: msg.Channel,
			Lines: []components.ChatLine{
				components.TopicChange{TopicChangeEvent: msg},
			},
		}))
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
		Context:  s.commandContext(),
	}))

	if s.active != "" {
		cmds = append(cmds, msgCmd(components.AppendLinesMsg{
			Channel: s.active,
			Lines: []components.ChatLine{
				components.NickChange{NickChangeEvent: msg},
			},
		}))
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
		Context:  s.commandContext(),
	}))

	if s.active == msg.Channel {
		cmds = append(cmds, msgCmd(components.AppendLinesMsg{
			Channel: msg.Channel,
			Lines: []components.ChatLine{
				components.ModelInvited{ModelInvitedEvent: msg},
			},
		}))
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
		Context:  s.commandContext(),
	}))

	if s.active == msg.Channel {
		cmds = append(cmds, msgCmd(components.AppendLinesMsg{
			Channel: msg.Channel,
			Lines: []components.ChatLine{
				components.ModelKicked{ModelKickedEvent: msg},
			},
		}))
	}

	return s, tea.Batch(cmds...)
}

func (s *ChatScreen) handleMessageEvent(msg domain.MessageEvent) (ui.Model, tea.Cmd) {
	return s.handleNewMessage(msg.Message.Channel)
}

func (s *ChatScreen) handleModelReplyEvent(msg domain.ModelReplyEvent) (ui.Model, tea.Cmd) {
	return s.handleNewMessage(msg.Message.Channel)
}

func (s *ChatScreen) handleNewMessage(channel domain.ChannelName) (ui.Model, tea.Cmd) {
	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.PendingResponseMsg{Pending: false}))

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
		Context:  s.commandContext(),
	}))

	return s, tea.Batch(cmds...)
}

func (s *ChatScreen) handleConfigChangedEvent(msg domain.ConfigChangedEvent) (ui.Model, tea.Cmd) {
	if s.active == "" {
		return s, nil
	}

	return s, msgCmd(components.AppendLinesMsg{
		Channel: s.active,
		Lines: []components.ChatLine{
			components.ConfigChanged{Operation: msg.Operation},
		},
	})
}

func (s *ChatScreen) handleErrorEvent(msg domain.ErrorEvent) (ui.Model, tea.Cmd) {
	var cmds []tea.Cmd

	cmds = append(cmds, msgCmd(components.AppendLinesMsg{
		Channel: s.active,
		Lines: []components.ChatLine{
			components.BackendError{
				Operation: msg.Operation,
				Err:       msg.Err,
			},
		},
	}))
	cmds = append(cmds, msgCmd(components.PendingResponseMsg{Pending: false}))

	return s, tea.Batch(cmds...)
}

func (s *ChatScreen) handleLiveModelsLoaded(msg liveModelsLoadedMsg) (ui.Model, tea.Cmd) {
	s.liveModels = msg.models

	return s, msgCmd(components.CommandStateMsg{
		Commands: s.Commands(),
		Context:  s.commandContext(),
	})
}

func (s *ChatScreen) commandContext() command.CompletionContext {
	return command.CompletionContext{
		Channels:      append([]domain.Channel(nil), s.channels...),
		Instances:     append([]domain.ModelInstance(nil), s.instances...),
		ActiveChannel: s.active,
		ActiveMembers: s.activeMembers(),
		UserNick:      s.sess.UserNick(),
		LiveModels:    append([]command.ModelOption(nil), s.liveModels...),
	}
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
