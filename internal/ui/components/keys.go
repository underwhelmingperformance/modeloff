package components

import "github.com/charmbracelet/bubbles/key"

// SidebarAction identifies a sidebar keybinding action.
type SidebarAction int

// Sidebar keybinding actions.
const (
	SidebarDown SidebarAction = iota
	SidebarUp
	SidebarSelect
)

// SidebarKeyMap defines keybindings for the sidebar component.
type SidebarKeyMap struct {
	Down   key.Binding
	Up     key.Binding
	Select key.Binding
}

// WithHelp returns a copy with the help description overridden for
// the given action.
func (km SidebarKeyMap) WithHelp(action SidebarAction, desc string) SidebarKeyMap {
	override := func(b key.Binding, desc string) key.Binding {
		h := b.Help()
		b.SetHelp(h.Key, desc)

		return b
	}

	switch action {
	case SidebarDown:
		km.Down = override(km.Down, desc)
	case SidebarUp:
		km.Up = override(km.Up, desc)
	case SidebarSelect:
		km.Select = override(km.Select, desc)
	}

	return km
}

// DefaultSidebarKeyMap is the default set of keybindings for the
// sidebar.
var DefaultSidebarKeyMap = SidebarKeyMap{
	Down: key.NewBinding(
		key.WithKeys("ctrl+d"),
		key.WithHelp("^D", "↓"),
	),
	Up: key.NewBinding(
		key.WithKeys("ctrl+u"),
		key.WithHelp("^U", "↑"),
	),
	Select: key.NewBinding(
		key.WithKeys("ctrl+o"),
		key.WithHelp("^O", "select"),
	),
}

// InputBarKeyMap defines keybindings for the input bar component.
type InputBarKeyMap struct {
	Submit    key.Binding
	HistoryUp key.Binding
	HistoryDn key.Binding
}

// DefaultInputBarKeyMap is the default set of keybindings for the
// input bar.
var DefaultInputBarKeyMap = InputBarKeyMap{
	Submit: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("↵", "send"),
	),
	HistoryUp: key.NewBinding(
		key.WithKeys("up"),
		key.WithHelp("↑", "history"),
	),
	HistoryDn: key.NewBinding(
		key.WithKeys("down"),
		key.WithHelp("↓", "history"),
	),
}

// ChatViewKeyMap defines explicit scroll bindings for the chat
// viewport. Plain arrow keys remain with the input bar.
type ChatViewKeyMap struct {
	PageUp     key.Binding
	PageDown   key.Binding
	ScrollUp   key.Binding
	ScrollDown key.Binding
}

// DefaultChatViewKeyMap is the default set of chat viewport
// keybindings.
var DefaultChatViewKeyMap = ChatViewKeyMap{
	PageUp: key.NewBinding(
		key.WithKeys("pgup"),
		key.WithHelp("PgUp", "page up"),
	),
	PageDown: key.NewBinding(
		key.WithKeys("pgdown"),
		key.WithHelp("PgDn", "page down"),
	),
	ScrollUp: key.NewBinding(
		key.WithKeys("ctrl+up"),
		key.WithHelp("^↑", "up"),
	),
	ScrollDown: key.NewBinding(
		key.WithKeys("ctrl+down"),
		key.WithHelp("^↓", "down"),
	),
}

// ChatScreenKeyMap defines keybindings owned by the chat screen
// rather than any child component.
type ChatScreenKeyMap struct {
	ToggleNickList key.Binding
}

// DefaultChatScreenKeyMap is the default set of chat screen
// keybindings.
var DefaultChatScreenKeyMap = ChatScreenKeyMap{
	ToggleNickList: key.NewBinding(
		key.WithKeys("ctrl+n"),
		key.WithHelp("^N", "nicks"),
	),
}

// WorkspaceKeyMap defines keybindings for the chat workspace and
// observability panes.
type WorkspaceKeyMap struct {
	ToggleObservability key.Binding
	ToggleFullscreen    key.Binding
	NextPane            key.Binding
	ExitFullscreen      key.Binding
}

// DefaultWorkspaceKeyMap is the default set of workspace bindings.
var DefaultWorkspaceKeyMap = WorkspaceKeyMap{
	ToggleObservability: key.NewBinding(
		key.WithKeys("ctrl+l"),
		key.WithHelp("^L", "logs"),
	),
	ToggleFullscreen: key.NewBinding(
		key.WithKeys("ctrl+f"),
		key.WithHelp("^F", "fullscreen"),
	),
	NextPane: key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("Tab", "next pane"),
	),
	ExitFullscreen: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("Esc", "exit fullscreen"),
	),
}
