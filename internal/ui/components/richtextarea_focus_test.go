package components

import (
	"testing"

	"charm.land/bubbles/v2/cursor"
	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui"
)

// TestRichTextarea_cursor_follows_application_focus_not_terminal_focus
// pins which focus the cursor follows. Root clears the screen's focus
// while a modal is open. A terminal focus event arriving meanwhile
// says the operator came back to the window, which is a different
// thing: the modal is still there and the cursor behind it stays
// blurred.
//
// Each assertion reads whether the cursor schedules its next blink.
// `cursor.Model` answers `cursor.Blink()` with a command only while it
// is focused, and `IsBlinked` alone would not separate a blurred
// cursor from the dark half of a blinking one.
func TestRichTextarea_cursor_follows_application_focus_not_terminal_focus(t *testing.T) {
	send := func(r RichTextarea, msgs ...tea.Msg) RichTextarea {
		t.Helper()

		for _, msg := range msgs {
			updated, _ := r.Update(msg)
			r = updated.(RichTextarea)
		}

		return r
	}

	// blinking reports whether the cursor schedules its next blink,
	// which only a focused cursor does.
	blinking := func(r RichTextarea) bool {
		t.Helper()

		_, cmd := r.Update(cursor.Blink())

		return cmd != nil
	}

	editor := NewRichTextarea(RichTextareaConfig{SingleLine: true})
	editor = send(editor, ui.ScreenFocusMsg{Focused: true})
	require.True(t, blinking(editor), "a focused editor must blink its cursor")

	editor = send(editor, tea.BlurMsg{})
	require.True(t, blinking(editor),
		"the terminal lost focus and blurred a cursor while the screen still had focus")

	editor = send(editor, ui.ScreenFocusMsg{Focused: false})
	require.False(t, blinking(editor), "a modal took focus and the cursor kept blinking")

	editor = send(editor, tea.FocusMsg{})
	require.False(t, blinking(editor), "the terminal regaining focus revived the cursor behind a modal")

	editor = send(editor, ui.ScreenFocusMsg{Focused: true})
	require.True(t, blinking(editor), "the modal closed and the cursor stayed blurred")
}
