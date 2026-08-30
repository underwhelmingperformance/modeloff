package components

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/laney/modeloff/internal/ui"
)

const keyChordModifiers tea.KeyMod = tea.ModShift | tea.ModAlt | tea.ModCtrl |
	tea.ModMeta | tea.ModHyper | tea.ModSuper

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
	)).WithHelpMetadata(ui.KeyHelpNavigation, ui.KeyHintLow),
	Up: ui.Bind(key.NewBinding(
		key.WithKeys("alt+up"),
		key.WithHelp("M-↑", "↑"),
	)).WithHelpMetadata(ui.KeyHelpNavigation, ui.KeyHintLow),
	Select: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+o"),
		key.WithHelp("^O", "select"),
	)).WithHelpMetadata(ui.KeyHelpNavigation, ui.KeyHintLow),
}

// EmptySidebarKeyMap carries no key bindings. The nick list has no
// activation semantics today (selecting a member has no visible
// effect), so it uses this keymap and never reacts to the channel
// sidebar's cursor keys.
var EmptySidebarKeyMap = SidebarKeyMap{}

// RichTextareaKeyMap binds the keys the editor handles while it is
// editing text: movement, selection, the readline kills, and the IRC
// formatting toggles. One binding covers a movement and its
// shift-extended form, because the editor reads the shift modifier off
// the key and extends the selection with it. The keys the editor
// handles while its colour palette is open are in
// [ColourPaletteKeyMap].
//
// The editor is the only thing that routes these, and its keymap is the
// only thing that describes them, so a key it handles is a key the
// status bar and the help view can name.
type RichTextareaKeyMap struct {
	Left            ui.KeyBinding
	Right           ui.KeyBinding
	Up              ui.KeyBinding
	Down            ui.KeyBinding
	WordLeft        ui.KeyBinding
	WordRight       ui.KeyBinding
	LineStart       ui.KeyBinding
	LineEnd         ui.KeyBinding
	Backspace       ui.KeyBinding
	Delete          ui.KeyBinding
	DeleteWordBack  ui.KeyBinding
	DeleteWordFwd   ui.KeyBinding
	DeleteToEnd     ui.KeyBinding
	Transpose       ui.KeyBinding
	Yank            ui.KeyBinding
	Newline         ui.KeyBinding
	ToggleBold      ui.KeyBinding
	ToggleItalic    ui.KeyBinding
	ToggleUnderline ui.KeyBinding
	ToggleReverse   ui.KeyBinding
	ToggleStrike    ui.KeyBinding
	OpenPalette     ui.KeyBinding
	ResetFormat     ui.KeyBinding
}

// DefaultRichTextareaKeyMap is the editor's default set. The movement
// and kill chords follow readline where the terminal has a binding for
// one. A letter chord is spelled once, in lower case, because
// [ui.Matches] folds the shift a terminal may report with it.
var DefaultRichTextareaKeyMap = RichTextareaKeyMap{
	Left: ui.Bind(key.NewBinding(
		key.WithKeys("left", "shift+left"),
		key.WithHelp("←", "left"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
	Right: ui.Bind(key.NewBinding(
		key.WithKeys("right", "shift+right"),
		key.WithHelp("→", "right"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
	Up: ui.Bind(key.NewBinding(
		key.WithKeys("up", "shift+up"),
		key.WithHelp("↑", "up"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
	Down: ui.Bind(key.NewBinding(
		key.WithKeys("down", "shift+down"),
		key.WithHelp("↓", "down"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
	// alt+b and alt+f are the Emacs pairing for word movement, which
	// is why bold takes ctrl+b below.
	WordLeft: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+left", "ctrl+shift+left", "alt+b"),
		key.WithHelp("^←", "word ←"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
	WordRight: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+right", "ctrl+shift+right", "alt+f"),
		key.WithHelp("^→", "word →"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
	LineStart: ui.Bind(key.NewBinding(
		key.WithKeys("home", "shift+home", "ctrl+a"),
		key.WithHelp("Home", "line start"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
	LineEnd: ui.Bind(key.NewBinding(
		key.WithKeys("end", "shift+end", "ctrl+e"),
		key.WithHelp("End", "line end"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
	Backspace: ui.Bind(key.NewBinding(
		key.WithKeys("backspace"),
		key.WithHelp("⌫", "delete back"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
	Delete: ui.Bind(key.NewBinding(
		key.WithKeys("delete"),
		key.WithHelp("Del", "delete"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
	DeleteWordBack: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+w", "alt+backspace"),
		key.WithHelp("^W", "del word"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
	DeleteWordFwd: ui.Bind(key.NewBinding(
		key.WithKeys("alt+d"),
		key.WithHelp("M-d", "del next word"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
	DeleteToEnd: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+k"),
		key.WithHelp("^K", "del → end"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
	Transpose: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+t"),
		key.WithHelp("^T", "transpose"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
	Yank: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+y"),
		key.WithHelp("^Y", "yank"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
	Newline: ui.Bind(key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("↵", "newline"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
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
	)).WithHelpMetadata(ui.KeyHelpFormatting, ui.KeyHintNone),
	ToggleItalic: ui.Bind(key.NewBinding(
		key.WithKeys("alt+i"),
		key.WithHelp("M-i", "italic"),
	)).WithHelpMetadata(ui.KeyHelpFormatting, ui.KeyHintNone),
	ToggleUnderline: ui.Bind(key.NewBinding(
		key.WithKeys("alt+u"),
		key.WithHelp("M-u", "underline"),
	)).WithHelpMetadata(ui.KeyHelpFormatting, ui.KeyHintNone),
	ToggleReverse: ui.Bind(key.NewBinding(
		key.WithKeys("alt+r"),
		key.WithHelp("M-r", "reverse"),
	)).WithHelpMetadata(ui.KeyHelpFormatting, ui.KeyHintNone),
	ToggleStrike: ui.Bind(key.NewBinding(
		key.WithKeys("alt+s"),
		key.WithHelp("M-s", "strike"),
	)).WithHelpMetadata(ui.KeyHelpFormatting, ui.KeyHintNone),
	OpenPalette: ui.Bind(key.NewBinding(
		key.WithKeys("alt+c"),
		key.WithHelp("M-c", "colour"),
	)).WithHelpMetadata(ui.KeyHelpFormatting, ui.KeyHintNone),
	ResetFormat: ui.Bind(key.NewBinding(
		key.WithKeys("alt+o"),
		key.WithHelp("M-o", "reset fmt"),
	)).WithHelpMetadata(ui.KeyHelpFormatting, ui.KeyHintNone),
}

// PopoverKeyMap binds the keys the completion popover handles.
//
// AcceptWithEnter is the input bar's own send key, which the popover
// takes when accepting the highlighted suggestion would replace the
// typed prefix. It is bound here as well so that what takes the key
// and what describes it stay the same thing.
type PopoverKeyMap struct {
	Accept          ui.KeyBinding
	AcceptWithEnter ui.KeyBinding
	Navigate        ui.KeyBinding
	Dismiss         ui.KeyBinding
}

// DefaultPopoverKeyMap is the default set of completion-popover
// keybindings.
var DefaultPopoverKeyMap = PopoverKeyMap{
	Accept: ui.Bind(key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("Tab", "accept"),
	)).WithHelpMetadata(ui.KeyHelpCompletion, ui.KeyHintHigh),
	AcceptWithEnter: ui.Bind(key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("↵", "accept"),
	)).WithHelpMetadata(ui.KeyHelpCompletion, ui.KeyHintHigh),
	Navigate: ui.Bind(key.NewBinding(
		key.WithKeys("up", "down", "shift+tab"),
		key.WithHelp("↑↓", "navigate"),
	)).WithHelpMetadata(ui.KeyHelpCompletion, ui.KeyHintHigh),
	Dismiss: ui.Bind(key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("Esc", "dismiss"),
	)).WithHelpMetadata(ui.KeyHelpCompletion, ui.KeyHintEssential),
}

// Bindings returns every binding in the map, in declaration order.
func (m PopoverKeyMap) Bindings() []ui.KeyBinding {
	return []ui.KeyBinding{m.Accept, m.AcceptWithEnter, m.Navigate, m.Dismiss}
}

// ColourPaletteKeyMap binds the keys the editor handles while its
// colour palette is open. The palette takes these before the editing
// keys are consulted, so an open palette is a mode of its own and not
// an addition to the editing keys.
type ColourPaletteKeyMap struct {
	Prev         ui.KeyBinding
	Next         ui.KeyBinding
	JumpSwatch   ui.KeyBinding
	ToggleTarget ui.KeyBinding
	Apply        ui.KeyBinding
	Close        ui.KeyBinding
}

// DefaultColourPaletteKeyMap is the default set of colour-palette
// keybindings.
var DefaultColourPaletteKeyMap = ColourPaletteKeyMap{
	Prev: ui.Bind(key.NewBinding(
		key.WithKeys("left"),
		key.WithHelp("←", "prev swatch"),
	)).WithHelpMetadata(ui.KeyHelpFormatting, ui.KeyHintNormal),
	Next: ui.Bind(key.NewBinding(
		key.WithKeys("right"),
		key.WithHelp("→", "next swatch"),
	)).WithHelpMetadata(ui.KeyHelpFormatting, ui.KeyHintNormal),
	JumpSwatch: ui.Bind(key.NewBinding(
		key.WithKeys("0", "1", "2", "3", "4", "5", "6", "7", "8", "9"),
		key.WithHelp("0-9", "jump"),
	)).WithHelpMetadata(ui.KeyHelpFormatting, ui.KeyHintLow),
	ToggleTarget: ui.Bind(key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("Tab", "fg/bg"),
	)).WithHelpMetadata(ui.KeyHelpFormatting, ui.KeyHintNormal),
	Apply: ui.Bind(key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("↵", "apply"),
	)).WithHelpMetadata(ui.KeyHelpFormatting, ui.KeyHintHigh),
	Close: ui.Bind(key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("Esc", "dismiss"),
	)).WithHelpMetadata(ui.KeyHelpFormatting, ui.KeyHintEssential),
}

// InputBarKeyMap defines the keys the input bar handles itself,
// before the editor sees them. The keys the editor handles are in
// [RichTextareaKeyMap].
type InputBarKeyMap struct {
	Submit        ui.KeyBinding
	HistoryUp     ui.KeyBinding
	HistoryDn     ui.KeyBinding
	KillLineStart ui.KeyBinding
	DeleteChar    ui.KeyBinding
	CopySelection ui.KeyBinding
}

// DefaultInputBarKeyMap is the default set of keybindings for the
// input bar.
var DefaultInputBarKeyMap = InputBarKeyMap{
	Submit: ui.Bind(key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("↵", "send"),
	)).WithHelpMetadata(ui.KeyHelpMessaging, ui.KeyHintNormal),
	HistoryUp: ui.Bind(key.NewBinding(
		key.WithKeys("up"),
		key.WithHelp("↑", "history"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
	HistoryDn: ui.Bind(key.NewBinding(
		key.WithKeys("down"),
		key.WithHelp("↓", "history"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
	KillLineStart: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+u"),
		key.WithHelp("^U", "del → start"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
	DeleteChar: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+d"),
		key.WithHelp("^D", "del char"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
	CopySelection: ui.Bind(key.NewBinding(
		key.WithKeys("alt+w"),
		key.WithHelp("M-w", "copy sel"),
	)).WithHelpMetadata(ui.KeyHelpEditing, ui.KeyHintNone),
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
	)).WithHelpMetadata(ui.KeyHelpNavigation, ui.KeyHintNone),
	PageDown: ui.Bind(key.NewBinding(
		key.WithKeys("pgdown"),
		key.WithHelp("PgDn", "page down"),
	)).WithHelpMetadata(ui.KeyHelpNavigation, ui.KeyHintNone),
	ScrollUp: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+up"),
		key.WithHelp("^↑", "up"),
	)).WithHelpMetadata(ui.KeyHelpNavigation, ui.KeyHintNone),
	ScrollDown: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+down"),
		key.WithHelp("^↓", "down"),
	)).WithHelpMetadata(ui.KeyHelpNavigation, ui.KeyHintNone),
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
	)).WithHelpMetadata(ui.KeyHelpPanels, ui.KeyHintLow),
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
	)).WithHelpMetadata(ui.KeyHelpPanels, ui.KeyHintLow),
	ToggleFullscreen: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+f"),
		key.WithHelp("^F", "fullscreen"),
	)).WithHelpMetadata(ui.KeyHelpPanels, ui.KeyHintLow),
	NextPane: ui.Bind(key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("Tab", "next pane"),
	)).WithHelpMetadata(ui.KeyHelpPanels, ui.KeyHintHigh),
	ExitFullscreen: ui.Bind(key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("Esc", "exit fullscreen"),
	)).WithHelpMetadata(ui.KeyHelpPanels, ui.KeyHintEssential),
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
	)).WithHelpMetadata(ui.KeyHelpNavigation, ui.KeyHintLow),
	NextActivity: ui.Bind(key.NewBinding(
		key.WithKeys("alt+a"),
		key.WithHelp("M-a", "next active"),
	)).WithHelpMetadata(ui.KeyHelpNavigation, ui.KeyHintNormal),
	Next: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+n"),
		key.WithHelp("^N", "next window"),
	)).WithHelpMetadata(ui.KeyHelpNavigation, ui.KeyHintNormal),
	Previous: ui.Bind(key.NewBinding(
		key.WithKeys("ctrl+p"),
		key.WithHelp("^P", "prev window"),
	)).WithHelpMetadata(ui.KeyHelpNavigation, ui.KeyHintLow),
}

// Bindings returns every binding in the map, in declaration order.
func (m RichTextareaKeyMap) Bindings() []ui.KeyBinding {
	return []ui.KeyBinding{
		m.Left, m.Right, m.Up, m.Down,
		m.WordLeft, m.WordRight,
		m.LineStart, m.LineEnd,
		m.Backspace, m.Delete,
		m.DeleteWordBack, m.DeleteWordFwd, m.DeleteToEnd,
		m.Transpose, m.Yank, m.Newline,
		m.ToggleBold, m.ToggleItalic, m.ToggleUnderline,
		m.ToggleReverse, m.ToggleStrike,
		m.OpenPalette, m.ResetFormat,
	}
}

// Bindings returns every binding in the map, in declaration order.
func (m ColourPaletteKeyMap) Bindings() []ui.KeyBinding {
	return []ui.KeyBinding{
		m.Prev, m.Next, m.JumpSwatch,
		m.ToggleTarget, m.Apply, m.Close,
	}
}

// Bindings returns every binding in the map, in declaration order.
func (m InputBarKeyMap) Bindings() []ui.KeyBinding {
	return []ui.KeyBinding{
		m.Submit, m.HistoryUp, m.HistoryDn,
		m.KillLineStart, m.DeleteChar, m.CopySelection,
	}
}

// Bindings returns every binding in the map, in declaration order.
func (m ChatViewKeyMap) Bindings() []ui.KeyBinding {
	return []ui.KeyBinding{m.PageUp, m.PageDown, m.ScrollUp, m.ScrollDown}
}
