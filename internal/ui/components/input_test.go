package components_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/clipboard"
	"github.com/laney/modeloff/internal/ui/components"
	"github.com/laney/modeloff/internal/ui/uitest"
)

func typeText(t *testing.T, m ui.Component, text string) ui.Component {
	t.Helper()

	for _, r := range text {
		m, _ = m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	return m
}

func enter(t *testing.T, m ui.Component) (ui.Component, tea.Cmd) {
	t.Helper()

	return m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
}

func viewText(m ui.Component) string {
	return uitest.LastNonEmptyLine(renderToBuffer(m, 80, 1))
}

func viewTokens(m ui.Component) []string {
	return strings.Fields(viewText(m))
}

func TestInputBar_type_and_submit_message(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "hello world")
	m, cmd := enter(t, m)

	require.NotNil(t, cmd)

	msg := cmd()
	require.Equal(t, components.MessageSubmitMsg{Text: "hello world"}, msg)

	require.Equal(t, "", inputValue(t, m))
}

func TestInputBar_submit_command(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "/join #general")
	_, cmd := enter(t, m)

	require.NotNil(t, cmd)

	msg := cmd()
	require.Equal(t, components.CommandSubmitMsg{Raw: "/join #general"}, msg)
}

func TestInputBar_submit_command_no_args(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "/list")
	_, cmd := enter(t, m)

	require.NotNil(t, cmd)

	msg := cmd()
	sub := msg.(components.CommandSubmitMsg)
	require.Equal(t, "/list", sub.Raw)
}

func TestInputBar_submit_rich_message_as_irc_formatting(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Component = b

	m, _ = m.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	m = typeText(t, m, "bold")
	_, cmd := enter(t, m)

	require.NotNil(t, cmd)
	require.Equal(t, components.MessageSubmitMsg{Text: "\x02bold\x0f"}, cmd())
}

func TestInputBar_space_key_inserts_space(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "hello")
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	m = typeText(t, m, "world")

	require.Equal(t, "hello world", inputValue(t, m))
}

func TestInputBar_empty_submit_does_nothing(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	_, cmd := enter(t, m)
	require.Nil(t, cmd)

	// Whitespace-only should also be ignored.
	m = typeText(t, m, "   ")
	_, cmd = enter(t, m)
	require.Nil(t, cmd)
}

func TestInputBar_backspace(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "abc")
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})

	require.Equal(t, "ab", inputValue(t, m))
}

func TestInputBar_cursor_movement(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "abcd")

	// Move left twice.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})

	// Type at cursor position.
	m = typeText(t, m, "X")

	require.Equal(t, "abXcd", inputValue(t, m))
}

func TestInputBar_home_end(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "hello")

	// Home, then type.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	m = typeText(t, m, "X")

	require.Equal(t, "Xhello", inputValue(t, m))

	// End, then type.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	m = typeText(t, m, "Y")

	require.Equal(t, "XhelloY", inputValue(t, m))
}

func TestInputBar_ctrl_u_kills_to_line_start(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "abcde")

	// Move to position 2, then ctrl-u kills back to the start.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m, _ = m.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})

	require.Equal(t, "cde", inputValue(t, m))
}

func TestInputBar_ctrl_u_feeds_the_kill_ring(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "abcde")
	m, _ = m.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	m, _ = m.Update(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})

	require.Equal(t, "abcde", inputValue(t, m))
}

func TestInputBar_ctrl_k_kills_to_end(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "abcde")

	// Move to position 2, then ctrl-k kills to end.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m, _ = m.Update(tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})

	require.Equal(t, "ab", inputValue(t, m))
}

func TestInputBar_delete_key(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "abc")

	// Move to start, delete the first character.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDelete})

	require.Equal(t, "bc", inputValue(t, m))
}

func TestInputBar_paste_with_newline_shows_flatten_hint(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m, _ = m.Update(tea.PasteMsg{Content: "line one\nline two"})

	require.Equal(t, "line one line two", inputValue(t, m))
	require.Contains(t, renderedLines(renderToBuffer(m, 60, 2)), "Pasted text flattened to one line")
}

func TestInputBar_paste_without_newline_shows_no_hint(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m, _ = m.Update(tea.PasteMsg{Content: "no newline here"})

	require.NotContains(t, renderedLines(renderToBuffer(m, 60, 2)), "Pasted text flattened to one line")
}

func TestInputBar_paste_flatten_hint_clears_on_next_key(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m, _ = m.Update(tea.PasteMsg{Content: "a\nb"})
	m = typeText(t, m, "x")

	require.NotContains(t, renderedLines(renderToBuffer(m, 60, 2)), "Pasted text flattened to one line")
}

// TestInputBar_history_excludes_config_api_key pins that the history
// exclusion comes from the chatcmd grammar's own secret:"" marker
// (via SecretCheckerMsg), not a hand-maintained list local to the
// input bar: it builds the real production parser, so a future
// grammar change that dropped the marker from APIKeyConfig would fail
// this test rather than only the command package's own unit test.
func TestInputBar_history_excludes_config_api_key(t *testing.T) {
	parser, err := chatcmd.NewParser()
	require.NoError(t, err)

	var m ui.Component = components.NewInputBar("")
	m, _ = m.Update(components.SecretCheckerMsg{Checker: parser})

	m = typeText(t, m, "/config api-key sk-super-secret")
	m, _ = enter(t, m)
	m = typeText(t, m, "hello")
	m, _ = enter(t, m)

	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, "hello", inputValue(t, m))

	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, "hello", inputValue(t, m),
		"the api-key line must be excluded from history, leaving only one entry to recall")
}

// TestInputBar_history_excludes_config_api_key_any_case pins that the
// exclusion follows Set.Find's ascii-fold command resolution: typing
// the command in any casing still keeps it out of history.
func TestInputBar_history_excludes_config_api_key_any_case(t *testing.T) {
	parser, err := chatcmd.NewParser()
	require.NoError(t, err)

	var m ui.Component = components.NewInputBar("")
	m, _ = m.Update(components.SecretCheckerMsg{Checker: parser})

	m = typeText(t, m, "/CONFIG API-KEY sk-super-secret")
	m, _ = enter(t, m)
	m = typeText(t, m, "hello")
	m, _ = enter(t, m)

	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, "hello", inputValue(t, m))

	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, "hello", inputValue(t, m),
		"the api-key line must be excluded from history however it was cased")
}

// TestInputBar_history_keeps_lines_before_the_checker_is_set pins the
// nil-checker default: before SecretCheckerMsg has arrived (Init's
// batch is still in flight, in production), pushHistory must not
// refuse every line — it has nothing to exclude yet, not everything.
func TestInputBar_history_keeps_lines_before_the_checker_is_set(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "/config api-key sk-super-secret")
	m, _ = enter(t, m)

	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, "/config api-key sk-super-secret", inputValue(t, m))
}

func TestInputBar_word_left_moves_by_word(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "one two three")
	m, _ = m.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModAlt})
	m = typeText(t, m, "X")

	require.Equal(t, "one two Xthree", inputValue(t, m), "alt+b must move the cursor back one word, not toggle bold")
}

func TestInputBar_View_contains_prompt(t *testing.T) {
	b := components.NewInputBar("")
	v := strings.Fields(renderedLines(renderToBuffer(b, 40, 1))[0])

	require.Equal(t, []string{">"}, v)
}

func TestInputBar_View_includes_user_nick_and_fits_width(t *testing.T) {
	b := components.NewInputBar("testuser")

	v := renderToBuffer(b, 20, 1)

	require.Equal(t, []string{"testuser", ">"}, strings.Fields(renderedLines(v)[0]))
	require.LessOrEqual(t, lipgloss.Width(v), 20)
}

func TestInputBar_set_cursor_from_cell(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Component = b

	m = typeText(t, m, "hello")
	b = m.(components.InputBar).SetCursorFromCell(4)
	require.Equal(t, 1, b.Cursor())
	m = b
	m = typeText(t, m, "X")

	require.Equal(t, "hXello", m.(components.InputBar).Value())
}

func TestInputBar_history_up_down(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	// Submit three messages.
	m = typeText(t, m, "first")
	m, _ = enter(t, m)
	m = typeText(t, m, "second")
	m, _ = enter(t, m)
	m = typeText(t, m, "third")
	m, _ = enter(t, m)

	// Up once = most recent. Verify the rendered view matches, so this
	// is a user-visible change and not just an internal-state one.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, "third", inputValue(t, m))
	require.Equal(t, []string{">", "third"}, viewTokens(m))

	// Up again = previous.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, "second", inputValue(t, m))
	require.Equal(t, []string{">", "second"}, viewTokens(m))

	// Down = back to most recent.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	require.Equal(t, "third", inputValue(t, m))
	require.Equal(t, []string{">", "third"}, viewTokens(m))

	// Down again = back to empty draft.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	require.Equal(t, "", inputValue(t, m))
	require.Equal(t, []string{">"}, viewTokens(m))
}

func TestInputBar_history_preserves_draft(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "old message")
	m, _ = enter(t, m)

	// Start typing a new message.
	m = typeText(t, m, "draft")

	// Up enters history, saving the draft.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, "old message", inputValue(t, m))
	require.Equal(t, []string{">", "old", "message"}, viewTokens(m))

	// Down restores the draft.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	require.Equal(t, "draft", inputValue(t, m))
	require.Equal(t, []string{">", "draft"}, viewTokens(m))
}

func TestInputBar_history_restores_draft_cursor(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "previous")
	m, _ = enter(t, m)

	m = typeText(t, m, "draft text")
	// Move cursor to position 5 ("draft|text").
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	for range 5 {
		m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	}
	require.Equal(t, 5, m.(components.InputBar).Cursor())

	// Recall, then return to the draft.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, "previous", inputValue(t, m))

	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	require.Equal(t, "draft text", inputValue(t, m))
	require.Equal(t, 5, m.(components.InputBar).Cursor(), "draft cursor must be restored to its original position")
}

func TestInputBar_history_no_duplicate_consecutive(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "same")
	m, _ = enter(t, m)
	m = typeText(t, m, "same")
	m, _ = enter(t, m)

	// Up once = "same", up again should stay (only one entry).
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, "same", inputValue(t, m))

	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, "same", inputValue(t, m))
}

func TestInputBar_history_up_with_no_history(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	// Up with no history should do nothing.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})

	require.Equal(t, "", inputValue(t, m))
}

func TestInputBar_history_ring_buffer_overflow(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	// Submit 51 messages (one more than historySize of 50).
	for i := 0; i <= 50; i++ {
		m = typeText(t, m, fmt.Sprintf("msg %d", i))
		m, _ = enter(t, m)
	}

	// Up once = most recent (msg 50).
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, "msg 50", inputValue(t, m))

	// Navigate all the way up: 49 more presses to reach the oldest.
	for range 49 {
		m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	}

	// Oldest should be msg 1, not msg 0 (evicted by overflow).
	require.Equal(t, "msg 1", inputValue(t, m))

	// One more up should stay at the oldest.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, "msg 1", inputValue(t, m))
}

func TestInputBar_ctrl_a_moves_to_start(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "hello")
	m, _ = m.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	m = typeText(t, m, "X")

	require.Equal(t, "Xhello", inputValue(t, m))
}

func TestInputBar_ctrl_e_moves_to_end(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "hello")
	m, _ = m.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	m, _ = m.Update(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	m = typeText(t, m, "X")

	require.Equal(t, "helloX", inputValue(t, m))
}

func TestInputBar_delete_word_backward(t *testing.T) {
	tests := []struct {
		name string
		key  tea.KeyPressMsg
	}{
		{name: "ctrl+w", key: tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl}},
		{name: "alt+backspace", key: tea.KeyPressMsg{Code: tea.KeyBackspace, Mod: tea.ModAlt}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m ui.Component = components.NewInputBar("")

			m = typeText(t, m, "hello world")
			m, _ = m.Update(tt.key)

			require.Equal(t, "hello ", inputValue(t, m))
		})
	}
}

func TestInputBar_editing_shortcuts_work_after_history_recall(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "first message")
	m, _ = enter(t, m)

	// Recall from history.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, "first message", inputValue(t, m))

	// Ctrl+A to start, type prefix.
	m, _ = m.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	m = typeText(t, m, "re: ")

	require.Equal(t, "re: first message", inputValue(t, m))

	// Ctrl+W to delete last word.
	m, _ = m.Update(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	m, _ = m.Update(tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})

	require.Equal(t, "re: first ", inputValue(t, m))
}

func TestInputBar_ctrl_d_deletes_char_forward(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "abcde")

	// Move to start, then ctrl-d deletes the character under the
	// cursor, like the Delete key.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	m, _ = m.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})

	require.Equal(t, "bcde", inputValue(t, m))
}

func TestInputBar_ctrl_d_at_end_is_a_no_op(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "abc")
	m, _ = m.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})

	require.Equal(t, "abc", inputValue(t, m))
}

func TestInputBar_history_preserves_rich_formatting(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Component = b

	m, _ = m.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	m = typeText(t, m, "bold")
	m, _ = enter(t, m)

	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	_, cmd := enter(t, m)

	require.NotNil(t, cmd)
	require.Equal(t, components.MessageSubmitMsg{Text: "\x02bold\x0f"}, cmd())
}

func TestInputBar_command_mode_disables_formatting_shortcuts(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Component = b

	m = typeText(t, m, "/join ")
	m, _ = m.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	m = typeText(t, m, "#general")
	_, cmd := enter(t, m)

	require.NotNil(t, cmd)
	require.Equal(t, components.CommandSubmitMsg{Raw: "/join #general"}, cmd())
}

func TestInputBar_palette_right_arrow_navigates_swatches(t *testing.T) {
	var m ui.Component = components.NewInputBar()

	m, _ = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModAlt})
	require.True(t, m.(components.InputBar).PaletteVisible())
	require.Equal(t, 0, m.(components.InputBar).PaletteIndex())

	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})

	require.Equal(t, 2, m.(components.InputBar).PaletteIndex())
}

func TestInputBar_palette_tab_switches_target(t *testing.T) {
	var m ui.Component = components.NewInputBar()

	m, _ = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModAlt})
	require.Equal(t, components.PaletteTargetForeground, m.(components.InputBar).PaletteTarget())

	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})

	require.Equal(t, components.PaletteTargetBackground, m.(components.InputBar).PaletteTarget())
}

func TestInputBar_palette_enter_applies_and_dismisses(t *testing.T) {
	var m ui.Component = components.NewInputBar()

	m, _ = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModAlt})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	require.False(t, m.(components.InputBar).PaletteVisible())

	m = typeText(t, m, "x")
	_, cmd := enter(t, m)

	require.NotNil(t, cmd)
	require.Equal(t, components.MessageSubmitMsg{Text: "\x0300x\x0f"}, cmd())
}

func TestInputBar_palette_up_does_not_walk_history(t *testing.T) {
	var m ui.Component = components.NewInputBar()

	m = typeText(t, m, "earlier")
	m, _ = enter(t, m)
	require.Equal(t, "", inputValue(t, m))

	m, _ = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModAlt})
	require.True(t, m.(components.InputBar).PaletteVisible())

	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})

	require.Equal(t, "", inputValue(t, m))
	require.True(t, m.(components.InputBar).PaletteVisible(),
		"Up inside the palette must not dismiss it; if it did, the empty-value check above would pass for the wrong reason")
}

func TestInputBar_alt_w_copies_selection_via_osc52(t *testing.T) {
	var buf bytes.Buffer
	restore := clipboard.SetWriter(&buf)
	t.Cleanup(restore)

	var m ui.Component = components.NewInputBar()
	m = typeText(t, m, "hello world")

	// Select "hello" with shift+home from position 5.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModShift})

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'w', Mod: tea.ModAlt})

	require.NotNil(t, cmd, "alt+w with a selection must emit a clipboard cmd")
	cmd()

	want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("hello")) + "\x07"
	require.Equal(t, want, buf.String())
}

func TestInputBar_alt_w_with_no_selection_is_noop(t *testing.T) {
	var buf bytes.Buffer
	restore := clipboard.SetWriter(&buf)
	t.Cleanup(restore)

	var m ui.Component = components.NewInputBar()
	m = typeText(t, m, "hello")

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'w', Mod: tea.ModAlt})

	require.Nil(t, cmd, "alt+w without a selection must not emit a cmd")
	require.Empty(t, buf.String())
}

func TestInputBar_mouse_drag_selects_with_absolute_bounds(t *testing.T) {
	var buf bytes.Buffer
	restore := clipboard.SetWriter(&buf)
	t.Cleanup(restore)

	var m ui.Component = components.NewInputBar()
	m = typeText(t, m, "hello world")
	m, _ = m.Update(ui.BoundsMsg{Rect: uv.Rect(10, 5, 30, 1)})

	m, _ = m.Update(tea.MouseClickMsg{X: 14, Y: 5, Button: tea.MouseLeft})
	m, _ = m.Update(tea.MouseMotionMsg{X: 18, Y: 5, Button: tea.MouseLeft})
	m, _ = m.Update(tea.MouseReleaseMsg{X: 18, Y: 5, Button: tea.MouseLeft})

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'w', Mod: tea.ModAlt})
	require.NotNil(t, cmd)
	cmd()

	want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("ello")) + "\x07"
	require.Equal(t, want, buf.String())
}

func TestInputBar_clicking_editor_while_palette_open_moves_cursor(t *testing.T) {
	var m ui.Component = components.NewInputBar()
	m, _ = m.Update(ui.BoundsMsg{Rect: uv.Rect(10, 5, 30, 2)})
	m = typeText(t, m, "hello")
	m, _ = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModAlt})

	m, _ = m.Update(tea.MouseClickMsg{X: 14, Y: 6, Button: tea.MouseLeft})
	require.True(t, m.(components.InputBar).PaletteVisible())
	m = typeText(t, m, "X")

	require.Equal(t, "hXello", inputValue(t, m))
}

func TestInputBar_locked_view_shows_indicator(t *testing.T) {
	var m ui.Component = components.NewInputBar("user")
	m = typeText(t, m, "draft")

	m, _ = m.Update(components.InputLockedMsg{Locked: true})

	require.Equal(t, []string{"user (locked) > draft"}, renderedLines(renderToBuffer(m, 40, 1)))
}

func TestInputBar_unlocked_view_does_not_show_indicator(t *testing.T) {
	b := components.NewInputBar("user")

	require.Equal(t, []string{"user >"}, renderedLines(renderToBuffer(b, 40, 1)))
}

func TestInputBar_keybindings_include_palette_when_visible(t *testing.T) {
	var m ui.Component = components.NewInputBar()

	m, _ = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModAlt})
	require.True(t, m.(components.InputBar).PaletteVisible())

	bindings := m.(components.InputBar).KeyBindings()

	var helpTexts []string
	for _, b := range bindings {
		helpTexts = append(helpTexts, b.Help().Desc)
	}

	require.Equal(t, []string{"prev swatch", "next swatch", "jump", "fg/bg", "apply", "dismiss"}, helpTexts)
}

func TestInputBar_palette_enter_does_not_submit(t *testing.T) {
	var m ui.Component = components.NewInputBar()

	m = typeText(t, m, "draft")

	m, _ = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModAlt})

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	require.Nil(t, cmd, "Enter inside palette must not produce a submit cmd")
}

func TestInputBar_keybindings_include_rich_shortcuts(t *testing.T) {
	b := components.NewInputBar()

	bindings := keyMapByHelp(b.KeyBindings())
	require.Equal(t, map[string]struct{}{
		"↵\x00send":            {},
		"↑\x00history":         {},
		"↓\x00history":         {},
		"^B\x00bold":           {},
		"M-i\x00italic":        {},
		"M-u\x00underline":     {},
		"M-r\x00reverse":       {},
		"M-s\x00strike":        {},
		"M-c\x00colour":        {},
		"M-o\x00reset fmt":     {},
		"M-d\x00del next word": {},
		"^←\x00word ←":         {},
		"^→\x00word →":         {},
		"^W\x00del word":       {},
		"^K\x00del → end":      {},
		"^U\x00del → start":    {},
		"^D\x00del char":       {},
		"^Y\x00yank":           {},
		"^T\x00transpose":      {},
		"M-w\x00copy sel":      {},
		"Home\x00line start":   {},
		"End\x00line end":      {},
		"←\x00left":            {},
		"→\x00right":           {},
		"⌫\x00delete back":     {},
		"Del\x00delete":        {},
	}, bindings)

	b = typeText(t, b, "/join").(components.InputBar)
	bindings = keyMapByHelp(b.KeyBindings())
	require.Equal(t, map[string]struct{}{
		"↵\x00send":            {},
		"↑\x00history":         {},
		"↓\x00history":         {},
		"M-d\x00del next word": {},
		"^←\x00word ←":         {},
		"^→\x00word →":         {},
		"^W\x00del word":       {},
		"^K\x00del → end":      {},
		"^U\x00del → start":    {},
		"^D\x00del char":       {},
		"^Y\x00yank":           {},
		"^T\x00transpose":      {},
		"M-w\x00copy sel":      {},
		"Home\x00line start":   {},
		"End\x00line end":      {},
		"←\x00left":            {},
		"→\x00right":           {},
		"⌫\x00delete back":     {},
		"Del\x00delete":        {},
	}, bindings)
}

func keyMapByHelp(bindings []ui.KeyBinding) map[string]struct{} {
	index := map[string]struct{}{}
	for _, binding := range bindings {
		help := binding.Help()
		index[help.Key+"\x00"+help.Desc] = struct{}{}
	}

	return index
}

func TestInputBar_ignores_non_key_messages(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m, cmd := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	require.Nil(t, cmd)

	require.Equal(t, []string{">"}, viewTokens(m))
}

func inputBarWithNicks(nicks []domain.Nick) ui.Component {
	members := domain.NewMemberList()

	for _, n := range nicks {
		members.Add(domain.NewModelInstance(domain.InstanceID("inst-"+string(n)), n, "", "", nil))
	}

	var m ui.Component = components.NewInputBar("")
	m, _ = m.Update(components.NickListUpdatedMsg{Members: members})

	return m
}

func tabKey() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: tea.KeyTab}
}

func shiftTabKey() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
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
			m := inputBarWithNicks(nicks)

			m = typeText(t, m, tt.input)

			for range tt.tabs {
				m, _ = m.Update(tabKey())
			}

			require.Equal(t, tt.wantText, inputValue(t, m))
		})
	}
}

func TestInputBar_nick_completion_cycles(t *testing.T) {
	nicks := []domain.Nick{"alice", "alex", "bob"}
	m := inputBarWithNicks(nicks)

	m = typeText(t, m, "al")

	// First Tab: alex.
	m, _ = m.Update(tabKey())
	require.Equal(t, "alex: ", inputValue(t, m))

	// Second Tab: alice.
	m, _ = m.Update(tabKey())
	require.Equal(t, "alice: ", inputValue(t, m))

	// Third Tab: wraps back to alex.
	m, _ = m.Update(tabKey())
	require.Equal(t, "alex: ", inputValue(t, m))
}

func TestInputBar_nick_completion_reverse_cycles_with_shift_tab(t *testing.T) {
	nicks := []domain.Nick{"alice", "alex", "bob"}
	m := inputBarWithNicks(nicks)

	m = typeText(t, m, "al")

	// Shift+Tab starts from the last match, alphabetically: alice.
	m, _ = m.Update(shiftTabKey())
	require.Equal(t, "alice: ", inputValue(t, m))

	// Shift+Tab again steps backward, wrapping to the last match: alex.
	m, _ = m.Update(shiftTabKey())
	require.Equal(t, "alex: ", inputValue(t, m))
}

func TestInputBar_nick_completion_shift_tab_then_tab_reverses_direction(t *testing.T) {
	nicks := []domain.Nick{"alice", "alex", "bob"}
	m := inputBarWithNicks(nicks)

	m = typeText(t, m, "al")

	m, _ = m.Update(tabKey())
	require.Equal(t, "alex: ", inputValue(t, m))

	// Shift+Tab steps back to the previous match.
	m, _ = m.Update(shiftTabKey())
	require.Equal(t, "alice: ", inputValue(t, m))
}

func TestInputBar_nick_completion_resets_on_other_key(t *testing.T) {
	nicks := []domain.Nick{"alice", "alex"}
	m := inputBarWithNicks(nicks)

	m = typeText(t, m, "al")

	m, _ = m.Update(tabKey())
	require.Equal(t, "alex: ", inputValue(t, m))

	// Typing resets completion state.
	m = typeText(t, m, "h")
	require.Equal(t, "alex: h", inputValue(t, m))

	// Tab again starts a new completion on "h" (no match).
	m, _ = m.Update(tabKey())
	require.Equal(t, "alex: h", inputValue(t, m))
}

func TestInputBar_nick_completion_skipped_in_command_mode(t *testing.T) {
	nicks := []domain.Nick{"alice"}
	m := inputBarWithNicks(nicks)

	m = typeText(t, m, "/join al")

	m, _ = m.Update(tabKey())

	// Tab in command mode should not perform nick completion.
	require.Equal(t, "/join al", inputValue(t, m))
}

func TestInputBar_nick_completion_no_nicks(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m = typeText(t, m, "al")
	m, _ = m.Update(tabKey())

	require.Equal(t, "al", inputValue(t, m))
}

func TestInputBar_nick_completion_mid_line_with_trailing_text(t *testing.T) {
	nicks := []domain.Nick{"alice"}
	m := inputBarWithNicks(nicks)

	m = typeText(t, m, "hey al please")

	// Move cursor to just after "al" (position 6).
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	for range 6 {
		m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	}

	m, _ = m.Update(tabKey())

	require.Equal(t, "hey alice please", inputValue(t, m))
}

func TestInputBar_UserNickMsg_updates_nick_in_view(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	m, _ = m.Update(components.UserNickMsg{Nick: "oldnick"})
	require.Equal(t, []string{"oldnick", ">"}, viewTokens(m))

	m, _ = m.Update(components.UserNickMsg{Nick: "newnick"})

	require.Equal(t, []string{"newnick", ">"}, viewTokens(m))
}

func TestInputBar_NickListUpdatedMsg_enables_nick_completion(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	members := domain.NewMemberList()
	members.Add(domain.NewModelInstance("inst-alice", "alice", "", "", nil))
	members.Add(domain.NewModelInstance("inst-bob", "bob", "", "", nil))

	m, _ = m.Update(components.NickListUpdatedMsg{Members: members})
	m = typeText(t, m, "al")
	m, _ = m.Update(tabKey())

	require.Equal(t, "alice: ", inputValue(t, m))
}

func TestInputBar_NickListUpdatedMsg_ignores_an_older_revision(t *testing.T) {
	var m ui.Component = components.NewInputBar("")

	current := domain.NewMemberList()
	current.Add(domain.NewModelInstance("inst-alice", "alice", "", "", nil))
	stale := domain.NewMemberList()
	stale.Add(domain.NewModelInstance("inst-bob", "bob", "", "", nil))

	m, _ = m.Update(components.NickListUpdatedMsg{Members: current, Revision: 2})
	m, _ = m.Update(components.NickListUpdatedMsg{Members: stale, Revision: 1})
	m = typeText(t, m, "al")
	m, _ = m.Update(tabKey())

	require.Equal(t, "alice: ", inputValue(t, m))
}

// inputBarKind is a minimal KindProvider for InputBar popover tests.
type inputBarKind domain.ChannelKind

func (k inputBarKind) ChannelKind() domain.ChannelKind { return domain.ChannelKind(k) }

const inputBarKindChannel = inputBarKind(domain.KindChannel)

func inputBarWithPopover(nodes []*command.Node[inputBarKind]) ui.Component {
	var m ui.Component = components.NewInputBar("testuser")

	m, _ = m.Update(components.CompleterMsg{
		Completer: command.CompletionSet[inputBarKind]{Set: command.Set[inputBarKind]{Commands: nodes}, Ctx: inputBarKindChannel},
	})
	m, _ = m.Update(ui.BoundsMsg{Rect: uv.Rect(0, 0, 60, 24)})

	return m
}

func TestInputBar_popover_shows_completions(t *testing.T) {
	nodes := []*command.Node[inputBarKind]{
		{Name: "join", Help: "Join a channel"},
		{Name: "part", Help: "Leave a channel"},
	}
	m := inputBarWithPopover(nodes)

	m = typeText(t, m, "/")

	require.Equal(t, []string{
		"/join  Join a channel",
		"/part  Leave a channel",
		"testuser > /",
	}, visibleLines(renderToBuffer(m, 60, 3)))
}

func TestInputBar_popover_tab_accepts(t *testing.T) {
	nodes := []*command.Node[inputBarKind]{
		{Name: "join", Help: "Join a channel"},
	}
	m := inputBarWithPopover(nodes)

	m = typeText(t, m, "/jo")

	var cmd tea.Cmd
	m, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})

	require.NotNil(t, cmd, "Tab should produce a cmd")
	m, _ = m.Update(cmd())

	m = typeText(t, m, "#general")
	_, cmd = enter(t, m)

	require.NotNil(t, cmd)
	sub := cmd().(components.CommandSubmitMsg)
	require.Equal(t, "/join #general", sub.Raw)
}

func TestInputBar_enter_submits_with_optional_continuation_suggested(t *testing.T) {
	const modelID = "anthropic/claude-3-haiku"

	nodes := []*command.Node[inputBarKind]{
		{
			Name: "add-model",
			Positionals: []command.Positional[inputBarKind]{
				{
					Name: "model",
					Source: command.LiteralSource[inputBarKind](
						command.Suggestion{Value: modelID, Label: modelID},
					),
				},
			},
			Flags: []command.Flag[inputBarKind]{
				{Name: "--persona", Optional: true, Help: "Persona ID or literal text"},
			},
		},
	}
	m := inputBarWithPopover(nodes)
	m = typeText(t, m, "/add-model anth")

	m, cmd := enter(t, m)
	require.NotNil(t, cmd)
	m, _ = m.Update(cmd())

	require.Contains(t, visibleLines(renderToBuffer(m, 60, 2)), "--persona  Persona ID or literal text")

	_, cmd = enter(t, m)
	require.NotNil(t, cmd)
	require.Equal(t, components.CommandSubmitMsg{Raw: "/add-model " + modelID}, cmd())
}

func TestInputBar_enter_accepts_value_after_tab_accepts_optional_flag(t *testing.T) {
	const modelID = "anthropic/claude-3-haiku"

	nodes := []*command.Node[inputBarKind]{
		{
			Name:        "add-model",
			Positionals: []command.Positional[inputBarKind]{{Name: "model"}},
			Flags: []command.Flag[inputBarKind]{
				{
					Name:     "--persona",
					Optional: true,
					Variadic: true,
					Source: command.LiteralSource[inputBarKind](
						command.Suggestion{Value: "bard", Label: "bard"},
						command.Suggestion{Value: "sage", Label: "sage"},
					),
				},
			},
		},
	}
	m := inputBarWithPopover(nodes)
	m = typeText(t, m, "/add-model "+modelID+" ")

	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	require.NotNil(t, cmd)
	m, _ = m.Update(cmd())

	m, cmd = enter(t, m)
	require.NotNil(t, cmd)
	require.Equal(t, components.PopoverAcceptMsg{
		ReplaceStart: len("/add-model " + modelID + " --persona "),
		ReplaceEnd:   len("/add-model " + modelID + " --persona "),
		Replacement:  "bard ",
	}, cmd())

	m, _ = m.Update(cmd())
	_, cmd = enter(t, m)
	require.NotNil(t, cmd)
	require.Equal(t, components.CommandSubmitMsg{
		Raw: "/add-model " + modelID + " --persona bard",
	}, cmd())
}

func TestInputBar_popover_tab_preserves_typed_alias(t *testing.T) {
	tests := []struct {
		name    string
		typed   string
		wantRaw string
	}{
		{name: "alias-exact preserves typed alias", typed: "/j", wantRaw: "/j #general"},
		{name: "value-exact preserves typed canonical", typed: "/join", wantRaw: "/join #general"},
		{name: "ambiguous prefix expands to canonical", typed: "/jo", wantRaw: "/join #general"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodes := []*command.Node[inputBarKind]{
				{Name: "join", Help: "Join a channel", Aliases: []string{"j"}},
			}
			m := inputBarWithPopover(nodes)

			m = typeText(t, m, tt.typed)

			var cmd tea.Cmd
			m, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
			require.NotNil(t, cmd, "Tab should produce a cmd")
			m, _ = m.Update(cmd())

			m = typeText(t, m, "#general")
			_, cmd = enter(t, m)
			require.NotNil(t, cmd)

			sub := cmd().(components.CommandSubmitMsg)
			require.Equal(t, tt.wantRaw, sub.Raw)
		})
	}
}

func TestInputBar_popover_dismiss_on_esc(t *testing.T) {
	nodes := []*command.Node[inputBarKind]{
		{Name: "join", Help: "Join a channel"},
		{Name: "part", Help: "Leave a channel"},
	}
	m := inputBarWithPopover(nodes)

	m = typeText(t, m, "/")

	// Popover should be showing completions.
	require.Equal(t, []string{
		"/join  Join a channel",
		"/part  Leave a channel",
		"testuser > /",
	}, visibleLines(renderToBuffer(m, 60, 3)))

	// Esc should dismiss the popover.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})

	require.Equal(t, []string{"testuser > /"}, visibleLines(renderToBuffer(m, 60, 3)))
}

func TestInputBar_keybindings_include_popover_when_visible(t *testing.T) {
	nodes := []*command.Node[inputBarKind]{
		{Name: "join", Help: "Join a channel"},
	}
	m := inputBarWithPopover(nodes)

	m = typeText(t, m, "/")

	bar := m.(components.InputBar)
	bindings := bar.KeyBindings()

	var helpTexts []string
	for _, b := range bindings {
		helpTexts = append(helpTexts, b.Help().Desc)
	}

	require.Equal(t, []string{"send", "accept", "navigate", "dismiss"}, helpTexts)
}

func TestInputBar_keybindings_include_history_when_popover_hidden(t *testing.T) {
	var m ui.Component = components.NewInputBar("testuser")

	// Submit a message to populate history.
	m = typeText(t, m, "something")
	m, _ = enter(t, m)

	bar := m.(components.InputBar)
	bindings := bar.KeyBindings()

	var helpTexts []string
	for _, b := range bindings {
		helpTexts = append(helpTexts, b.Help().Desc)
	}

	require.Equal(t, []string{
		"send",
		"history",
		"history",
		"del \u2192 start",
		"del char",
		"copy sel",
		"left",
		"right",
		"word \u2190",
		"word \u2192",
		"line start",
		"line end",
		"delete back",
		"delete",
		"del word",
		"del next word",
		"del \u2192 end",
		"transpose",
		"yank",
		"bold",
		"italic",
		"underline",
		"reverse",
		"strike",
		"colour",
		"reset fmt",
	}, helpTexts)
}

func TestInputBar_view_does_not_show_plain_indicator(t *testing.T) {
	b := components.NewInputBar("user")

	require.Equal(t, []string{"user", ">"}, viewTokens(b))
}

func TestInputBar_active_formats(t *testing.T) {
	altToggle := func(r rune) tea.KeyPressMsg {
		return tea.KeyPressMsg{Code: r, Mod: tea.ModAlt}
	}

	tests := []struct {
		name      string
		hasToggle bool
		toggle    tea.KeyPressMsg
		want      components.ActiveFormats
	}{
		{
			name: "no formatting active by default",
			want: components.ActiveFormats{},
		},
		{
			name:      "bold active after toggle",
			hasToggle: true,
			toggle:    tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl},
			want:      components.ActiveFormats{Bold: true},
		},
		{
			name:      "italic active after toggle",
			hasToggle: true,
			toggle:    altToggle('i'),
			want:      components.ActiveFormats{Italic: true},
		},
		{
			name:      "underline active after toggle",
			hasToggle: true,
			toggle:    altToggle('u'),
			want:      components.ActiveFormats{Underline: true},
		},
		{
			name:      "reverse active after toggle",
			hasToggle: true,
			toggle:    altToggle('r'),
			want:      components.ActiveFormats{Reverse: true},
		},
		{
			name:      "strikethrough active after toggle",
			hasToggle: true,
			toggle:    altToggle('s'),
			want:      components.ActiveFormats{Strike: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m ui.Component = components.NewInputBar()

			if tt.hasToggle {
				m, _ = m.Update(tt.toggle)
			}

			bar := m.(components.InputBar)
			require.Equal(t, tt.want, bar.ActiveFormats())
		})
	}
}

func TestInputBar_status_bar_renders_active_format_bold(t *testing.T) {
	// Force colour output so ANSI escapes are emitted.
	var m ui.Component = components.NewInputBar("user")

	// Toggle bold formatting.
	m, _ = m.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})

	bar := m.(components.InputBar)
	bindings := bar.KeyBindings()

	screen := uv.NewScreenBuffer(300, 1)
	components.NewStatusBar(bindings, nil).Draw(screen, screen.Bounds())

	// The bold-format binding must contribute "^B" and
	// "bold" so the user sees that the active format is highlighted;
	// "M-o reset fmt" is also rendered bold because pressing it
	// would clear bold — the styling reflects current state.
	require.Equal(t, []string{"^B", "bold", "M-o", "reset fmt"}, boldCellSegments(screen))
}

func boldCellSegments(screen uv.Screen) []string {
	var segments []string
	var current strings.Builder

	for x := screen.Bounds().Min.X; x < screen.Bounds().Max.X; x++ {
		cell := screen.CellAt(x, 0)
		if cell.Style.Attrs&uv.AttrBold != 0 {
			current.WriteString(cell.Content)
			continue
		}

		if current.Len() > 0 {
			segments = append(segments, current.String())
			current.Reset()
		}
	}

	if current.Len() > 0 {
		segments = append(segments, current.String())
	}

	return segments
}
