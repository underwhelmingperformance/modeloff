package components

import (
	"github.com/charmbracelet/bubbles/key"

	"github.com/laney/modeloff/internal/ui"
)

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
	Down   ui.KeyBinding
	Up     ui.KeyBinding
	Select ui.KeyBinding
}

// WithHelp returns a copy with the help description overridden for
// the given action.
func (km SidebarKeyMap) WithHelp(action SidebarAction, desc string) SidebarKeyMap {
	override := func(b ui.KeyBinding, desc string) ui.KeyBinding {
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
	Down: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+d"),
		key.WithHelp("^D", "↓"),
	)),
	Up: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+u"),
		key.WithHelp("^U", "↑"),
	)),
	Select: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+o"),
		key.WithHelp("^O", "select"),
	)),
}

// InputBarKeyMap defines keybindings for the input bar component.
type InputBarKeyMap struct {
	Submit          ui.KeyBinding
	HistoryUp       ui.KeyBinding
	HistoryDn       ui.KeyBinding
	WordLeft        ui.KeyBinding
	WordRight       ui.KeyBinding
	DeleteWordBack  ui.KeyBinding
	DeleteWordFwd   ui.KeyBinding
	DeleteToEnd     ui.KeyBinding
	Yank            ui.KeyBinding
	Transpose       ui.KeyBinding
	Home            ui.KeyBinding
	End             ui.KeyBinding
	ToggleBold      ui.KeyBinding
	ToggleItalic    ui.KeyBinding
	ToggleUnderline ui.KeyBinding
	ToggleReverse   ui.KeyBinding
	ToggleStrike    ui.KeyBinding
	OpenPalette     ui.KeyBinding
	ResetFormat     ui.KeyBinding
	CopySelection   ui.KeyBinding
}

// DefaultInputBarKeyMap is the default set of keybindings for the
// input bar.
var DefaultInputBarKeyMap = InputBarKeyMap{
	Submit: ui.Bind(key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("↵", "send"),
	)),
	HistoryUp: ui.Bind(key.NewBinding(
		key.WithKeys("up"),
		key.WithHelp("↑", "history"),
	)),
	HistoryDn: ui.Bind(key.NewBinding(
		key.WithKeys("down"),
		key.WithHelp("↓", "history"),
	)),
	WordLeft: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+left"),
		key.WithHelp("^←", "word ←"),
	)),
	WordRight: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+right", "alt+f"),
		key.WithHelp("^→", "word →"),
	)),
	DeleteWordBack: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+w", "alt+backspace"),
		key.WithHelp("^W", "del word"),
	)),
	DeleteWordFwd: ui.Bind(key.NewBinding(
		key.WithKeys("alt+d"),
		key.WithHelp("M-d", "del next word"),
	)),
	DeleteToEnd: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+k"),
		key.WithHelp("^K", "del → end"),
	)),
	Yank: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+y"),
		key.WithHelp("^Y", "yank"),
	)),
	Transpose: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+t"),
		key.WithHelp("^T", "transpose"),
	)),
	Home: ui.Bind(key.NewBinding(
		key.WithKeys("home", "ctrl+a"),
		key.WithHelp("Home", "line start"),
	)),
	End: ui.Bind(key.NewBinding(
		key.WithKeys("end", "ctrl+e"),
		key.WithHelp("End", "line end"),
	)),
	ToggleBold: ui.Bind(key.NewBinding(
		key.WithKeys("alt+b"),
		key.WithHelp("M-b", "bold"),
	)),
	ToggleItalic: ui.Bind(key.NewBinding(
		key.WithKeys("alt+i"),
		key.WithHelp("M-i", "italic"),
	)),
	ToggleUnderline: ui.Bind(key.NewBinding(
		key.WithKeys("alt+u"),
		key.WithHelp("M-u", "underline"),
	)),
	ToggleReverse: ui.Bind(key.NewBinding(
		key.WithKeys("alt+r"),
		key.WithHelp("M-r", "reverse"),
	)),
	ToggleStrike: ui.Bind(key.NewBinding(
		key.WithKeys("alt+s"),
		key.WithHelp("M-s", "strike"),
	)),
	OpenPalette: ui.Bind(key.NewBinding(
		key.WithKeys("alt+c"),
		key.WithHelp("M-c", "colour"),
	)),
	ResetFormat: ui.Bind(key.NewBinding(
		key.WithKeys("alt+o"),
		key.WithHelp("M-o", "reset fmt"),
	)),
	CopySelection: ui.Bind(key.NewBinding(
		key.WithKeys("alt+w"),
		key.WithHelp("M-w", "copy sel"),
	)),
}

// ChatViewKeyMap defines explicit scroll bindings for the chat
// viewport. Plain arrow keys remain with the input bar.
type ChatViewKeyMap struct {
	PageUp     ui.KeyBinding
	PageDown   ui.KeyBinding
	ScrollUp   ui.KeyBinding
	ScrollDown ui.KeyBinding
}

// DefaultChatViewKeyMap is the default set of chat viewport
// keybindings.
var DefaultChatViewKeyMap = ChatViewKeyMap{
	PageUp: ui.Bind(key.NewBinding(
		key.WithKeys("pgup"),
		key.WithHelp("PgUp", "page up"),
	)),
	PageDown: ui.Bind(key.NewBinding(
		key.WithKeys("pgdown"),
		key.WithHelp("PgDn", "page down"),
	)),
	ScrollUp: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+up"),
		key.WithHelp("^↑", "up"),
	)),
	ScrollDown: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+down"),
		key.WithHelp("^↓", "down"),
	)),
}

// ChatScreenKeyMap defines keybindings owned by the chat screen
// rather than any child component.
type ChatScreenKeyMap struct {
	ToggleNickList ui.KeyBinding
}

// DefaultChatScreenKeyMap is the default set of chat screen
// keybindings.
var DefaultChatScreenKeyMap = ChatScreenKeyMap{
	ToggleNickList: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+n"),
		key.WithHelp("^N", "nicks"),
	)),
}

// WorkspaceKeyMap defines keybindings for the chat workspace and
// observability panes.
type WorkspaceKeyMap struct {
	ToggleObservability ui.KeyBinding
	ToggleFullscreen    ui.KeyBinding
	NextPane            ui.KeyBinding
	ExitFullscreen      ui.KeyBinding
}

// DefaultWorkspaceKeyMap is the default set of workspace bindings.
var DefaultWorkspaceKeyMap = WorkspaceKeyMap{
	ToggleObservability: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+l"),
		key.WithHelp("^L", "logs"),
	)),
	ToggleFullscreen: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+f"),
		key.WithHelp("^F", "fullscreen"),
	)),
	NextPane: ui.Bind(key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("Tab", "next pane"),
	)),
	ExitFullscreen: ui.Bind(key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("Esc", "exit fullscreen"),
	)),
}
