package components

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/richtext"
)

// multiClickInterval is how close together two clicks at one position
// have to fall to count as a double-click.
const multiClickInterval = 500 * time.Millisecond

// clickTracker counts consecutive clicks at one position, which is
// what tells a click from a double- or triple-click.
type clickTracker struct {
	lastAt  time.Time
	lastPos richtext.Position
	count   int
}

// press records a click at position and returns how many have landed
// there in a row: 1 for a fresh click, 2 for a double, 3 for a triple.
// A fourth click starts the count again, so clicking on gives the user
// a plain cursor back after the line selection.
func (c *clickTracker) press(position richtext.Position, now time.Time) int {
	if now.Sub(c.lastAt) < multiClickInterval && position == c.lastPos {
		c.count++
	} else {
		c.count = 1
	}

	if c.count > 3 {
		c.count = 1
	}

	c.lastAt = now
	c.lastPos = position

	return c.count
}

func (r RichTextarea) handleMouse(msg tea.MouseMsg) (RichTextarea, bool) {
	if r.palette.open {
		if updated, handled := r.handlePaletteMouse(msg); handled {
			return updated, true
		}
	}

	if !r.config.SingleLine {
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			if msg.Action == tea.MouseActionPress && r.yOffset > 0 {
				r.yOffset--
				return r, true
			}
		case tea.MouseButtonWheelDown:
			if msg.Action == tea.MouseActionPress {
				r.yOffset++
				return r.ensureViewport(), true
			}
		}
	}

	if msg.Button != tea.MouseButtonLeft {
		return r, false
	}

	switch msg.Action {
	case tea.MouseActionPress:
		position := r.positionFromPoint(msg.X, msg.Y)

		switch r.clicks.press(position, time.Now()) {
		case 2:
			r.selection = r.wordSelection(position)
			r.position = r.selection.Head
			return r.ensureViewport(), true
		case 3:
			r.selection = r.lineSelection(position)
			r.position = r.selection.Head
			return r.ensureViewport(), true
		}

		r.mouseSelecting = true
		r.position = position
		r.selection = richtext.Selection{Anchor: r.position, Head: r.position}
		return r.ensureViewport(), true
	case tea.MouseActionMotion:
		if !r.mouseSelecting {
			return r, false
		}
		r.position = r.positionFromPoint(msg.X, msg.Y)
		r.selection.Head = r.position
		return r.ensureViewport(), true
	case tea.MouseActionRelease:
		r.mouseSelecting = false
		return r, true
	}

	return r, false
}

// lineSelection covers the whole line the position sits on, which is
// what a triple-click selects.
func (r RichTextarea) lineSelection(position richtext.Position) richtext.Selection {
	return richtext.Selection{
		Anchor: richtext.Position{Line: position.Line, Cluster: 0},
		Head: richtext.Position{
			Line:    position.Line,
			Cluster: r.document.LineClusterCount(position.Line),
		},
	}
}

// wordSelection covers the word around the position, which is what a
// double-click selects. Between two word boundaries there is no word
// to take, so a single grapheme is selected instead.
func (r RichTextarea) wordSelection(position richtext.Position) richtext.Selection {
	start := r.document.MoveWordLeft(position)
	end := r.document.MoveWordRight(position)
	if start == end {
		if end.Cluster < r.document.LineClusterCount(end.Line) {
			end.Cluster++
		}
	}

	return richtext.Selection{Anchor: start, Head: end}
}
