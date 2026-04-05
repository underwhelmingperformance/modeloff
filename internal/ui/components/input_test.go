package components_test

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
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
	require.Equal(t, "/join #general", sub.Raw)
}

func TestInputBar_submit_command_no_args(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "/list")
	_, cmd := enter(t, m)

	require.NotNil(t, cmd)

	msg := cmd()
	sub := msg.(components.CommandSubmitMsg)
	require.Equal(t, "/list", sub.Raw)
}

func TestInputBar_space_key_inserts_space(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "hello")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	m = typeText(t, m, "world")

	require.Equal(t, "hello world", m.(components.InputBar).Value())
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

func TestInputBar_ctrl_u_does_not_kill_to_start(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "abcde")

	// Ctrl+U is reserved for sidebar navigation and should not
	// modify the input buffer.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})

	require.Equal(t, "abcde", m.(components.InputBar).Value())
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

func TestInputBar_ctrl_d_does_not_delete_forward(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "abcde")

	// Move to start, then Ctrl+D — reserved for sidebar navigation
	// and should not delete the character under the cursor.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})

	require.Equal(t, "abcde", m.(components.InputBar).Value())
}

func TestInputBar_ignores_non_key_messages(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m, cmd := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	require.Nil(t, cmd)
	require.Equal(t, "", m.(components.InputBar).Value())
}

func tabKey() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyTab}
}

func TestInputBar_nick_completion(t *testing.T) {
	nicks := []domain.Nick{"alice", "bob", "charlie"}

	tests := []struct {
		name     string
		input    string
		tabs     int
		wantText string
	}{
		{
			name:     "complete at start of line appends colon",
			input:    "al",
			tabs:     1,
			wantText: "alice: ",
		},
		{
			name:     "complete mid-line appends space",
			input:    "hey al",
			tabs:     1,
			wantText: "hey alice ",
		},
		{
			name:     "cycle through matches",
			input:    "b",
			tabs:     1,
			wantText: "bob: ",
		},
		{
			name:     "no match leaves input unchanged",
			input:    "z",
			tabs:     1,
			wantText: "z",
		},
		{
			name:     "empty prefix does nothing",
			input:    "",
			tabs:     1,
			wantText: "",
		},
		{
			name:     "case insensitive match",
			input:    "AL",
			tabs:     1,
			wantText: "alice: ",
		},
		{
			name:     "tab cycles through multiple matches",
			input:    "c",
			tabs:     1,
			wantText: "charlie: ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := components.NewInputBar().SetNicks(nicks)
			var m ui.Model = b

			m = typeText(t, m, tt.input)

			for range tt.tabs {
				m, _ = m.Update(tabKey())
			}

			require.Equal(t, tt.wantText, m.(components.InputBar).Value())
		})
	}
}

func TestInputBar_nick_completion_cycles(t *testing.T) {
	nicks := []domain.Nick{"alice", "alex", "bob"}
	b := components.NewInputBar().SetNicks(nicks)
	var m ui.Model = b

	m = typeText(t, m, "al")

	// First Tab: alice.
	m, _ = m.Update(tabKey())
	require.Equal(t, "alex: ", m.(components.InputBar).Value())

	// Second Tab: alex.
	m, _ = m.Update(tabKey())
	require.Equal(t, "alice: ", m.(components.InputBar).Value())

	// Third Tab: wraps back to alice.
	m, _ = m.Update(tabKey())
	require.Equal(t, "alex: ", m.(components.InputBar).Value())
}

func TestInputBar_nick_completion_resets_on_other_key(t *testing.T) {
	nicks := []domain.Nick{"alice", "alex"}
	b := components.NewInputBar().SetNicks(nicks)
	var m ui.Model = b

	m = typeText(t, m, "al")

	m, _ = m.Update(tabKey())
	require.Equal(t, "alex: ", m.(components.InputBar).Value())

	// Typing resets completion state.
	m = typeText(t, m, "h")
	require.Equal(t, "alex: h", m.(components.InputBar).Value())

	// Tab again starts a new completion on "h" (no match).
	m, _ = m.Update(tabKey())
	require.Equal(t, "alex: h", m.(components.InputBar).Value())
}

func TestInputBar_nick_completion_skipped_in_command_mode(t *testing.T) {
	nicks := []domain.Nick{"alice"}
	b := components.NewInputBar().SetNicks(nicks)
	var m ui.Model = b

	m = typeText(t, m, "/join al")

	m, _ = m.Update(tabKey())

	// Tab in command mode should not perform nick completion.
	// The textinput may or may not modify the value, but nick
	// completion should not have inserted "alice: ".
	require.NotContains(t, m.(components.InputBar).Value(), "alice: ")
}

func TestInputBar_nick_completion_no_nicks(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "al")
	m, _ = m.Update(tabKey())

	require.Equal(t, "al", m.(components.InputBar).Value())
}

func TestInputBar_nick_completion_mid_line_with_trailing_text(t *testing.T) {
	nicks := []domain.Nick{"alice"}
	b := components.NewInputBar().SetNicks(nicks)
	var m ui.Model = b

	m = typeText(t, m, "hey al please")

	// Move cursor to just after "al" (position 6).
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	for range 6 {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	}

	m, _ = m.Update(tabKey())

	require.Equal(t, "hey alice please", m.(components.InputBar).Value())
}
