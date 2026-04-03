package components_test

import (
	"fmt"
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

	m = typeText(t, m, "/join #general")
	_, cmd := enter(t, m)

	require.NotNil(t, cmd)

	msg := cmd()
	sub, ok := msg.(components.CommandSubmitMsg)
	require.True(t, ok, "expected CommandSubmitMsg, got %T", msg)
	require.Equal(t, "join", sub.Name)
	require.Equal(t, "#general", sub.Args)
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

func TestInputBar_history_up_down(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	// Submit three messages.
	m = typeText(t, m, "first")
	m, _ = enter(t, m)
	m = typeText(t, m, "second")
	m, _ = enter(t, m)
	m = typeText(t, m, "third")
	m, _ = enter(t, m)

	// Up once = most recent.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	require.Equal(t, "third", m.(components.InputBar).Value())

	// Up again = previous.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	require.Equal(t, "second", m.(components.InputBar).Value())

	// Down = back to most recent.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	require.Equal(t, "third", m.(components.InputBar).Value())

	// Down again = back to empty draft.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	require.Equal(t, "", m.(components.InputBar).Value())
}

func TestInputBar_history_preserves_draft(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "old message")
	m, _ = enter(t, m)

	// Start typing a new message.
	m = typeText(t, m, "draft")

	// Up enters history, saving the draft.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	require.Equal(t, "old message", m.(components.InputBar).Value())

	// Down restores the draft.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	require.Equal(t, "draft", m.(components.InputBar).Value())
}

func TestInputBar_history_no_duplicate_consecutive(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "same")
	m, _ = enter(t, m)
	m = typeText(t, m, "same")
	m, _ = enter(t, m)

	// Up once = "same", up again should stay (only one entry).
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	require.Equal(t, "same", m.(components.InputBar).Value())

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	require.Equal(t, "same", m.(components.InputBar).Value())
}

func TestInputBar_history_up_with_no_history(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	// Up with no history should do nothing.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	require.Equal(t, "", m.(components.InputBar).Value())
}

func TestInputBar_history_ring_buffer_overflow(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	// Submit 51 messages (one more than historySize of 50).
	for i := 0; i <= 50; i++ {
		m = typeText(t, m, fmt.Sprintf("msg %d", i))
		m, _ = enter(t, m)
	}

	// Up once = most recent (msg 50).
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	require.Equal(t, "msg 50", m.(components.InputBar).Value())

	// Navigate all the way up: 49 more presses to reach the oldest.
	for range 49 {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	}

	// Oldest should be msg 1, not msg 0 (evicted by overflow).
	require.Equal(t, "msg 1", m.(components.InputBar).Value())

	// One more up should stay at the oldest.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	require.Equal(t, "msg 1", m.(components.InputBar).Value())
}

func TestInputBar_ctrl_a_moves_to_start(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "hello")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	m = typeText(t, m, "X")

	require.Equal(t, "Xhello", m.(components.InputBar).Value())
}

func TestInputBar_ctrl_e_moves_to_end(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "hello")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	m = typeText(t, m, "X")

	require.Equal(t, "helloX", m.(components.InputBar).Value())
}

func TestInputBar_ctrl_w_deletes_word_backward(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "hello world")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlW})

	require.Equal(t, "hello ", m.(components.InputBar).Value())
}

func TestInputBar_editing_shortcuts_work_after_history_recall(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "first message")
	m, _ = enter(t, m)

	// Recall from history.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	require.Equal(t, "first message", m.(components.InputBar).Value())

	// Ctrl+A to start, type prefix.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	m = typeText(t, m, "re: ")

	require.Equal(t, "re: first message", m.(components.InputBar).Value())

	// Ctrl+W to delete last word.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlW})

	require.Equal(t, "re: first ", m.(components.InputBar).Value())
}

func TestInputBar_ctrl_u_then_history_up(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "old")
	m, _ = enter(t, m)

	// Type something, then Ctrl+U to clear it.
	m = typeText(t, m, "discard this")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	require.Equal(t, "", m.(components.InputBar).Value())

	// History up should still work.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	require.Equal(t, "old", m.(components.InputBar).Value())
}

func TestInputBar_ignores_non_key_messages(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m, cmd := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	require.Nil(t, cmd)
	require.Equal(t, "", m.(components.InputBar).Value())
}
