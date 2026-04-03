package components_test

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
)

func typeText(t *testing.T, m ui.Model, text string) ui.Model {
	t.Helper()

	for _, r := range text {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	return m
}

func enter(t *testing.T, m ui.Model) (ui.Model, tea.Cmd) {
	t.Helper()

	return m.Update(tea.KeyMsg{Type: tea.KeyEnter})
}

func TestInputBar_type_and_submit_message(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "hello world")
	m, cmd := enter(t, m)

	require.NotNil(t, cmd)

	msg := cmd()
	sub, ok := msg.(components.MessageSubmitMsg)
	require.True(t, ok, "expected MessageSubmitMsg, got %T", msg)
	require.Equal(t, "hello world", sub.Text)

	// Buffer should be cleared after submit.
	require.Equal(t, "", m.(components.InputBar).Value())
}

func TestInputBar_submit_command(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "/join ¢general")
	_, cmd := enter(t, m)

	require.NotNil(t, cmd)

	msg := cmd()
	sub, ok := msg.(components.CommandSubmitMsg)
	require.True(t, ok, "expected CommandSubmitMsg, got %T", msg)
	require.Equal(t, "join", sub.Name)
	require.Equal(t, "¢general", sub.Args)
}

func TestInputBar_submit_command_no_args(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "/list")
	_, cmd := enter(t, m)

	require.NotNil(t, cmd)

	msg := cmd()
	sub := msg.(components.CommandSubmitMsg)
	require.Equal(t, "list", sub.Name)
	require.Equal(t, "", sub.Args)
}

func TestInputBar_empty_submit_does_nothing(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	_, cmd := enter(t, m)
	require.Nil(t, cmd)

	// Whitespace-only should also be ignored.
	m = typeText(t, m, "   ")
	_, cmd = enter(t, m)
	require.Nil(t, cmd)
}

func TestInputBar_backspace(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "abc")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})

	require.Equal(t, "ab", m.(components.InputBar).Value())
}

func TestInputBar_cursor_movement(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "abcd")

	// Move left twice.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})

	// Type at cursor position.
	m = typeText(t, m, "X")

	require.Equal(t, "abXcd", m.(components.InputBar).Value())
}

func TestInputBar_home_end(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "hello")

	// Home, then type.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	m = typeText(t, m, "X")

	require.Equal(t, "Xhello", m.(components.InputBar).Value())

	// End, then type.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	m = typeText(t, m, "Y")

	require.Equal(t, "XhelloY", m.(components.InputBar).Value())
}

func TestInputBar_ctrl_u_kills_to_start(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "abcde")

	// Move left twice, then ctrl-u kills everything before cursor.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})

	require.Equal(t, "de", m.(components.InputBar).Value())
}

func TestInputBar_ctrl_k_kills_to_end(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "abcde")

	// Move to position 2, then ctrl-k kills to end.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlK})

	require.Equal(t, "ab", m.(components.InputBar).Value())
}

func TestInputBar_delete_key(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "abc")

	// Move to start, delete the first character.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDelete})

	require.Equal(t, "bc", m.(components.InputBar).Value())
}

func TestInputBar_View_contains_prompt(t *testing.T) {
	b := components.NewInputBar()
	v := b.View(40, 1)

	require.Contains(t, v, ">")
}

func TestInputBar_ignores_non_key_messages(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m, cmd := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	require.Nil(t, cmd)
	require.Equal(t, "", m.(components.InputBar).Value())
}
