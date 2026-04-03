package components

import (
	"strings"

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
	Name string
	Args string
}

// InputBar is a single-line text input with prompt styling and
// command detection.
type InputBar struct {
	buffer []rune
	cursor int
}

// NewInputBar creates an empty input bar.
func NewInputBar() InputBar {
	return InputBar{}
}

// Init implements ui.Model.
func (b InputBar) Init() tea.Cmd {
	return nil
}

// Update implements ui.Model.
func (b InputBar) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return b, nil
	}

	switch km.Type {
	case tea.KeyEnter:
		return b.submit()

	case tea.KeyBackspace:
		if b.cursor > 0 {
			b.buffer = append(b.buffer[:b.cursor-1], b.buffer[b.cursor:]...)
			b.cursor--
		}

	case tea.KeyDelete:
		if b.cursor < len(b.buffer) {
			b.buffer = append(b.buffer[:b.cursor], b.buffer[b.cursor+1:]...)
		}

	case tea.KeyLeft:
		if b.cursor > 0 {
			b.cursor--
		}

	case tea.KeyRight:
		if b.cursor < len(b.buffer) {
			b.cursor++
		}

	case tea.KeyHome, tea.KeyCtrlA:
		b.cursor = 0

	case tea.KeyEnd, tea.KeyCtrlE:
		b.cursor = len(b.buffer)

	case tea.KeyCtrlU:
		b.buffer = b.buffer[b.cursor:]
		b.cursor = 0

	case tea.KeyCtrlK:
		b.buffer = b.buffer[:b.cursor]

	case tea.KeyRunes:
		runes := km.Runes
		tail := make([]rune, len(b.buffer[b.cursor:]))
		copy(tail, b.buffer[b.cursor:])
		b.buffer = append(b.buffer[:b.cursor], append(runes, tail...)...)
		b.cursor += len(runes)
	}

	return b, nil
}

func (b InputBar) submit() (ui.Model, tea.Cmd) {
	text := strings.TrimSpace(string(b.buffer))
	if text == "" {
		return b, nil
	}

	b.buffer = nil
	b.cursor = 0

	if strings.HasPrefix(text, "/") {
		name, args := parseCommand(text)

		return b, func() tea.Msg {
			return CommandSubmitMsg{Name: name, Args: args}
		}
	}

	return b, func() tea.Msg {
		return MessageSubmitMsg{Text: text}
	}
}

func parseCommand(text string) (string, string) {
	text = text[1:]

	name, args, _ := strings.Cut(text, " ")

	return name, strings.TrimSpace(args)
}

// Value returns the current text in the input buffer.
func (b InputBar) Value() string {
	return string(b.buffer)
}

// View implements ui.Model.
func (b InputBar) View(width, _ int) string {
	prompt := theme.Prompt.Render("> ")
	promptWidth := lipgloss.Width(prompt)

	available := width - promptWidth
	if available <= 0 {
		return prompt
	}

	content := string(b.buffer)
	if b.cursor < len(b.buffer) {
		before := string(b.buffer[:b.cursor])
		cursorChar := string(b.buffer[b.cursor])
		after := string(b.buffer[b.cursor+1:])
		content = before + "\x1b[7m" + cursorChar + "\x1b[0m" + after
	} else {
		content += "\x1b[7m \x1b[0m"
	}

	return prompt + content
}
