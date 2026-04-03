package components

import "github.com/charmbracelet/bubbles/key"

// SidebarKeyMap defines keybindings for the sidebar component.
type SidebarKeyMap struct {
	Down   key.Binding
	Up     key.Binding
	Select key.Binding
}

// DefaultSidebarKeyMap is the default set of keybindings for the
// sidebar.
var DefaultSidebarKeyMap = SidebarKeyMap{
	Down: key.NewBinding(
		key.WithKeys("ctrl+d"),
		key.WithHelp("^D", "↓ channels"),
	),
	Up: key.NewBinding(
		key.WithKeys("ctrl+u"),
		key.WithHelp("^U", "↑ channels"),
	),
	Select: key.NewBinding(
		key.WithKeys("ctrl+o"),
		key.WithHelp("^O", "switch"),
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
