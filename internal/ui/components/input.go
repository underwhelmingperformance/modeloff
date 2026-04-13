package components

import (
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

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

// InputBar wraps bubbles/textinput with command completion, nick
// completion, and input history. It owns the command popover as a
// child model.
type InputBar struct {
	input    textinput.Model
	keyMap   InputBarKeyMap
	userNick domain.Nick
	popover  Popover
	bounds   ui.Rect

	history   []string
	histPos   int // -1 = editing new input, 0..len(history)-1 = browsing
	histDraft string

	nicks    []domain.Nick
	nickComp nickCompletion
}

// NewInputBar creates an input bar with the given user nick.
func NewInputBar(userNick domain.Nick) InputBar {
	ti := textinput.New()
	ti.Prompt = theme.Prompt.Render("> ")
	ti.Focus()

	// Ctrl+D and Ctrl+U are reserved for sidebar navigation (channel
	// up/down). Remove them from the textinput bindings so they don't
	// conflict. Delete key still works for forward-delete.
	ti.KeyMap.DeleteCharacterForward = key.NewBinding(key.WithKeys("delete"))
	ti.KeyMap.DeleteBeforeCursor = key.NewBinding(key.WithDisabled())

	return InputBar{
		input:    ti,
		keyMap:   DefaultInputBarKeyMap,
		userNick: userNick,
		popover:  NewPopover(),
		histPos:  -1,
	}
}

// Init implements ui.Model.
func (b InputBar) Init() tea.Cmd {
	return textinput.Blink
}

// Update implements ui.Model.
func (b InputBar) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case UserNickMsg:
		b.userNick = msg.Nick
		return b, nil

	case NickListUpdatedMsg:
		b.nicks = slices.Collect(msg.Members.Nicks())
		return b, nil

	case CommandStateMsg:
		b = b.refreshPopover(PopoverApplyMsg{
			Completer: msg.Completer,
			Raw:       b.input.Value(),
			Cursor:    b.input.Position(),
		})
		return b, nil

	case PopoverAcceptMsg:
		b = b.replaceRange(msg.ReplaceStart, msg.ReplaceEnd, msg.Replacement)
		b = b.refreshPopover(PopoverRefreshMsg{
			Raw:    b.input.Value(),
			Cursor: b.input.Position(),
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
		return b.handleKey(msg)
	}

	var cmd tea.Cmd
	b.input, cmd = b.input.Update(msg)

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

	switch {
	case key.Matches(msg, b.keyMap.Submit):
		return b.submit()

	case key.Matches(msg, b.keyMap.HistoryUp):
		if !b.popover.IsVisible() {
			b = b.historyUp()
			b = b.refreshPopover(PopoverRefreshMsg{
				Raw:    b.input.Value(),
				Cursor: b.input.Position(),
			})
			return b, nil
		}

	case key.Matches(msg, b.keyMap.HistoryDn):
		if !b.popover.IsVisible() {
			b = b.historyDown()
			b = b.refreshPopover(PopoverRefreshMsg{
				Raw:    b.input.Value(),
				Cursor: b.input.Position(),
			})
			return b, nil
		}

	case msg.Type == tea.KeyTab:
		if !strings.HasPrefix(b.input.Value(), "/") {
			return b.completeNick(), nil
		}
	}

	b.nickComp.active = false

	var cmd tea.Cmd
	b.input, cmd = b.input.Update(msg)

	b = b.refreshPopover(PopoverRefreshMsg{
		Raw:    b.input.Value(),
		Cursor: b.input.Position(),
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
				b = b.setCursorFromCell(localX)
				b = b.refreshPopover(PopoverRefreshMsg{
					Raw:    b.input.Value(),
					Cursor: b.input.Position(),
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

	b = b.pushHistory(text)
	b.input.Reset()
	b.histPos = -1
	b.histDraft = ""

	b = b.refreshPopover(PopoverRefreshMsg{
		Raw:    b.input.Value(),
		Cursor: b.input.Position(),
	})

	if strings.HasPrefix(text, "/") {
		return b, func() tea.Msg {
			return CommandSubmitMsg{Raw: text}
		}
	}

	return b, func() tea.Msg {
		return MessageSubmitMsg{Text: text}
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
		b.histDraft = b.input.Value()
		b.histPos = len(b.history) - 1
	} else if b.histPos > 0 {
		b.histPos--
	} else {
		return b
	}

	b.input.SetValue(b.history[b.histPos])
	b.input.CursorEnd()

	return b
}

func (b InputBar) historyDown() InputBar {
	if b.histPos == -1 {
		return b
	}

	if b.histPos < len(b.history)-1 {
		b.histPos++
		b.input.SetValue(b.history[b.histPos])
	} else {
		b.histPos = -1
		b.input.SetValue(b.histDraft)
		b.histDraft = ""
	}

	b.input.CursorEnd()

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
	cursor := b.input.Position()

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

	b.input.SetValue(
		string(value[:b.nickComp.start]) +
			replacement +
			string(value[b.nickComp.end:]),
	)

	newEnd := b.nickComp.start + len([]rune(replacement))
	b.input.SetCursor(newEnd)
	b.nickComp.end = newEnd

	return b
}

func (b InputBar) replaceRange(start, end int, replacement string) InputBar {
	value := []rune(b.input.Value())
	start = clampInputIndex(start, len(value))
	end = clampInputIndex(end, len(value))
	end = max(end, start)

	runes := []rune(replacement)
	next := make([]rune, 0, start+len(runes)+len(value)-end)
	next = append(next, value[:start]...)
	next = append(next, runes...)
	next = append(next, value[end:]...)

	b.input.SetValue(string(next))
	b.input.SetCursor(start + len(runes))

	return b
}

func (b InputBar) setCursorFromCell(x int) InputBar {
	x -= b.prefixWidth()

	if x <= 0 {
		b.input.SetCursor(0)
		return b
	}

	value := []rune(b.input.Value())
	width := 0
	for i := range len(value) {
		nextWidth := width + lipgloss.Width(string(value[i]))
		if x <= nextWidth {
			if x-width <= nextWidth-x {
				b.input.SetCursor(i)
				return b
			}

			b.input.SetCursor(i + 1)
			return b
		}

		width = nextWidth
	}

	b.input.SetCursor(len(value))
	return b
}

// KeyBindings implements ui.Keybinding.
func (b InputBar) KeyBindings() []key.Binding {
	bindings := []key.Binding{
		b.keyMap.Submit,
	}

	if b.popover.IsVisible() {
		bindings = append(bindings,
			ui.WithBindingEnabled(
				key.NewBinding(
					key.WithKeys("tab"),
					key.WithHelp("Tab", "accept"),
				),
				b.popover.HasSuggestions(),
			),
			ui.WithBindingEnabled(
				key.NewBinding(
					key.WithKeys("up", "down", "shift+tab"),
					key.WithHelp("↑↓", "navigate"),
				),
				b.popover.HasSuggestions(),
			),
			key.NewBinding(
				key.WithKeys("esc"),
				key.WithHelp("Esc", "dismiss"),
			),
		)
	} else {
		bindings = append(bindings,
			ui.WithBindingEnabled(
				key.NewBinding(
					key.WithKeys("up", "down"),
					key.WithHelp("↑↓", "history"),
				),
				len(b.history) > 0,
			),
		)
	}

	return bindings
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
	nickWidth := lipgloss.Width(nickLabel)

	// textinput renders: prompt + text/padding(Width cells) + cursor(1 cell).
	// The cursor sits outside the Width when the input is empty, so subtract
	// an extra cell to keep the total within the available space.
	b.input.Width = max(width-nickWidth-promptWidth()-1, 0)

	inputLine := nickLabel + b.input.View()

	popoverView := b.popover.Render(width)
	if popoverView == "" {
		return inputLine
	}

	return lipgloss.JoinVertical(lipgloss.Left, popoverView, inputLine)
}
