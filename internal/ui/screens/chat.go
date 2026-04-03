package screens

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"

	"github.com/laney/modeloff/internal/set"
)

// chatLoadedMsg carries the initial data needed to render the chat
// screen after loading from the session.
type chatLoadedMsg struct {
	channels []domain.Channel
	active   domain.ChannelName
	title    string
	messages []domain.Message
	unread   map[domain.ChannelName]int
	members  []domain.Nick
}

// channelSwitchedMsg is sent after a channel switch completes,
// carrying the new channel's messages.
type channelSwitchedMsg struct {
	channel  domain.ChannelName
	title    string
	channels []domain.Channel
	messages []domain.Message
	unread   map[domain.ChannelName]int
	members  []domain.Nick
}

// messageSentMsg is sent after a message is saved, carrying the
// updated message list.
type messageSentMsg struct {
	channel  domain.ChannelName
	messages []domain.Message
}

// commandResultMsg carries the result of a slash command that
// modified session state.
type commandResultMsg struct {
	channels []domain.Channel
	active   domain.ChannelName
	title    string
	messages []domain.Message
	unread   map[domain.ChannelName]int
	members  []domain.Nick
	events   []components.ChatLine
}

// systemEventMsg carries typed events to display in the chat view
// without changing channel/sidebar state.
type systemEventMsg struct {
	events []components.ChatLine
}

// PokeTickMsg triggers a background poke cycle for model instances.
type PokeTickMsg struct{}

// ChatScreen is the main screen that composes Sidebar, ChatView, and
// MainLayout. It holds a reference to the session for backend
// operations. The ChatView is held as a pointer so that viewport
// and input state survive across message/channel updates.
type ChatScreen struct {
	ctx      context.Context
	sess     *session.Session
	layout   components.MainLayout
	chatView *components.ChatView
	nickList components.NickList

	active       domain.ChannelName
	title        string
	channelCount int
}

// NewChatScreen creates a chat screen backed by the given session.
// The provided context is used for all backend operations, allowing
// them to be cancelled on shutdown.
func NewChatScreen(ctx context.Context, sess *session.Session) *ChatScreen {
	sidebar := components.NewSidebar(nil, "", nil)
	chatView := components.NewChatView("", sess.UserNick(), "", nil)
	nickList := components.NewNickList(nil)
	layout := components.NewMainLayout(sidebar, chatView)
	layout.SetNickList(nickList)

	return &ChatScreen{
		ctx:      ctx,
		sess:     sess,
		layout:   layout,
		chatView: chatView,
		nickList: nickList,
	}
}

// Init implements ui.Model.
func (s *ChatScreen) Init() tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		channels, err := s.sess.ListChannels(ctx)
		if err != nil {
			channels = nil
		}

		active, err := s.sess.LastChannel(ctx)
		if err != nil {
			active = ""
		}

		var messages []domain.Message
		var title string
		var members []domain.Nick

		if active != "" {
			messages, _ = s.sess.Messages(ctx, active)

			if ch, err := s.sess.GetChannel(ctx, active); err == nil {
				title = ch.Title
				members = sortedMembers(ch.Members)
			}
		}

		return chatLoadedMsg{
			channels: channels,
			active:   active,
			title:    title,
			messages: messages,
			unread:   s.unreadCounts(ctx, channels),
			members:  members,
		}
	}
}

// Update implements ui.Model.
func (s *ChatScreen) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case chatLoadedMsg:
		return s.handleLoaded(msg)

	case channelSwitchedMsg:
		return s.handleChannelSwitched(msg)

	case messageSentMsg:
		return s.handleMessageSent(msg)

	case commandResultMsg:
		return s.handleCommandResult(msg)

	case systemEventMsg:
		return s.handleSystemEvent(msg)

	case PokeTickMsg:
		s.forwardToLayout(components.PendingResponseMsg{Pending: true})

		return s, s.handlePoke()

	case components.ChannelSelectedMsg:
		return s, s.switchChannel(msg.Channel)

	case components.MessageSubmitMsg:
		if s.active == "" {
			return s, func() tea.Msg {
				return systemEventMsg{events: []components.ChatLine{components.NoChannel{}}}
			}
		}

		s.forwardToLayout(components.PendingResponseMsg{Pending: true})

		return s, s.sendMessage(msg.Text)

	case components.CommandSubmitMsg:
		return s, s.handleCommand(msg)

	case tea.KeyMsg:
		if msg.String() == "ctrl+n" {
			s.forwardToLayout(components.NickListToggleMsg{})
			return s, nil
		}
	}

	updated, cmd := s.layout.Update(msg)
	s.layout = updated.(components.MainLayout)

	return s, cmd
}

func (s *ChatScreen) handleLoaded(msg chatLoadedMsg) (ui.Model, tea.Cmd) {
	s.active = msg.active
	s.title = msg.title
	s.channelCount = len(msg.channels)

	s.updateSidebar(msg.channels, msg.active, msg.unread)
	s.chatView.SetChannel(msg.active, msg.title, components.MessagesToLines(msg.messages))
	s.updateNickList(msg.members)

	if s.channelCount == 0 {
		s.chatView.SetPlaceholder(welcomeText(s.sess.UserNick()))
	} else {
		s.chatView.SetPlaceholder("")
	}

	return s, nil
}

func (s *ChatScreen) handleChannelSwitched(msg channelSwitchedMsg) (ui.Model, tea.Cmd) {
	s.active = msg.channel
	s.title = msg.title
	s.channelCount = len(msg.channels)

	s.updateSidebar(msg.channels, msg.channel, msg.unread)
	s.chatView.SetPlaceholder("")
	s.chatView.SetChannel(msg.channel, msg.title, components.MessagesToLines(msg.messages))
	s.updateNickList(msg.members)

	return s, nil
}

func (s *ChatScreen) handleMessageSent(msg messageSentMsg) (ui.Model, tea.Cmd) {
	s.chatView.SetLines(components.MessagesToLines(msg.messages))
	s.forwardToLayout(components.PendingResponseMsg{Pending: false})

	return s, nil
}

func (s *ChatScreen) handleCommandResult(msg commandResultMsg) (ui.Model, tea.Cmd) {
	s.active = msg.active
	s.title = msg.title
	s.channelCount = len(msg.channels)

	lines := components.MessagesToLines(msg.messages)
	lines = append(lines, msg.events...)

	s.updateSidebar(msg.channels, msg.active, msg.unread)
	s.chatView.SetPlaceholder("")
	s.chatView.SetChannel(msg.active, msg.title, lines)
	s.updateNickList(msg.members)
	s.forwardToLayout(components.PendingResponseMsg{Pending: false})

	return s, nil
}

func (s *ChatScreen) handleSystemEvent(msg systemEventMsg) (ui.Model, tea.Cmd) {
	var lines []components.ChatLine

	if s.active != "" {
		messages, _ := s.sess.Messages(s.ctx, s.active)
		lines = components.MessagesToLines(messages)
	}

	lines = append(lines, msg.events...)
	s.chatView.SetLines(lines)

	return s, nil
}

func (s *ChatScreen) updateSidebar(channels []domain.Channel, active domain.ChannelName, unread map[domain.ChannelName]int) {
	s.forwardToLayout(components.ChannelsUpdatedMsg{
		Channels: channels,
		Active:   active,
		Unread:   unread,
	})
}

func (s *ChatScreen) updateNickList(members []domain.Nick) {
	s.nickList = components.NewNickList(members)
	s.layout.SetNickList(s.nickList)
}

func (s *ChatScreen) forwardToLayout(msg tea.Msg) {
	updated, _ := s.layout.Update(msg)
	s.layout = updated.(components.MainLayout)
}

func (s *ChatScreen) unreadCounts(ctx context.Context, channels []domain.Channel) map[domain.ChannelName]int {
	counts := make(map[domain.ChannelName]int, len(channels))

	for _, ch := range channels {
		n, err := s.sess.UnreadCount(ctx, ch.Name)
		if err != nil {
			continue
		}

		if n > 0 {
			counts[ch.Name] = n
		}
	}

	return counts
}

func sortedMembers(members set.Ordered[domain.Nick]) []domain.Nick {
	if members == nil {
		return nil
	}

	var nicks []domain.Nick
	for nick := range members.Sorted() {
		nicks = append(nicks, nick)
	}

	return nicks
}

func (s *ChatScreen) switchChannel(ch domain.ChannelName) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		_, _ = s.sess.Join(ctx, string(ch))

		channels, _ := s.sess.ListChannels(ctx)
		messages, _ := s.sess.Messages(ctx, ch)

		var title string
		var members []domain.Nick

		if channel, err := s.sess.GetChannel(ctx, ch); err == nil {
			title = channel.Title
			members = sortedMembers(channel.Members)
		}

		return channelSwitchedMsg{
			channel:  ch,
			title:    title,
			channels: channels,
			messages: messages,
			unread:   s.unreadCounts(ctx, channels),
			members:  members,
		}
	}
}

func (s *ChatScreen) sendMessage(text string) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		_, _ = s.sess.SendMessage(ctx, s.active, text)

		messages, _ := s.sess.Messages(ctx, s.active)

		return messageSentMsg{
			channel:  s.active,
			messages: messages,
		}
	}
}

// View implements ui.Model.
func (s *ChatScreen) View(width, height int) string {
	return s.layout.View(width, height)
}
