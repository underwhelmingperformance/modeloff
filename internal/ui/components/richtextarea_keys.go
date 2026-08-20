package components

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/richtext"
)

// handleFormattingKey answers the alt-modified keys that toggle IRC
// formatting on the selection, or on the text the user types next when
// there is no selection. With formatting switched off the key is still
// taken, so an alt combination never falls through and inserts its
// letter as text.
func (r RichTextarea) handleFormattingKey(msg tea.KeyMsg) (RichTextarea, bool) {
	if !msg.Alt {
		return r, false
	}

	if msg.Type != tea.KeyRunes && msg.Type != tea.KeySpace {
		return r, false
	}

	switch strings.ToLower(string(msg.Runes)) {
	case "b":
		if !r.config.AllowFormatting {
			return r, true
		}
		return r.toggleFormatting(func(attrs *richtext.Attrs) { attrs.Bold = !attrs.Bold }), true
	case "i":
		if !r.config.AllowFormatting {
			return r, true
		}
		return r.toggleFormatting(func(attrs *richtext.Attrs) { attrs.Italic = !attrs.Italic }), true
	case "u":
		if !r.config.AllowFormatting {
			return r, true
		}
		return r.toggleFormatting(func(attrs *richtext.Attrs) { attrs.Underline = !attrs.Underline }), true
	case "r":
		if !r.config.AllowFormatting {
			return r, true
		}
		return r.toggleFormatting(func(attrs *richtext.Attrs) { attrs.Reverse = !attrs.Reverse }), true
	case "s":
		if !r.config.AllowFormatting {
			return r, true
		}
		return r.toggleFormatting(func(attrs *richtext.Attrs) { attrs.Strike = !attrs.Strike }), true
	case "o":
		if !r.config.AllowFormatting {
			return r, true
		}
		if r.selection.Collapsed() {
			r.pending = richtext.Attrs{}
		} else {
			r.document.UpdateAttrs(r.selection, func(richtext.Attrs) richtext.Attrs { return richtext.Attrs{} })
		}
		return r, true
	case "c":
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
func (r RichTextarea) handleEditorKey(msg tea.KeyMsg) (RichTextarea, bool) {
	extendSelection := false
	switch msg.Type {
	case tea.KeyShiftLeft, tea.KeyShiftRight, tea.KeyShiftUp, tea.KeyShiftDown:
		extendSelection = true
	}

	switch msg.String() {
	case "alt+d":
		if !r.selection.Collapsed() {
			r.killSelection()
			return r, true
		}
		end := r.document.MoveWordRight(r.position)
		r.killRange(richtext.Selection{Anchor: r.position, Head: end})
		return r, true
	case "ctrl+left":
		r.moveCursor(r.document.MoveWordLeft(r.position), extendSelection)
		return r, true
	case "ctrl+right", "alt+f":
		r.moveCursor(r.document.MoveWordRight(r.position), extendSelection)
		return r, true
	case "ctrl+shift+left":
		r.moveCursor(r.document.MoveWordLeft(r.position), true)
		return r, true
	case "ctrl+shift+right":
		r.moveCursor(r.document.MoveWordRight(r.position), true)
		return r, true
	case "left", "shift+left":
		r.moveCursor(r.document.MoveLeft(r.position), extendSelection)
		return r, true
	case "right", "shift+right":
		r.moveCursor(r.document.MoveRight(r.position), extendSelection)
		return r, true
	case "home":
		r.moveCursor(r.document.MoveLineStart(r.position), extendSelection)
		return r, true
	case "shift+home":
		r.moveCursor(r.document.MoveLineStart(r.position), true)
		return r, true
	case "end":
		r.moveCursor(r.document.MoveLineEnd(r.position), extendSelection)
		return r, true
	case "shift+end":
		r.moveCursor(r.document.MoveLineEnd(r.position), true)
		return r, true
	case "up", "shift+up":
		r.moveCursor(r.moveVertical(-1), extendSelection)
		return r, true
	case "down", "shift+down":
		r.moveCursor(r.moveVertical(1), extendSelection)
		return r, true
	case "ctrl+a":
		r.moveCursor(r.document.MoveLineStart(r.position), false)
		return r, true
	case "ctrl+e":
		r.moveCursor(r.document.MoveLineEnd(r.position), false)
		return r, true
	case "ctrl+k":
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
	case "ctrl+t":
		return r.transposeChars(), true
	case "ctrl+w", "alt+backspace":
		if !r.selection.Collapsed() {
			r.killSelection()
			return r, true
		}
		start := r.document.MoveWordLeft(r.position)
		r.killRange(richtext.Selection{Anchor: start, Head: r.position})
		return r, true
	case "ctrl+y":
		return r.yank(), true
	case "backspace":
		if !r.selection.Collapsed() {
			r.deleteSelection()
			return r, true
		}
		start := r.document.MoveLeft(r.position)
		r.position = r.document.Delete(richtext.Selection{Anchor: start, Head: r.position})
		r.selection = richtext.Selection{Anchor: r.position, Head: r.position}
		return r.ensureViewport(), true
	case "delete":
		if !r.selection.Collapsed() {
			r.deleteSelection()
			return r, true
		}
		end := r.document.MoveRight(r.position)
		r.position = r.document.Delete(richtext.Selection{Anchor: r.position, Head: end})
		r.selection = richtext.Selection{Anchor: r.position, Head: r.position}
		return r.ensureViewport(), true
	case "enter":
		if r.config.SingleLine {
			return r, false
		}
		r.insertText("\n")
		return r, true
	}

	if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
		r.insertText(string(msg.Runes))
		return r, true
	}

	return r, false
}
