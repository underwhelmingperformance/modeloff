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
// channel sidebar. ctrl+d and ctrl+u belong to the input editor
// (delete-char and kill-to-line-start). The sidebar is a
// window/panel operation, not an editing one, so its cursor keys use
// alt+up/alt+down: ctrl+j and ctrl+g are also unused, but each
// carries its own readline meaning (accept-line, abort), so binding
// either one would just spend a different ctrl chord the editor might
// still want, leaving ctrl no more free for the editor than before.
var DefaultSidebarKeyMap = SidebarKeyMap{
	Down: ui.Bind(key.NewBinding(
		key.WithKeys("alt+down"),
		key.WithHelp("M-↓", "↓"),
	)),
	Up: ui.Bind(key.NewBinding(
		key.WithKeys("alt+up"),
		key.WithHelp("M-↑", "↑"),
	)),
	Select: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+o"),
		key.WithHelp("^O", "select"),
	)),
}

// EmptySidebarKeyMap carries no key bindings. The nick list has no
// activation semantics today (selecting a member has no visible
// effect), so it uses this keymap and never reacts to the channel
// sidebar's cursor keys.
var EmptySidebarKeyMap = SidebarKeyMap{}

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
	KillLineStart   ui.KeyBinding
	DeleteChar      ui.KeyBinding
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
		key.WithKeys("ctrl+left", "alt+b"),
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
	KillLineStart: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+u"),
		key.WithHelp("^U", "del → start"),
	)),
	DeleteChar: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+d"),
		key.WithHelp("^D", "del char"),
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
	// ToggleBold uses ctrl+b: alt+b is already WordLeft's Emacs
	// pairing, and every other letter a formatting toggle could
	// plausibly use (i, u, r, s, o, c, w, d, f) is already taken by
	// another editor binding. ctrl+b collides with readline's
	// backward-char, but that collision stays inside the editor,
	// matching the pattern the rest of this keymap follows for
	// editing chords: window/panel actions live on alt, and the
	// editor keeps ctrl. Left arrow is a full, always-available
	// substitute for backward-char, so the cost is a readline habit
	// occasionally toggling bold when it expected the cursor to move,
	// not a lost capability. ctrl+b also matches the IRC bold control
	// character mIRC and HexChat use, and it stays safe from the rich
	// text editor ever mistaking it for a literal character to
	// insert, a risk every alt+letter chord carries.
	ToggleBold: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+b"),
		key.WithHelp("^B", "bold"),
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

// ChatScreenKeyMap defines keybindings the chat screen itself owns,
// not any child component.
type ChatScreenKeyMap struct {
	ToggleNickList ui.KeyBinding
}

// DefaultChatScreenKeyMap is the default set of chat screen
// keybindings. ctrl+n now switches to the next window (see
// WindowSwitchKeyMap). Nick-list toggling is a panel operation, not
// an editing one, so it lives in the alt+letter space alongside the
// other panel toggles.
var DefaultChatScreenKeyMap = ChatScreenKeyMap{
	ToggleNickList: ui.Bind(key.NewBinding(
		key.WithKeys("alt+n"),
		key.WithHelp("M-n", "nicks"),
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
// ctrl+l is reserved as the universal terminal-repaint chord, and the
// observability drawer is a panel, not editor, operation, so its
// toggle lives in the alt+letter space with the app's other panel
// toggles.
var DefaultWorkspaceKeyMap = WorkspaceKeyMap{
	ToggleObservability: ui.Bind(key.NewBinding(
		key.WithKeys("alt+l"),
		key.WithHelp("M-l", "logs"),
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

// WindowSwitchKeyMap defines the keybindings for switching between
// open windows (channels and DMs) from anywhere in the chat screen:
// alt+1..alt+9 jump directly to a window by position, alt+a jumps to
// the next window with unseen activity, and ctrl+n/ctrl+p step to the
// next and previous window in list order.
type WindowSwitchKeyMap struct {
	Direct       ui.KeyBinding
	NextActivity ui.KeyBinding
	Next         ui.KeyBinding
	Previous     ui.KeyBinding
}

// DefaultWindowSwitchKeyMap is the default set of window-switch
// keybindings. AGENTS.md's "The flow" point 3 mandates ctrl+n/ctrl+p
// specifically. They do collide with readline's
// next-history/previous-history, but window switching is the same
// "step to the next/previous item in a sequence" action readline's
// ctrl+n/ctrl+p already mean, applied here to windows and there to
// history entries, so the collision reinforces the mnemonic.
var DefaultWindowSwitchKeyMap = WindowSwitchKeyMap{
	Direct: ui.Bind(key.NewBinding(
		key.WithKeys("alt+1", "alt+2", "alt+3", "alt+4", "alt+5", "alt+6", "alt+7", "alt+8", "alt+9"),
		key.WithHelp("M-1..9", "switch window"),
	)),
	NextActivity: ui.Bind(key.NewBinding(
		key.WithKeys("alt+a"),
		key.WithHelp("M-a", "next active"),
	)),
	Next: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+n"),
		key.WithHelp("^N", "next window"),
	)),
	Previous: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+p"),
		key.WithHelp("^P", "prev window"),
	)),
}
