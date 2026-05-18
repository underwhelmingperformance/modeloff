package components

import (
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ircfmt"
	"github.com/laney/modeloff/internal/richtext"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/clipboard"
	"github.com/laney/modeloff/internal/ui/theme"
)

// ActiveFormats reports which formatting styles are currently active
// at the cursor position in the input bar.
type ActiveFormats struct {
	Bold      bool
	Italic    bool
	Underline bool
	Reverse   bool
	Strike    bool
}

// MessageSubmitMsg is emitted when the user presses Enter with a
// non-command message.
type MessageSubmitMsg struct {
	Text string
}

// CommandSubmitMsg is emitted when the user presses Enter with a
// message starting with "/".
type CommandSubmitMsg struct {
	Raw string
}

// InputLockedMsg toggles the input bar's locked state. While locked,
// keyboard input and submissions are ignored. Used while the client
// is shutting down to prevent the user typing into a UI that is
// about to disappear.
type InputLockedMsg struct {
	Locked bool
}

// historySize is the maximum number of entries kept in the input
// history ring buffer.
const historySize = 50

// nickCompletion holds the transient state for Tab-cycling through
// nick matches.
type nickCompletion struct {
	active  bool
	prefix  string
	start   int
	end     int
	matches []domain.Nick
	index   int
}

// InputBar wraps the rich composer with command completion, nick
// completion, and input history. It owns the command popover as a
// child model.
type InputBar struct {
	input    RichTextarea
	keyMap   InputBarKeyMap
	userNick domain.Nick
	popover  Popover
	bounds   ui.Rect

	history         []string
	histPos         int // -1 = editing new input, 0..len(history)-1 = browsing
	histDraft       string
	histDraftCursor int

	nicks    []domain.Nick
	nickComp nickCompletion

	locked bool
}

// NewInputBar creates an input bar with an optional user nick. When
// called with no arguments, the nick is left empty (set later via
// UserNickMsg). A single argument sets the initial nick.
func NewInputBar(nick ...domain.Nick) InputBar {
	editor := NewRichTextarea(RichTextareaConfig{
		SingleLine:      true,
		Wrap:            false,
		AllowFormatting: true,
	})

	b := InputBar{
		input:   editor,
		keyMap:  DefaultInputBarKeyMap,
		popover: NewPopover(),
		histPos: -1,
	}

	if len(nick) > 0 {
		b.userNick = nick[0]
	}

	return b
}

// Init implements ui.Model.
func (b InputBar) Init() tea.Cmd {
	return b.input.Init()
}

// Update implements ui.Model.
func (b InputBar) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case InputLockedMsg:
		b.locked = msg.Locked
		return b, nil

	case UserNickMsg:
		b.userNick = msg.Nick
		return b, nil

	case NickListUpdatedMsg:
		b.nicks = slices.Collect(msg.Members.Nicks())
		return b, nil

	case CompleterMsg:
		b = b.refreshPopover(PopoverApplyMsg{
			Completer: msg.Completer,
			Raw:       b.input.Value(),
			Cursor:    b.input.Cursor(),
		})
		return b, nil

	case PopoverAcceptMsg:
		b = b.ReplaceRange(msg.ReplaceStart, msg.ReplaceEnd, msg.Replacement)
		b = b.refreshPopover(PopoverRefreshMsg{
			Raw:    b.input.Value(),
			Cursor: b.input.Cursor(),
		})
		return b, nil

	case ui.BoundsMsg:
		b.bounds = msg.Rect
		b = b.refreshPopover(msg)
		return b, nil

	case tea.MouseMsg:
		if updated, handled, cmd := b.handleMouse(msg); handled {
			return updated, cmd
		}

		return b, nil

	case tea.KeyMsg:
		if b.locked {
			return b, nil
		}

		return b.handleKey(msg)
	}

	if b.locked {
		return b, nil
	}

	var cmd tea.Cmd
	updated, cmd := b.input.Update(msg)
	b.input = updated.(RichTextarea)

	return b, cmd
}

func (b InputBar) handleKey(msg tea.KeyMsg) (ui.Model, tea.Cmd) {
	// When the popover is visible, give it first shot at keys.
	if b.popover.IsVisible() {
		updated, cmd := b.popover.Update(msg)
		b.popover = updated.(Popover)

		if b.popover.Handled() {
			return b, cmd
		}
	}

	// When the colour palette is open, the rich textarea owns
	// Esc/Tab/Left/Right/Enter and digit jumps. Forward the key
	// straight through so the input bar's Submit, history, and
	// nick-completion bindings don't swallow them.
	if b.input.PaletteVisible() {
		b.nickComp.active = false
		updated, cmd := b.input.Update(msg)
		b.input = updated.(RichTextarea)
		return b, cmd
	}

	switch {
	case ui.Matches(msg, b.keyMap.Submit):
		return b.submit()

	case ui.Matches(msg, b.keyMap.CopySelection):
		return b, clipboard.CopyCmd(b.input.SelectedText())

	case ui.Matches(msg, b.keyMap.HistoryUp):
		if !b.popover.IsVisible() {
			b = b.historyUp()
			b = b.refreshPopover(PopoverRefreshMsg{
				Raw:    b.input.Value(),
				Cursor: b.input.Cursor(),
			})
			return b, nil
		}

	case ui.Matches(msg, b.keyMap.HistoryDn):
		if !b.popover.IsVisible() {
			b = b.historyDown()
			b = b.refreshPopover(PopoverRefreshMsg{
				Raw:    b.input.Value(),
				Cursor: b.input.Cursor(),
			})
			return b, nil
		}

	case msg.Type == tea.KeyTab:
		if !strings.HasPrefix(b.input.Value(), "/") {
			return b.completeNick(), nil
		}
	}

	b.nickComp.active = false

	b.input = b.input.SetAllowFormatting(!strings.HasPrefix(b.input.Value(), "/"))
	updated, cmd := b.input.Update(msg)
	b.input = updated.(RichTextarea)
	b.input = b.input.SetAllowFormatting(!strings.HasPrefix(b.input.Value(), "/"))

	b = b.refreshPopover(PopoverRefreshMsg{
		Raw:    b.input.Value(),
		Cursor: b.input.Cursor(),
	})

	return b, cmd
}

func (b InputBar) handleMouse(msg tea.MouseMsg) (InputBar, bool, tea.Cmd) {
	if b.bounds.Width == 0 || b.bounds.Height == 0 {
		return b, false, nil
	}

	inputRect := b.inputRect()
	popoverLayout := b.popover.Layout(b.bounds, inputRect)

	if popoverLayout.Rect.Contains(msg.X, msg.Y) {
		updated, cmd := b.popover.Update(msg)
		b.popover = updated.(Popover)

		return b, true, cmd
	}

	if b.popover.IsVisible() && msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
		b = b.refreshPopover(PopoverDismissMsg{Raw: b.input.Value()})
	}

	if inputRect.Contains(msg.X, msg.Y) {
		switch msg.Action {
		case tea.MouseActionPress:
			if msg.Button == tea.MouseButtonLeft {
				localX, _ := inputRect.Local(msg.X, msg.Y)
				b = b.SetCursorFromCell(localX)
				b = b.refreshPopover(PopoverRefreshMsg{
					Raw:    b.input.Value(),
					Cursor: b.input.Cursor(),
				})

				return b, true, nil
			}
		case tea.MouseActionMotion:
			return b, true, nil
		}
	}

	return b, false, nil
}

func (b InputBar) submit() (ui.Model, tea.Cmd) {
	text := strings.TrimSpace(b.input.Value())
	if text == "" {
		return b, nil
	}

	raw := b.rawValue()
	b = b.pushHistory(raw)
	b.input = NewRichTextarea(RichTextareaConfig{
		SingleLine:      true,
		Wrap:            false,
		AllowFormatting: true,
	})
	b.histPos = -1
	b.histDraft = ""

	b = b.refreshPopover(PopoverRefreshMsg{
		Raw:    b.input.Value(),
		Cursor: b.input.Cursor(),
	})

	if strings.HasPrefix(text, "/") {
		return b, func() tea.Msg {
			return CommandSubmitMsg{Raw: text}
		}
	}

	return b, func() tea.Msg {
		return MessageSubmitMsg{Text: raw}
	}
}

func (b InputBar) pushHistory(text string) InputBar {
	if len(b.history) > 0 && b.history[len(b.history)-1] == text {
		return b
	}

	b.history = append(b.history, text)

	if len(b.history) > historySize {
		b.history = b.history[len(b.history)-historySize:]
	}

	return b
}

func (b InputBar) historyUp() InputBar {
	if len(b.history) == 0 {
		return b
	}

	if b.histPos == -1 {
		b.histDraft = b.rawValue()
		b.histDraftCursor = b.input.Cursor()
		b.histPos = len(b.history) - 1
	} else if b.histPos > 0 {
		b.histPos--
	} else {
		return b
	}

	b = b.setRawValue(b.history[b.histPos])

	return b
}

func (b InputBar) historyDown() InputBar {
	if b.histPos == -1 {
		return b
	}

	if b.histPos < len(b.history)-1 {
		b.histPos++
		b = b.setRawValue(b.history[b.histPos])
		return b
	}

	b.histPos = -1
	b = b.setRawValue(b.histDraft)
	b.input = b.input.SetCursorFromRuneIndex(b.histDraftCursor)
	b.histDraft = ""
	b.histDraftCursor = 0

	return b
}

func (b InputBar) completeNick() InputBar {
	if len(b.nicks) == 0 {
		return b
	}

	if b.nickComp.active {
		b.nickComp.index = (b.nickComp.index + 1) % len(b.nickComp.matches)
		return b.applyNickCompletion()
	}

	value := []rune(b.input.Value())
	cursor := b.input.Cursor()

	// Find the word boundary before the cursor.
	start := cursor
	for start > 0 && value[start-1] != ' ' {
		start--
	}

	if start == cursor {
		return b
	}

	prefix := strings.ToLower(string(value[start:cursor]))
	matches := b.matchNicks(prefix)

	if len(matches) == 0 {
		return b
	}

	b.nickComp = nickCompletion{
		active:  true,
		prefix:  prefix,
		start:   start,
		end:     cursor,
		matches: matches,
		index:   0,
	}

	return b.applyNickCompletion()
}

func (b InputBar) matchNicks(prefix string) []domain.Nick {
	var matches []domain.Nick

	for _, nick := range b.nicks {
		if strings.HasPrefix(strings.ToLower(string(nick)), prefix) {
			matches = append(matches, nick)
		}
	}

	slices.Sort(matches)

	return matches
}

func (b InputBar) applyNickCompletion() InputBar {
	nick := b.nickComp.matches[b.nickComp.index]
	value := []rune(b.input.Value())

	// At the start of the line, append ": ". Mid-line, append " "
	// unless the next character is already a space.
	suffix := " "
	if b.nickComp.start == 0 {
		suffix = ": "
	} else if b.nickComp.end < len(value) && value[b.nickComp.end] == ' ' {
		suffix = ""
	}

	replacement := string(nick) + suffix

	b.input = b.input.ReplaceRange(b.nickComp.start, b.nickComp.end, replacement)

	newEnd := b.nickComp.start + len([]rune(replacement))
	b.input = b.input.SetCursorFromRuneIndex(newEnd)
	b.nickComp.end = newEnd

	return b
}

// Value returns the current text in the input buffer.
func (b InputBar) Value() string {
	return b.input.Value()
}

// Cursor returns the cursor position in runes.
func (b InputBar) Cursor() int {
	return b.input.Cursor()
}

// ReplaceRange replaces the given rune range with the provided text.
func (b InputBar) ReplaceRange(start, end int, replacement string) InputBar {
	value := []rune(b.input.Value())
	start = clampInputIndex(start, len(value))
	end = clampInputIndex(end, len(value))
	end = max(end, start)
	b.input = b.input.ReplaceRange(start, end, replacement)

	return b
}

// SetCursorFromCell moves the cursor to the nearest cell within the input area.
func (b InputBar) SetCursorFromCell(x int) InputBar {
	x -= b.prefixWidth()

	if x <= 0 {
		b.input = b.input.SetCursorFromRuneIndex(0)
		return b
	}

	b.input = b.input.SetCursorFromCell(x)

	return b
}

// KeyBindings implements ui.Keybinding.
func (b InputBar) KeyBindings() []ui.KeyBinding {
	if b.popover.IsVisible() {
		return []ui.KeyBinding{
			b.keyMap.Submit,
			ui.WithBindingEnabled(
				ui.Bind(key.NewBinding(
					key.WithKeys("tab"),
					key.WithHelp("Tab", "accept"),
				)),
				b.popover.HasSuggestions(),
			),
			ui.WithBindingEnabled(
				ui.Bind(key.NewBinding(
					key.WithKeys("up", "down", "shift+tab"),
					key.WithHelp("↑↓", "navigate"),
				)),
				b.popover.HasSuggestions(),
			),
			ui.Bind(key.NewBinding(
				key.WithKeys("esc"),
				key.WithHelp("Esc", "dismiss"),
			)),
		}
	}

	if b.input.PaletteVisible() {
		return []ui.KeyBinding{
			ui.Bind(key.NewBinding(
				key.WithKeys("left", "right"),
				key.WithHelp("←→", "swatch"),
			)),
			ui.Bind(key.NewBinding(
				key.WithKeys("0", "1", "2", "3", "4", "5", "6", "7", "8", "9"),
				key.WithHelp("0-9", "jump"),
			)),
			ui.Bind(key.NewBinding(
				key.WithKeys("tab"),
				key.WithHelp("Tab", "fg/bg"),
			)),
			ui.Bind(key.NewBinding(
				key.WithKeys("enter"),
				key.WithHelp("↵", "apply"),
			)),
			ui.Bind(key.NewBinding(
				key.WithKeys("esc"),
				key.WithHelp("Esc", "dismiss"),
			)),
		}
	}

	bindings := []ui.KeyBinding{
		b.keyMap.Submit,
		ui.WithBindingEnabled(b.keyMap.HistoryUp, len(b.history) > 0),
		ui.WithBindingEnabled(b.keyMap.HistoryDn, len(b.history) > 0),
		b.keyMap.WordLeft,
		b.keyMap.WordRight,
		b.keyMap.DeleteWordBack,
		b.keyMap.DeleteWordFwd,
		b.keyMap.DeleteToEnd,
		ui.WithBindingEnabled(b.keyMap.Yank, len(b.input.killRing) > 0),
		b.keyMap.Transpose,
		ui.WithBindingEnabled(b.keyMap.CopySelection, !b.input.selection.Collapsed()),
		b.keyMap.Home,
		b.keyMap.End,
	}

	commandMode := strings.HasPrefix(b.input.Value(), "/")
	if commandMode {
		return bindings
	}

	attrs := b.input.activeAttrs()
	bindings = append(bindings,
		b.fmtBinding(b.keyMap.ToggleBold, attrs.Bold),
		b.fmtBinding(b.keyMap.ToggleItalic, attrs.Italic),
		b.fmtBinding(b.keyMap.ToggleUnderline, attrs.Underline),
		b.fmtBinding(b.keyMap.ToggleReverse, attrs.Reverse),
		b.fmtBinding(b.keyMap.ToggleStrike, attrs.Strike),
		b.fmtBinding(b.keyMap.OpenPalette, attrs.FG != nil || attrs.BG != nil),
		b.fmtBinding(b.keyMap.ResetFormat, attrs != (richtext.Attrs{})),
	)

	return bindings
}

func (b InputBar) fmtBinding(binding ui.KeyBinding, active bool) ui.KeyBinding {
	return ui.WithBindingActive(binding, active)
}

func (b InputBar) refreshPopover(msg tea.Msg) InputBar {
	updated, _ := b.popover.Update(msg)
	b.popover = updated.(Popover)

	return b
}

func (b InputBar) inputRect() ui.Rect {
	return ui.Rect{
		X:      b.bounds.X,
		Y:      b.bounds.Y + b.bounds.Height - 1,
		Width:  b.bounds.Width,
		Height: 1,
	}
}

func clampInputIndex(index, length int) int {
	if index < 0 {
		return 0
	}

	if index > length {
		return length
	}

	return index
}

func promptWidth() int {
	return lipgloss.Width(theme.Prompt.Render("> "))
}

func (b InputBar) prefixWidth() int {
	return lipgloss.Width(theme.UserNick.Render(string(b.userNick))) + 1 + promptWidth()
}

// View implements ui.Model. It renders the popover (if visible)
// above the input line.
func (b InputBar) View(width, _ int) string {
	nickLabel := theme.UserNick.Render(string(b.userNick)) + " "
	prompt := theme.Prompt.Render("> ")

	var lockBadge string
	editor := b.input
	if b.locked {
		lockBadge = theme.Dim.Render("(locked) ")
		nickLabel = theme.Dim.Render(string(b.userNick)) + " "
		prompt = theme.Dim.Render("> ")
		editor.cursor.Blur()
	}

	editorWidth := max(width-lipgloss.Width(nickLabel)-lipgloss.Width(lockBadge)-lipgloss.Width(prompt), 0)
	inputLine := nickLabel + lockBadge + prompt + editor.View(editorWidth, 1)

	popoverView := b.popover.Render(width)
	if popoverView == "" {
		return inputLine
	}

	return lipgloss.JoinVertical(lipgloss.Left, popoverView, inputLine)
}

// ActiveFormats returns the formatting state at the current cursor
// position. In command mode all formats are reported as inactive.
func (b InputBar) ActiveFormats() ActiveFormats {
	if strings.HasPrefix(b.input.Value(), "/") {
		return ActiveFormats{}
	}

	attrs := b.input.activeAttrs()

	return ActiveFormats{
		Bold:      attrs.Bold,
		Italic:    attrs.Italic,
		Underline: attrs.Underline,
		Reverse:   attrs.Reverse,
		Strike:    attrs.Strike,
	}
}

// PaletteVisible reports whether the colour palette is open.
func (b InputBar) PaletteVisible() bool {
	return b.input.PaletteVisible()
}

// PaletteTarget reports which colour slot the palette is editing.
// The result is meaningful only when PaletteVisible reports true.
func (b InputBar) PaletteTarget() PaletteTarget {
	return b.input.PaletteTarget()
}

// PaletteIndex returns the active swatch index within the palette.
func (b InputBar) PaletteIndex() int {
	return b.input.PaletteIndex()
}

// PaletteView renders the colour palette picker.
func (b InputBar) PaletteView(width int) string {
	return b.input.PaletteView(width)
}

// PaletteHeight returns the rendered height of the palette, or 0.
func (b InputBar) PaletteHeight(width int) int {
	view := b.PaletteView(width)
	if view == "" {
		return 0
	}

	return lipgloss.Height(view)
}

// HandlePaletteMouse forwards a mouse event to the palette.
func (b InputBar) HandlePaletteMouse(msg tea.MouseMsg) (InputBar, bool, tea.Cmd) {
	updated, handled := b.input.handlePaletteMouse(msg)
	if !handled {
		return b, false, nil
	}

	b.input = updated

	return b, true, nil
}

func (b InputBar) rawValue() string {
	plain := b.input.Value()
	if strings.HasPrefix(plain, "/") {
		return plain
	}

	return ircfmt.Encode(b.input.Document())
}

func (b InputBar) setRawValue(raw string) InputBar {
	if strings.HasPrefix(raw, "/") {
		b.input = b.input.SetPlainText(raw)
		b.input = b.input.SetAllowFormatting(false)
		b.input = b.input.SetCursorFromRuneIndex(len([]rune(b.input.Value())))
		return b
	}

	b.input = b.input.SetDocument(ircfmt.Parse(raw))
	b.input = b.input.SetAllowFormatting(true)
	b.input = b.input.SetCursorFromRuneIndex(len([]rune(b.input.Value())))

	return b
}
