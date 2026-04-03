package screens

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
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
	rooms        []domain.Room
	active       domain.RoomName
	messages     []domain.Message
	systemEvents []string
}

// systemEventMsg carries system event text to display in the chat
// view without changing room/sidebar state.
type systemEventMsg struct {
	lines []string
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
	chatView := components.NewChatView("", sess.UserNick(), nil)
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

	case systemEventMsg:
		return s.handleSystemEvent(msg)

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
	chatView := components.NewChatView(msg.active, s.sess.UserNick(), msg.messages)
	s.layout = components.NewMainLayout(sidebar, chatView)

	return s, nil
}

func (s ChatScreen) handleRoomSwitched(msg roomSwitchedMsg) (ui.Model, tea.Cmd) {
	s.active = msg.room

	sidebar := components.NewSidebar(msg.rooms, msg.room)
	chatView := components.NewChatView(msg.room, s.sess.UserNick(), msg.messages)
	s.layout = components.NewMainLayout(sidebar, chatView)

	return s, nil
}

func (s ChatScreen) handleMessageSent(msg messageSentMsg) (ui.Model, tea.Cmd) {
	chatView := components.NewChatView(msg.room, s.sess.UserNick(), msg.messages)
	s.layout = components.NewMainLayout(s.layout.Sidebar, chatView)

	return s, nil
}

func (s ChatScreen) handleCommandResult(msg commandResultMsg) (ui.Model, tea.Cmd) {
	s.active = msg.active

	messages := msg.messages
	messages = appendSystemEvents(messages, s.active, msg.systemEvents)

	sidebar := components.NewSidebar(msg.rooms, msg.active)
	chatView := components.NewChatView(msg.active, s.sess.UserNick(), messages)
	s.layout = components.NewMainLayout(sidebar, chatView)

	return s, nil
}

func (s ChatScreen) handleSystemEvent(msg systemEventMsg) (ui.Model, tea.Cmd) {
	ctx := context.Background()

	messages, _ := s.sess.Messages(ctx, s.active)
	messages = appendSystemEvents(messages, s.active, msg.lines)

	chatView := components.NewChatView(s.active, s.sess.UserNick(), messages)
	s.layout = components.NewMainLayout(s.layout.Sidebar, chatView)

	return s, nil
}

func appendSystemEvents(messages []domain.Message, room domain.RoomName, events []string) []domain.Message {
	for _, line := range events {
		messages = append(messages, domain.Message{
			Room: room,
			From: "***",
			Body: line,
		})
	}

	return messages
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

func (s ChatScreen) handleCommand(msg components.CommandSubmitMsg) tea.Cmd {
	raw := "/" + msg.Name
	if msg.Args != "" {
		raw += " " + msg.Args
	}

	parsed, err := command.Parse(raw)
	if err != nil {
		return func() tea.Msg {
			return systemEventMsg{lines: []string{err.Error()}}
		}
	}

	switch cmd := parsed.(type) {
	case command.JoinCommand:
		return s.joinRoom(cmd.Room)

	case command.LeaveCommand:
		return s.leaveRoom()

	case command.NickCommand:
		return s.changeNick(domain.Nick(cmd.Nick))

	case command.TitleCommand:
		return s.setTitle(cmd.Title)

	case command.WhoisCommand:
		return s.whois(domain.Nick(cmd.Nick))

	case command.ListCommand:
		return s.listRooms()

	case command.InviteCommand:
		if cmd.Model == "" {
			return func() tea.Msg {
				return systemEventMsg{lines: []string{"usage: /invite <model-id>"}}
			}
		}

		return s.inviteModel(domain.ModelID(cmd.Model))

	case command.KickCommand:
		return s.kickModel(domain.Nick(cmd.Nick))

	case command.MsgCommand, command.ConfigCommand:
		return func() tea.Msg {
			return systemEventMsg{lines: []string{
				fmt.Sprintf("/%s is not yet implemented", msg.Name),
			}}
		}

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

func (s ChatScreen) changeNick(nick domain.Nick) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		evt := s.sess.ChangeNick(nick)

		rooms, _ := s.sess.ListRooms(ctx)
		messages, _ := s.sess.Messages(ctx, s.active)

		return commandResultMsg{
			rooms:    rooms,
			active:   s.active,
			messages: messages,
			systemEvents: []string{
				fmt.Sprintf("%s is now known as %s", evt.OldNick, evt.NewNick),
			},
		}
	}
}

func (s ChatScreen) setTitle(title string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		_, err := s.sess.SetTitle(ctx, s.active, title)
		if err != nil {
			return systemEventMsg{lines: []string{err.Error()}}
		}

		rooms, _ := s.sess.ListRooms(ctx)
		messages, _ := s.sess.Messages(ctx, s.active)

		event := fmt.Sprintf("topic for %s set to: %s", s.active, title)
		if title == "" {
			event = fmt.Sprintf("topic for %s cleared", s.active)
		}

		return commandResultMsg{
			rooms:        rooms,
			active:       s.active,
			messages:     messages,
			systemEvents: []string{event},
		}
	}
}

func (s ChatScreen) whois(nick domain.Nick) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		inst, err := s.sess.Whois(ctx, nick)
		if err != nil {
			return systemEventMsg{lines: []string{
				fmt.Sprintf("no such nick: %s", nick),
			}}
		}

		lines := []string{
			fmt.Sprintf("%s is %s", inst.Nick, inst.ModelID),
		}

		if inst.Persona != "" {
			lines = append(lines, fmt.Sprintf("  persona: %s", inst.Persona))
		}

		if len(inst.Rooms) > 0 {
			roomStrs := make([]string, len(inst.Rooms))
			for i, r := range inst.Rooms {
				roomStrs[i] = string(r)
			}

			lines = append(lines, fmt.Sprintf("  rooms: %s", strings.Join(roomStrs, ", ")))
		}

		return systemEventMsg{lines: lines}
	}
}

func (s ChatScreen) inviteModel(modelID domain.ModelID) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		evt, err := s.sess.Invite(ctx, s.active, modelID)
		if err != nil {
			return systemEventMsg{lines: []string{err.Error()}}
		}

		rooms, _ := s.sess.ListRooms(ctx)
		messages, _ := s.sess.Messages(ctx, s.active)

		return commandResultMsg{
			rooms:    rooms,
			active:   s.active,
			messages: messages,
			systemEvents: []string{
				fmt.Sprintf("%s (%s) has joined %s", evt.Instance.Nick, evt.Instance.ModelID, evt.Room),
			},
		}
	}
}

func (s ChatScreen) kickModel(nick domain.Nick) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		evt, err := s.sess.Kick(ctx, s.active, nick)
		if err != nil {
			return systemEventMsg{lines: []string{err.Error()}}
		}

		rooms, _ := s.sess.ListRooms(ctx)
		messages, _ := s.sess.Messages(ctx, s.active)

		return commandResultMsg{
			rooms:    rooms,
			active:   s.active,
			messages: messages,
			systemEvents: []string{
				fmt.Sprintf("%s has been kicked from %s", evt.Nick, evt.Room),
			},
		}
	}
}

func (s ChatScreen) listRooms() tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		rooms, err := s.sess.ListRooms(ctx)
		if err != nil {
			return systemEventMsg{lines: []string{err.Error()}}
		}

		if len(rooms) == 0 {
			return systemEventMsg{lines: []string{"no rooms"}}
		}

		lines := make([]string, len(rooms))
		for i, r := range rooms {
			line := string(r.Name)
			if r.Title != "" {
				line += " — " + r.Title
			}

			lines[i] = line
		}

		return systemEventMsg{lines: lines}
	}
}

// View implements ui.Model.
func (s ChatScreen) View(width, height int) string {
	return s.layout.View(width, height)
}
