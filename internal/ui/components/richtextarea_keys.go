package components

import (
	tea "charm.land/bubbletea/v2"

	"github.com/laney/modeloff/internal/richtext"
	"github.com/laney/modeloff/internal/ui"
)

// handleFormattingKey handles the keys that toggle IRC formatting on
// the selection, or on the text the user types next when there is no
// selection. With formatting switched off the key is still taken, so
// a chord never falls through and inserts its letter as text.
func (r RichTextarea) handleFormattingKey(msg tea.KeyPressMsg) (RichTextarea, bool) {
	toggles := []struct {
		binding ui.KeyBinding
		flip    func(*richtext.Attrs)
	}{
		{r.keyMap.ToggleBold, func(a *richtext.Attrs) { a.Bold = !a.Bold }},
		{r.keyMap.ToggleItalic, func(a *richtext.Attrs) { a.Italic = !a.Italic }},
		{r.keyMap.ToggleUnderline, func(a *richtext.Attrs) { a.Underline = !a.Underline }},
		{r.keyMap.ToggleReverse, func(a *richtext.Attrs) { a.Reverse = !a.Reverse }},
		{r.keyMap.ToggleStrike, func(a *richtext.Attrs) { a.Strike = !a.Strike }},
	}

	for _, toggle := range toggles {
		if !ui.Matches(msg, toggle.binding) {
			continue
		}
		if !r.config.AllowFormatting {
			return r, true
		}

		return r.toggleFormatting(toggle.flip), true
	}

	switch {
	case ui.Matches(msg, r.keyMap.ResetFormat):
		if !r.config.AllowFormatting {
			return r, true
		}
		if r.selection.Collapsed() {
			r.pending = richtext.Attrs{}
		} else {
			r.document.UpdateAttrs(r.selection, func(richtext.Attrs) richtext.Attrs { return richtext.Attrs{} })
		}

		return r, true

	case ui.Matches(msg, r.keyMap.OpenPalette):
		if !r.config.AllowFormatting {
			return r, true
		}
		r.palette.open = true
		r.palette.index = 0
		r.palette.target = colourTargetForeground

		return r, true
	}

	return r, false
}

// toggleFormatting flips one attribute. With no selection the flip
// lands on the pending attributes the next typed text takes; with one,
// every span in it is rewritten from the attributes at its end, so a
// mixed selection settles on a single state.
func (r RichTextarea) toggleFormatting(toggle func(*richtext.Attrs)) RichTextarea {
	if r.selection.Collapsed() {
		toggle(&r.pending)
		return r
	}

	start, end := r.selection.Normalized()
	if start == end {
		return r
	}

	target := r.document.AttrsBefore(end)
	toggle(&target)
	r.document.UpdateAttrs(r.selection, func(attrs richtext.Attrs) richtext.Attrs {
		next := attrs
		toggle(&next)
		return next
	})

	return r
}

// handleEditorKey answers the movement, selection, kill and text-entry
// keys, following readline's bindings where the terminal has one.
func (r RichTextarea) handleEditorKey(msg tea.KeyPressMsg) (RichTextarea, bool) {
	extend := msg.Mod.Contains(tea.ModShift)

	switch {
	case ui.Matches(msg, r.keyMap.WordLeft):
		r.moveCursor(r.document.MoveWordLeft(r.position), extend)
		return r, true

	case ui.Matches(msg, r.keyMap.WordRight):
		r.moveCursor(r.document.MoveWordRight(r.position), extend)
		return r, true

	case ui.Matches(msg, r.keyMap.Left):
		r.moveCursor(r.document.MoveLeft(r.position), extend)
		return r, true

	case ui.Matches(msg, r.keyMap.Right):
		r.moveCursor(r.document.MoveRight(r.position), extend)
		return r, true

	case ui.Matches(msg, r.keyMap.Up):
		r.moveCursor(r.moveVertical(-1), extend)
		return r, true

	case ui.Matches(msg, r.keyMap.Down):
		r.moveCursor(r.moveVertical(1), extend)
		return r, true

	case ui.Matches(msg, r.keyMap.LineStart):
		r.moveCursor(r.document.MoveLineStart(r.position), extend)
		return r, true

	case ui.Matches(msg, r.keyMap.LineEnd):
		r.moveCursor(r.document.MoveLineEnd(r.position), extend)
		return r, true

	case ui.Matches(msg, r.keyMap.DeleteWordFwd):
		if !r.selection.Collapsed() {
			r.killSelection()
			return r, true
		}
		end := r.document.MoveWordRight(r.position)
		r.killRange(richtext.Selection{Anchor: r.position, Head: end})

		return r, true

	case ui.Matches(msg, r.keyMap.DeleteWordBack):
		if !r.selection.Collapsed() {
			r.killSelection()
			return r, true
		}
		start := r.document.MoveWordLeft(r.position)
		r.killRange(richtext.Selection{Anchor: start, Head: r.position})

		return r, true

	case ui.Matches(msg, r.keyMap.DeleteToEnd):
		if !r.selection.Collapsed() {
			r.killSelection()
			return r, true
		}
		end := richtext.Position{
			Line:    r.position.Line,
			Cluster: r.document.LineClusterCount(r.position.Line),
		}
		r.killRange(richtext.Selection{Anchor: r.position, Head: end})

		return r, true

	case ui.Matches(msg, r.keyMap.Transpose):
		return r.transposeChars(), true

	case ui.Matches(msg, r.keyMap.Yank):
		return r.yank(), true

	case ui.Matches(msg, r.keyMap.Backspace):
		if !r.selection.Collapsed() {
			r.deleteSelection()
			return r, true
		}
		start := r.document.MoveLeft(r.position)
		r.position = r.document.Delete(richtext.Selection{Anchor: start, Head: r.position})
		r.selection = richtext.Selection{Anchor: r.position, Head: r.position}

		return r.ensureViewport(), true

	case ui.Matches(msg, r.keyMap.Delete):
		if !r.selection.Collapsed() {
			r.deleteSelection()
			return r, true
		}
		end := r.document.MoveRight(r.position)
		r.position = r.document.Delete(richtext.Selection{Anchor: r.position, Head: end})
		r.selection = richtext.Selection{Anchor: r.position, Head: r.position}

		return r.ensureViewport(), true

	case ui.Matches(msg, r.keyMap.Newline):
		if r.config.SingleLine {
			return r, false
		}
		r.insertText("\n")

		return r, true
	}

	if msg.Text != "" {
		r.insertText(msg.Text)
		return r, true
	}

	return r, false
}
