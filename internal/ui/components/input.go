package components

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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

// InputBar wraps bubbles/textinput with command detection and input
// history recall via Up/Down arrows.
type InputBar struct {
	input  textinput.Model
	keyMap InputBarKeyMap

	history   []string
	histPos   int // -1 = editing new input, 0..len(history)-1 = browsing
	histDraft string
}

// NewInputBar creates an empty input bar.
func NewInputBar() InputBar {
	ti := textinput.New()
	ti.Prompt = theme.Prompt.Render("> ")
	ti.Focus()

	// Ctrl+D and Ctrl+U are reserved for sidebar navigation (channel
	// up/down). Remove them from the textinput bindings so they don't
	// conflict. Delete key still works for forward-delete.
	ti.KeyMap.DeleteCharacterForward = key.NewBinding(key.WithKeys("delete"))
	ti.KeyMap.DeleteBeforeCursor = key.NewBinding(key.WithDisabled())

	return InputBar{
		input:   ti,
		keyMap:  DefaultInputBarKeyMap,
		histPos: -1,
	}
}

// Init implements ui.Model.
func (b InputBar) Init() tea.Cmd {
	return textinput.Blink
}

// Update implements ui.Model.
func (b InputBar) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		b.input, cmd = b.input.Update(msg)

		return b, cmd
	}

	switch {
	case key.Matches(km, b.keyMap.Submit):
		return b.submit()

	case key.Matches(km, b.keyMap.HistoryUp):
		return b.historyUp(), nil

	case key.Matches(km, b.keyMap.HistoryDn):
		return b.historyDown(), nil
	}

	var cmd tea.Cmd
	b.input, cmd = b.input.Update(msg)

	return b, cmd
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

// Value returns the current text in the input buffer.
func (b InputBar) Value() string {
	return b.input.Value()
}

// Cursor returns the cursor position in runes.
func (b InputBar) Cursor() int {
	return b.input.Position()
}

// ReplaceRange replaces the given rune range with the provided text.
func (b InputBar) ReplaceRange(start, end int, replacement string) InputBar {
	value := []rune(b.input.Value())
	start = clampInputIndex(start, len(value))
	end = clampInputIndex(end, len(value))
	if end < start {
		end = start
	}

	runes := []rune(replacement)
	next := make([]rune, 0, start+len(runes)+len(value)-end)
	next = append(next, value[:start]...)
	next = append(next, runes...)
	next = append(next, value[end:]...)

	b.input.SetValue(string(next))
	b.input.SetCursor(start + len(runes))

	return b
}

// SetCursorFromCell moves the cursor to the nearest cell within the input area.
func (b InputBar) SetCursorFromCell(x int) InputBar {
	if x <= 0 {
		b.input.SetCursor(0)
		return b
	}

	value := []rune(b.input.Value())
	width := 0
	for i := 0; i < len(value); i++ {
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
	return []key.Binding{
		b.keyMap.Submit,
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

// View implements ui.Model.
func (b InputBar) View(width, _ int) string {
	b.input.Width = width

	return b.input.View()
}
