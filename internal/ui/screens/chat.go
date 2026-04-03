package screens

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
)

// chatLoadedMsg carries the initial data needed to render the chat
// screen after loading from the session.
type chatLoadedMsg struct {
	rooms    []domain.Room
	active   domain.RoomName
	messages []domain.Message
}

// roomSwitchedMsg is sent after a room switch completes, carrying the
// new room's messages.
type roomSwitchedMsg struct {
	room     domain.RoomName
	rooms    []domain.Room
	messages []domain.Message
}

// messageSentMsg is sent after a message is saved, carrying the
// updated message list.
type messageSentMsg struct {
	room     domain.RoomName
	messages []domain.Message
}

// commandResultMsg carries the result of a slash command that
// modified session state.
type commandResultMsg struct {
	rooms    []domain.Room
	active   domain.RoomName
	messages []domain.Message
}

// ChatScreen is the main screen that composes Sidebar, ChatView, and
// MainLayout. It holds a reference to the session for backend
// operations.
type ChatScreen struct {
	sess   *session.Session
	layout components.MainLayout

	active domain.RoomName
}

// NewChatScreen creates a chat screen backed by the given session.
func NewChatScreen(sess *session.Session) ChatScreen {
	sidebar := components.NewSidebar(nil, "")
	chatView := components.NewChatView("", nil)
	layout := components.NewMainLayout(sidebar, chatView)

	return ChatScreen{
		sess:   sess,
		layout: layout,
	}
}

// Init implements ui.Model.
func (s ChatScreen) Init() tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		rooms, err := s.sess.ListRooms(ctx)
		if err != nil {
			rooms = nil
		}

		active, err := s.sess.LastRoom(ctx)
		if err != nil {
			active = ""
		}

		var messages []domain.Message
		if active != "" {
			messages, _ = s.sess.Messages(ctx, active)
		}

		return chatLoadedMsg{
			rooms:    rooms,
			active:   active,
			messages: messages,
		}
	}
}

// Update implements ui.Model.
func (s ChatScreen) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case chatLoadedMsg:
		return s.handleLoaded(msg)

	case roomSwitchedMsg:
		return s.handleRoomSwitched(msg)

	case messageSentMsg:
		return s.handleMessageSent(msg)

	case commandResultMsg:
		return s.handleCommandResult(msg)

	case components.RoomSelectedMsg:
		return s, s.switchRoom(msg.Room)

	case components.MessageSubmitMsg:
		return s, s.sendMessage(msg.Text)

	case components.CommandSubmitMsg:
		return s, s.handleCommand(msg)
	}

	updated, cmd := s.layout.Update(msg)
	s.layout = updated.(components.MainLayout)

	return s, cmd
}

func (s ChatScreen) handleLoaded(msg chatLoadedMsg) (ui.Model, tea.Cmd) {
	s.active = msg.active

	sidebar := components.NewSidebar(msg.rooms, msg.active)
	chatView := components.NewChatView(msg.active, msg.messages)
	s.layout = components.NewMainLayout(sidebar, chatView)

	return s, nil
}

func (s ChatScreen) handleRoomSwitched(msg roomSwitchedMsg) (ui.Model, tea.Cmd) {
	s.active = msg.room

	sidebar := components.NewSidebar(msg.rooms, msg.room)
	chatView := components.NewChatView(msg.room, msg.messages)
	s.layout = components.NewMainLayout(sidebar, chatView)

	return s, nil
}

func (s ChatScreen) handleMessageSent(msg messageSentMsg) (ui.Model, tea.Cmd) {
	chatView := components.NewChatView(msg.room, msg.messages)
	s.layout = components.NewMainLayout(s.layout.Sidebar, chatView)

	return s, nil
}

func (s ChatScreen) handleCommandResult(msg commandResultMsg) (ui.Model, tea.Cmd) {
	s.active = msg.active

	sidebar := components.NewSidebar(msg.rooms, msg.active)
	chatView := components.NewChatView(msg.active, msg.messages)
	s.layout = components.NewMainLayout(sidebar, chatView)

	return s, nil
}

func (s ChatScreen) switchRoom(room domain.RoomName) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		// Join is idempotent and persists the last active room.
		_, _ = s.sess.Join(ctx, string(room))

		rooms, _ := s.sess.ListRooms(ctx)
		messages, _ := s.sess.Messages(ctx, room)

		return roomSwitchedMsg{
			room:     room,
			rooms:    rooms,
			messages: messages,
		}
	}
}

func (s ChatScreen) sendMessage(text string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		_, _ = s.sess.SendMessage(ctx, s.active, text)

		messages, _ := s.sess.Messages(ctx, s.active)

		return messageSentMsg{
			room:     s.active,
			messages: messages,
		}
	}
}

func (s ChatScreen) handleCommand(cmd components.CommandSubmitMsg) tea.Cmd {
	switch cmd.Name {
	case "join":
		return s.joinRoom(cmd.Args)

	case "leave":
		return s.leaveRoom()

	default:
		return nil
	}
}

func (s ChatScreen) joinRoom(name string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		_, _ = s.sess.Join(ctx, name)

		rooms, _ := s.sess.ListRooms(ctx)
		active := domain.RoomName(name)
		messages, _ := s.sess.Messages(ctx, active)

		return commandResultMsg{
			rooms:    rooms,
			active:   active,
			messages: messages,
		}
	}
}

func (s ChatScreen) leaveRoom() tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		_, _ = s.sess.Leave(ctx, s.active)

		rooms, _ := s.sess.ListRooms(ctx)

		var active domain.RoomName
		var messages []domain.Message

		if len(rooms) > 0 {
			active = rooms[0].Name
			messages, _ = s.sess.Messages(ctx, active)
		}

		return commandResultMsg{
			rooms:    rooms,
			active:   active,
			messages: messages,
		}
	}
}

// View implements ui.Model.
func (s ChatScreen) View(width, height int) string {
	return s.layout.View(width, height)
}
