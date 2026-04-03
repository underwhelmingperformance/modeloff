package components

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

// RoomSelectedMsg is emitted when the user selects a room in the
// sidebar, either by pressing ctrl-o or clicking on it.
type RoomSelectedMsg struct {
	Room domain.RoomName
}

// RoomsUpdatedMsg tells the sidebar to refresh its room list.
type RoomsUpdatedMsg struct {
	Rooms  []domain.Room
	Active domain.RoomName
}

// Sidebar displays the list of open rooms and lets the user navigate
// between them.
type Sidebar struct {
	rooms  []domain.Room
	cursor int
	active domain.RoomName
}

// NewSidebar creates a sidebar with the given initial rooms and
// active room.
func NewSidebar(rooms []domain.Room, active domain.RoomName) Sidebar {
	cursor := 0

	for i, r := range rooms {
		if r.Name == active {
			cursor = i
			break
		}
	}

	return Sidebar{
		rooms:  rooms,
		cursor: cursor,
		active: active,
	}
}

// Init implements ui.Model.
func (s Sidebar) Init() tea.Cmd {
	return nil
}

// Update implements ui.Model.
func (s Sidebar) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return s.handleKey(msg)

	case tea.MouseMsg:
		return s.handleMouse(msg)

	case RoomsUpdatedMsg:
		s.rooms = msg.Rooms
		s.active = msg.Active
		s = s.clampCursor()
	}

	return s, nil
}

func (s Sidebar) handleKey(msg tea.KeyMsg) (ui.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+d":
		s.cursor++
		s = s.clampCursor()

	case "ctrl+u":
		s.cursor--
		s = s.clampCursor()

	case "ctrl+o":
		if len(s.rooms) == 0 {
			return s, nil
		}

		return s, s.selectCurrent()
	}

	return s, nil
}

func (s Sidebar) handleMouse(msg tea.MouseMsg) (ui.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return s, nil
	}

	if msg.Y < 0 || msg.Y >= len(s.rooms) {
		return s, nil
	}

	s.cursor = msg.Y

	return s, s.selectCurrent()
}

func (s Sidebar) selectCurrent() tea.Cmd {
	room := s.rooms[s.cursor].Name

	return func() tea.Msg {
		return RoomSelectedMsg{Room: room}
	}
}

func (s Sidebar) clampCursor() Sidebar {
	if len(s.rooms) == 0 {
		s.cursor = 0
		return s
	}

	if s.cursor < 0 {
		s.cursor = 0
	}

	if s.cursor >= len(s.rooms) {
		s.cursor = len(s.rooms) - 1
	}

	return s
}

// View implements ui.Model.
func (s Sidebar) View(width, height int) string {
	if len(s.rooms) == 0 {
		return lipgloss.Place(width, height,
			lipgloss.Center, lipgloss.Center,
			theme.Dim.Render("No rooms"))
	}

	var b strings.Builder

	for i, room := range s.rooms {
		if i >= height {
			break
		}

		name := string(room.Name)
		line := truncate(name, width)

		switch {
		case i == s.cursor && room.Name == s.active:
			line = theme.ActiveRoom.Render("▸ " + line)
		case i == s.cursor:
			line = theme.RoomName.Render("▸ " + line)
		case room.Name == s.active:
			line = theme.ActiveRoom.Render("  " + line)
		default:
			line = theme.InactiveRoom.Render("  " + line)
		}

		b.WriteString(line)

		if i < len(s.rooms)-1 {
			b.WriteByte('\n')
		}
	}

	return b.String()
}

func truncate(s string, maxWidth int) string {
	// Account for the "▸ " or "  " prefix (2 chars + space).
	available := maxWidth - 3
	if available <= 0 {
		return ""
	}

	if lipgloss.Width(s) <= available {
		return s
	}

	// Truncate rune by rune until it fits.
	runes := []rune(s)
	for len(runes) > 0 && lipgloss.Width(string(runes)) > available-1 {
		runes = runes[:len(runes)-1]
	}

	return string(runes) + "…"
}
