package components

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

// ChannelSelectedMsg is emitted when the user selects a channel in
// the sidebar, either by pressing ctrl-o or clicking on it.
type ChannelSelectedMsg struct {
	Channel domain.ChannelName
}

// ChannelsUpdatedMsg tells the sidebar to refresh its channel list,
// active channel, and unread counts.
type ChannelsUpdatedMsg struct {
	Channels []domain.Channel
	Active   domain.ChannelName
	Unread   map[domain.ChannelName]int
}

// Sidebar displays the list of open channels and lets the user
// navigate between them.
type Sidebar struct {
	channels []domain.Channel
	cursor   int
	active   domain.ChannelName
	unread   map[domain.ChannelName]int
	keyMap   SidebarKeyMap
}

// NewSidebar creates a sidebar with the given initial channels and
// active channel. The unread map holds per-channel unread counts; nil
// is safe and treated as all-zero.
func NewSidebar(channels []domain.Channel, active domain.ChannelName, unread map[domain.ChannelName]int) Sidebar {
	s := Sidebar{
		channels: channels,
		active:   active,
		unread:   unread,
		keyMap:   DefaultSidebarKeyMap,
	}

	return s.syncCursor()
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

	case ChannelsUpdatedMsg:
		s.channels = msg.Channels
		s.active = msg.Active
		s.unread = msg.Unread
		s = s.syncCursor()
	}

	return s, nil
}

func (s Sidebar) handleKey(msg tea.KeyMsg) (ui.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, s.keyMap.Down):
		s.cursor++
		s = s.clampCursor()

	case key.Matches(msg, s.keyMap.Up):
		s.cursor--
		s = s.clampCursor()

	case key.Matches(msg, s.keyMap.Select):
		if len(s.channels) == 0 {
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

	if msg.Y < 0 || msg.Y >= len(s.channels) {
		return s, nil
	}

	s.cursor = msg.Y

	return s, s.selectCurrent()
}

func (s Sidebar) selectCurrent() tea.Cmd {
	ch := s.channels[s.cursor].Name

	return func() tea.Msg {
		return ChannelSelectedMsg{Channel: ch}
	}
}

func (s Sidebar) syncCursor() Sidebar {
	for i, ch := range s.channels {
		if ch.Name == s.active {
			s.cursor = i
			return s
		}
	}

	return s.clampCursor()
}

func (s Sidebar) clampCursor() Sidebar {
	if len(s.channels) == 0 {
		s.cursor = 0
		return s
	}

	if s.cursor < 0 {
		s.cursor = 0
	}

	if s.cursor >= len(s.channels) {
		s.cursor = len(s.channels) - 1
	}

	return s
}

// View implements ui.Model.
func (s Sidebar) View(width, height int) string {
	if len(s.channels) == 0 {
		return lipgloss.Place(width, height,
			lipgloss.Center, lipgloss.Center,
			theme.Dim.Render("No channels"))
	}

	var b strings.Builder

	for i, ch := range s.channels {
		if i >= height {
			break
		}

		name := string(ch.Name)
		isDM := ch.Kind == domain.KindDM

		if isDM {
			name = "@" + name
		}

		count := s.unread[ch.Name]
		hasUnread := count > 0

		if hasUnread {
			name += fmt.Sprintf(" (%d)", count)
		}

		line := truncate(name, width)

		cursorStyle := theme.ChannelName
		if isDM {
			cursorStyle = theme.DMName
		}

		switch {
		case i == s.cursor && ch.Name == s.active:
			line = theme.ActiveChannel.Render("▸ " + line)
		case i == s.cursor:
			line = cursorStyle.Render("▸ " + line)
		case ch.Name == s.active:
			line = theme.ActiveChannel.Render("  " + line)
		case hasUnread:
			line = theme.UnreadChannel.Render("  " + line)
		default:
			line = theme.InactiveChannel.Render("  " + line)
		}

		b.WriteString(line)

		if i < len(s.channels)-1 {
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
