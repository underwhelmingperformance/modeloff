package screens

import (
	"slices"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
)

// chatEventHandler handles domain events on behalf of ChatScreen.
// It holds a back-pointer to the screen so it can read and mutate
// its state.
type chatEventHandler struct {
	screen *ChatScreen
}

func (h *chatEventHandler) handleInitialLoad(msg domain.InitialLoadEvent) (ui.Model, tea.Cmd) {
	s := h.screen

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
	cmds = append(cmds, h.commandStateCmd())

	return s, tea.Batch(cmds...)
}

func (h *chatEventHandler) handleEventBatch(msg eventBatchMsg) (ui.Model, tea.Cmd) {
	var cmds []tea.Cmd

	for _, evt := range msg.events {
		var cmd tea.Cmd

		switch e := evt.(type) {
		case domain.InitialLoadEvent:
			_, cmd = h.handleInitialLoad(e)
		case domain.JoinEvent:
			_, cmd = h.handleJoinEvent(e)
		case domain.PartEvent:
			_, cmd = h.handlePartEvent(e)
		case domain.TopicChangeEvent:
			_, cmd = h.handleTopicChangeEvent(e)
		case domain.NickChangeEvent:
			_, cmd = h.handleNickChangeEvent(e)
		case domain.ModelInvitedEvent:
			_, cmd = h.handleModelInvitedEvent(e)
		case domain.ModelKickedEvent:
			_, cmd = h.handleModelKickedEvent(e)
		case domain.MessageEvent:
			_, cmd = h.handleMessageEvent(e)
		case domain.ModelReplyEvent:
			_, cmd = h.handleModelReplyEvent(e)
		case domain.DMOpenedEvent:
			_, cmd = h.handleDMOpenedEvent(e)
		case domain.ConfigChangedEvent:
			_, cmd = h.handleConfigChangedEvent(e)
		case domain.ErrorEvent:
			_, cmd = h.handleErrorEvent(e)
		}

		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	return h.screen, tea.Batch(cmds...)
}

func (h *chatEventHandler) handleJoinEvent(msg domain.JoinEvent) (ui.Model, tea.Cmd) {
	s := h.screen

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
	cmds = append(cmds, h.commandStateCmd())

	return s, tea.Batch(cmds...)
}

func (h *chatEventHandler) handlePartEvent(msg domain.PartEvent) (ui.Model, tea.Cmd) {
	s := h.screen

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
	cmds = append(cmds, h.commandStateCmd())

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

func (h *chatEventHandler) handleTopicChangeEvent(msg domain.TopicChangeEvent) (ui.Model, tea.Cmd) {
	s := h.screen

	if msg.Channel == s.active {
		s.topic = msg.Topic
	}

	var cmds []tea.Cmd
	cmds = append(cmds, h.commandStateCmd())

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

func (h *chatEventHandler) handleNickChangeEvent(msg domain.NickChangeEvent) (ui.Model, tea.Cmd) {
	s := h.screen

	var members []domain.Member

	if ch, err := s.sess.GetChannel(s.ctx, s.active); err == nil {
		members = s.sortedMembers(ch.Members)
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))
	cmds = append(cmds, h.commandStateCmd())

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

func (h *chatEventHandler) handleModelInvitedEvent(msg domain.ModelInvitedEvent) (ui.Model, tea.Cmd) {
	s := h.screen

	instances, _ := s.sess.ListInstances(s.ctx)
	s.instances = instances

	var members []domain.Member

	if ch, err := s.sess.GetChannel(s.ctx, s.active); err == nil {
		members = s.sortedMembers(ch.Members)
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))
	cmds = append(cmds, h.commandStateCmd())

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

func (h *chatEventHandler) handleModelKickedEvent(msg domain.ModelKickedEvent) (ui.Model, tea.Cmd) {
	s := h.screen

	instances, _ := s.sess.ListInstances(s.ctx)
	s.instances = instances

	var members []domain.Member

	if ch, err := s.sess.GetChannel(s.ctx, s.active); err == nil {
		members = s.sortedMembers(ch.Members)
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))
	cmds = append(cmds, h.commandStateCmd())

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

func (h *chatEventHandler) handleMessageEvent(msg domain.MessageEvent) (ui.Model, tea.Cmd) {
	return h.handleNewMessage(msg.Message.Channel)
}

func (h *chatEventHandler) handleModelReplyEvent(msg domain.ModelReplyEvent) (ui.Model, tea.Cmd) {
	return h.handleNewMessage(msg.Message.Channel)
}

func (h *chatEventHandler) handleNewMessage(channel domain.ChannelName) (ui.Model, tea.Cmd) {
	s := h.screen

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

func (h *chatEventHandler) handleDMOpenedEvent(msg domain.DMOpenedEvent) (ui.Model, tea.Cmd) {
	s := h.screen

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
	cmds = append(cmds, h.commandStateCmd())

	return s, tea.Batch(cmds...)
}

func (h *chatEventHandler) handleConfigChangedEvent(msg domain.ConfigChangedEvent) (ui.Model, tea.Cmd) {
	s := h.screen

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

func (h *chatEventHandler) handleErrorEvent(msg domain.ErrorEvent) (ui.Model, tea.Cmd) {
	s := h.screen

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

func (h *chatEventHandler) handleLiveModelsLoaded(msg liveModelsLoadedMsg) (ui.Model, tea.Cmd) {
	s := h.screen

	s.liveModels = msg.models

	return s, h.commandStateCmd()
}

// commandStateCmd returns a tea.Cmd that sends a CommandStateMsg with
// the current commands and completion context.
func (h *chatEventHandler) commandStateCmd() tea.Cmd {
	s := h.screen

	return msgCmd(components.CommandStateMsg{
		Commands: s.Commands(),
		Context:  h.commandContext(),
	})
}

func (h *chatEventHandler) commandContext() command.CompletionContext {
	s := h.screen

	return command.CompletionContext{
		Channels:      append([]domain.Channel(nil), s.channels...),
		Instances:     append([]domain.ModelInstance(nil), s.instances...),
		ActiveChannel: s.active,
		ActiveMembers: h.activeMembers(),
		UserNick:      s.sess.UserNick(),
		LiveModels:    append([]command.ModelOption(nil), s.liveModels...),
	}
}

func (h *chatEventHandler) activeMembers() []domain.Nick {
	s := h.screen

	for _, ch := range s.channels {
		if ch.Name != s.active {
			continue
		}

		return slices.Collect(ch.Members.Sorted())
	}

	return nil
}
