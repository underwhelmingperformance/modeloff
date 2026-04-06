package components

import (
	"fmt"

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
	bounds   ui.Rect
	panel    PanelList
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
		panel:    NewPanelList(),
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
	case ui.BoundsMsg:
		s.bounds = msg.Rect

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

	cmd := s.panel.Update(msg)

	return s, cmd
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

	if !s.bounds.Contains(msg.X, msg.Y) {
		return s, nil
	}

	_, localY := s.bounds.Local(msg.X, msg.Y)
	itemIdx := localY + s.panel.YOffset()

	if itemIdx < 0 || itemIdx >= len(s.channels) {
		return s, nil
	}

	s.cursor = itemIdx

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

// KeyBindings implements ui.Keybinding.
func (s Sidebar) KeyBindings() []key.Binding {
	hasChannels := len(s.channels) > 0

	return []key.Binding{
		ui.WithBindingEnabled(
			key.NewBinding(
				key.WithKeys("ctrl+d", "ctrl+u"),
				key.WithHelp("^D/U", "channels"),
			),
			hasChannels,
		),
		ui.WithBindingEnabled(s.keyMap.Select, hasChannels),
	}
}

// View implements ui.Model.
func (s Sidebar) View(width, height int) string {
	available := width - PanelPadLeft

	items := make([]string, 0, len(s.channels))

	for _, ch := range s.channels {
		name := string(ch.Name)
		isDM := ch.Kind == domain.KindDM

		if isDM {
			name = "@" + name
		}

		count := s.unread[ch.Name]
		if count > 0 {
			name += fmt.Sprintf(" (%d)", count)
		}

		name = truncate(name, available)

		style := theme.InactiveChannel

		switch {
		case ch.Name == s.active:
			style = theme.ActiveChannel
		case count > 0:
			style = theme.UnreadChannel
		}

		items = append(items, style.Render(name))
	}

	return s.panel.Render(width, height, PanelContent{
		Items:  items,
		Cursor: s.cursor,
		Empty:  "No channels",
	})
}

func truncate(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}

	if lipgloss.Width(s) <= maxWidth {
		return s
	}

	runes := []rune(s)
	for len(runes) > 0 && lipgloss.Width(string(runes)) > maxWidth-1 {
		runes = runes[:len(runes)-1]
	}

	return string(runes) + "…"
}
