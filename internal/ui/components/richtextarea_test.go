package components

import (
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/richtext"
	"github.com/laney/modeloff/internal/ui/uitest"
)

func colour(index uint8) *uint8 {
	value := index

	return &value
}

func TestRichTextareaCursorVisibleByDefault(t *testing.T) {
	editor := NewRichTextarea(RichTextareaConfig{SingleLine: true})
	require.False(t, editor.cursor.Blink, "cursor must not be in blink-off state")
	require.Equal(t, cursor.CursorBlink, editor.cursor.Mode(), "cursor must use blink mode")
}

func TestRichTextareaCursorAppearsInView(t *testing.T) {
	editor := NewRichTextarea(RichTextareaConfig{SingleLine: true})
	editor = editor.SetPlainText("hello")
	editor = editor.SetCursorFromRuneIndex(2)

	view := editor.View(20, 1)

	require.Equal(t, []string{"hello"}, uitest.NonEmptyLines(view))
}

func TestRichTextareaCtrlWordMovementUsesBoundaries(t *testing.T) {
	editor := NewRichTextarea(RichTextareaConfig{})
	editor = editor.SetPlainText("one two ثلاثة")

	updated, _ := editor.Update(tea.KeyMsg{Type: tea.KeyCtrlRight})
	editor = updated.(RichTextarea)
	require.Equal(t, 3, editor.Cursor())

	updated, _ = editor.Update(tea.KeyMsg{Type: tea.KeyCtrlRight})
	editor = updated.(RichTextarea)
	require.Equal(t, 7, editor.Cursor())

	updated, _ = editor.Update(tea.KeyMsg{Type: tea.KeyCtrlLeft})
	editor = updated.(RichTextarea)
	require.Equal(t, 4, editor.Cursor())
}

func TestRichTextareaLineMovementAndSelection(t *testing.T) {
	editor := NewRichTextarea(RichTextareaConfig{Wrap: true})
	editor = editor.SetPlainText("alpha\nbeta")
	editor.width = 10
	editor.height = 2

	updated, _ := editor.Update(tea.KeyMsg{Type: tea.KeyEnd})
	editor = updated.(RichTextarea)
	require.Equal(t, richtext.Position{Line: 0, Cluster: 5}, editor.position)

	updated, _ = editor.Update(tea.KeyMsg{Type: tea.KeyDown})
	editor = updated.(RichTextarea)
	require.Equal(t, richtext.Position{Line: 1, Cluster: 4}, editor.position)

	updated, _ = editor.Update(tea.KeyMsg{Type: tea.KeyHome})
	editor = updated.(RichTextarea)
	require.Equal(t, richtext.Position{Line: 1, Cluster: 0}, editor.position)

	updated, _ = editor.Update(tea.KeyMsg{Type: tea.KeyShiftRight})
	editor = updated.(RichTextarea)
	require.Equal(t, richtext.Selection{
		Anchor: richtext.Position{Line: 1, Cluster: 0},
		Head:   richtext.Position{Line: 1, Cluster: 1},
	}, editor.selection)
}

func TestRichTextareaMultilineViewportTracksCursor(t *testing.T) {
	editor := NewRichTextarea(RichTextareaConfig{Wrap: true})
	editor = editor.SetPlainText("alpha beta gamma delta epsilon zeta")

	for range 6 {
		updated, _ := editor.Update(tea.KeyMsg{Type: tea.KeyRight})
		editor = updated.(RichTextarea)
	}

	editor.View(8, 2)
	updated, _ := editor.Update(tea.KeyMsg{Type: tea.KeyDown})
	editor = updated.(RichTextarea)
	editor = editor.ensureViewport()

	require.GreaterOrEqual(t, editor.yOffset, 0)
	require.NotZero(t, editor.currentRowIndex(8))
}

func TestRichTextareaSingleLineViewportScrollsHorizontally(t *testing.T) {
	editor := NewRichTextarea(RichTextareaConfig{SingleLine: true})
	editor = editor.SetPlainText("abcdefghij")
	editor.width = 4
	editor.height = 1

	for range 10 {
		updated, _ := editor.Update(tea.KeyMsg{Type: tea.KeyRight})
		editor = updated.(RichTextarea)
	}

	editor = editor.ensureViewport()

	require.Equal(t, richtext.Position{Line: 0, Cluster: 10}, editor.position)
	require.Greater(t, editor.xOffset, 0)
}

func TestRichTextareaBackspaceDeleteAndSelectionDelete(t *testing.T) {
	editor := NewRichTextarea(RichTextareaConfig{})
	editor = editor.SetPlainText("abcdef")

	updated, _ := editor.Update(tea.KeyMsg{Type: tea.KeyRight})
	editor = updated.(RichTextarea)
	updated, _ = editor.Update(tea.KeyMsg{Type: tea.KeyRight})
	editor = updated.(RichTextarea)
	updated, _ = editor.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	editor = updated.(RichTextarea)
	require.Equal(t, "acdef", editor.Value())

	editor.selection = richtext.Selection{
		Anchor: richtext.Position{Line: 0, Cluster: 1},
		Head:   richtext.Position{Line: 0, Cluster: 3},
	}
	editor.position = editor.selection.Head

	updated, _ = editor.Update(tea.KeyMsg{Type: tea.KeyDelete})
	editor = updated.(RichTextarea)

	require.Equal(t, "aef", editor.Value())
	require.Equal(t, richtext.Selection{
		Anchor: richtext.Position{Line: 0, Cluster: 1},
		Head:   richtext.Position{Line: 0, Cluster: 1},
	}, editor.selection)
}

func TestRichTextareaDoubleClickSelectsWord(t *testing.T) {
	editor := NewRichTextarea(RichTextareaConfig{})
	editor = editor.SetPlainText("hello world")
	editor.width = 20
	editor.height = 1

	updated, _ := editor.Update(tea.MouseMsg{
		X:      7,
		Y:      0,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	editor = updated.(RichTextarea)
	editor.lastClickAt = time.Now()

	updated, _ = editor.Update(tea.MouseMsg{
		X:      7,
		Y:      0,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	editor = updated.(RichTextarea)

	start, end := editor.selection.Normalized()
	require.Equal(t, 6, start.Cluster)
	require.Equal(t, 11, end.Cluster)
}

func TestRichTextareaMouseDragSelectsRange(t *testing.T) {
	editor := NewRichTextarea(RichTextareaConfig{})
	editor = editor.SetPlainText("hello world")
	editor.width = 20
	editor.height = 1

	updated, _ := editor.Update(tea.MouseMsg{
		X:      1,
		Y:      0,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	editor = updated.(RichTextarea)

	updated, _ = editor.Update(tea.MouseMsg{
		X:      5,
		Y:      0,
		Action: tea.MouseActionMotion,
		Button: tea.MouseButtonLeft,
	})
	editor = updated.(RichTextarea)

	start, end := editor.selection.Normalized()

	require.Equal(t, richtext.Position{Line: 0, Cluster: 1}, start)
	require.Equal(t, richtext.Position{Line: 0, Cluster: 5}, end)
}

func TestRichTextareaPaletteMouseAppliesForeground(t *testing.T) {
	editor := NewRichTextarea(RichTextareaConfig{AllowFormatting: true})
	editor = editor.SetPlainText("hello")

	updated, _ := editor.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}, Alt: true})
	editor = updated.(RichTextarea)
	require.True(t, editor.PaletteVisible())

	updated, _ = editor.Update(tea.MouseMsg{
		X:      10,
		Y:      0,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	editor = updated.(RichTextarea)

	require.False(t, editor.PaletteVisible())
	require.NotNil(t, editor.pending.FG)
}

func TestRichTextareaPaletteKeyboardTargetsBackgroundForSelection(t *testing.T) {
	editor := NewRichTextarea(RichTextareaConfig{AllowFormatting: true})
	editor = editor.SetPlainText("hello")
	editor.selection = richtext.Selection{
		Anchor: richtext.Position{Line: 0, Cluster: 1},
		Head:   richtext.Position{Line: 0, Cluster: 4},
	}
	editor.position = editor.selection.Head

	updated, _ := editor.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}, Alt: true})
	editor = updated.(RichTextarea)
	var handled bool
	editor, handled = editor.handlePaletteKey(tea.KeyMsg{Type: tea.KeyTab})
	require.True(t, handled)
	editor, handled = editor.handlePaletteKey(tea.KeyMsg{Type: tea.KeyRight})
	require.True(t, handled)
	editor = editor.applyPaletteSelection()

	require.False(t, editor.PaletteVisible())
	require.Equal(t, []richtext.Span{
		{Text: "h", Attrs: richtext.Attrs{}},
		{Text: "ell", Attrs: richtext.Attrs{BG: colour(0)}},
		{Text: "o", Attrs: richtext.Attrs{}},
	}, editor.document.Line(0).Spans)
}

func TestRichTextareaAltDDeletesNextWord(t *testing.T) {
	editor := NewRichTextarea(RichTextareaConfig{})
	editor = editor.SetPlainText("one two three")

	updated, _ := editor.Update(tea.KeyMsg{Type: tea.KeyCtrlRight})
	editor = updated.(RichTextarea)

	updated, _ = editor.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}, Alt: true})
	editor = updated.(RichTextarea)

	require.Equal(t, "one three", editor.Value())
}

func TestRichTextareaSingleLinePasteFlattensNewlines(t *testing.T) {
	tests := []struct {
		name  string
		paste string
		want  string
	}{
		{name: "lf between words", paste: "abc\ndef", want: "abc def"},
		{name: "crlf between words", paste: "abc\r\ndef", want: "abc def"},
		{name: "leading lf", paste: "\nabc", want: " abc"},
		{name: "trailing lf", paste: "abc\n", want: "abc "},
		{name: "multiple lf", paste: "a\n\nb", want: "a  b"},
		{name: "no break", paste: "abc", want: "abc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			editor := NewRichTextarea(RichTextareaConfig{SingleLine: true})

			updated, _ := editor.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tt.paste)})
			editor = updated.(RichTextarea)

			require.Equal(t, tt.want, editor.Value())
			require.Equal(t, 1, editor.document.LineCount())
		})
	}
}

func TestRichTextareaMultilinePastePreservesNewlines(t *testing.T) {
	editor := NewRichTextarea(RichTextareaConfig{Wrap: true})

	updated, _ := editor.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("abc\ndef")})
	editor = updated.(RichTextarea)

	require.Equal(t, "abc\ndef", editor.Value())
	require.Equal(t, 2, editor.document.LineCount())
}

func TestRichTextareaEnterAddsNewLineInMultilineMode(t *testing.T) {
	editor := NewRichTextarea(RichTextareaConfig{Wrap: true})
	editor = editor.SetPlainText("hello")
	editor = editor.SetCursorFromRuneIndex(5)

	updated, _ := editor.Update(tea.KeyMsg{Type: tea.KeyEnter})
	editor = updated.(RichTextarea)
	updated, _ = editor.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	editor = updated.(RichTextarea)

	require.Equal(t, "hello\nx", editor.Value())
	require.Equal(t, richtext.Position{Line: 1, Cluster: 1}, editor.position)
}
