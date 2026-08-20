package components

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/richtext"
	"github.com/laney/modeloff/internal/ui/theme"
)

// colourTarget names which of a span's two colours the palette is
// editing.
type colourTarget int

const (
	colourTargetForeground colourTarget = iota
	colourTargetBackground
)

// paletteSwatchCount is the number of swatches the palette offers: the
// "no colour" entry plus IRC colours 0-15.
const paletteSwatchCount = 17

// maxPaletteIndex is the last selectable swatch index.
const maxPaletteIndex = paletteSwatchCount - 1

// colourPalette is the swatch row the editor opens on alt+c. It holds
// only the user's place in the row; applying the selection belongs to
// the editor, which owns the document the colour lands on.
type colourPalette struct {
	open   bool
	target colourTarget
	index  int
}

// toggleTarget swaps between editing the foreground and the background
// colour.
func (p *colourPalette) toggleTarget() {
	if p.target == colourTargetForeground {
		p.target = colourTargetBackground

		return
	}

	p.target = colourTargetForeground
}

// moveLeft and moveRight walk the swatch row, stopping at each end.
func (p *colourPalette) moveLeft() {
	if p.index > 0 {
		p.index--
	}
}

func (p *colourPalette) moveRight() {
	if p.index < maxPaletteIndex {
		p.index++
	}
}

// colour is the IRC colour index the current swatch applies, or nil
// for the "no colour" entry, which clears the attribute.
func (p colourPalette) colour() *uint8 {
	if p.index == 0 {
		return nil
	}

	colour := uint8(min(p.index-1, 255)) //nolint:gosec // index bounded by palette size

	return &colour
}

// handlePaletteKey answers a key while the palette is open. It reports
// whether the palette took the key; anything it does not take falls
// through to the editor's own key handling.
func (r RichTextarea) handlePaletteKey(msg tea.KeyMsg) (RichTextarea, bool) {
	if !r.palette.open {
		return r, false
	}

	switch msg.String() {
	case "esc":
		r.palette.open = false
		return r, true
	case "tab":
		r.palette.toggleTarget()
		return r, true
	case "left":
		r.palette.moveLeft()
		return r, true
	case "right":
		r.palette.moveRight()
		return r, true
	case "enter":
		return r.applyPaletteSelection(), true
	}

	if digit, ok := digitRune(msg); ok {
		r.palette.index = digit
		return r, true
	}

	return r, false
}

// digitRune returns the numeric value of a single-digit rune key
// without any modifier, plus a boolean indicating a match. Used by
// the colour palette to let the user jump straight to a swatch.
func digitRune(msg tea.KeyMsg) (int, bool) {
	if msg.Type != tea.KeyRunes || msg.Alt || len(msg.Runes) != 1 {
		return 0, false
	}

	r := msg.Runes[0]
	if r < '0' || r > '9' {
		return 0, false
	}

	return int(r - '0'), true
}

func (r RichTextarea) handlePaletteMouse(msg tea.MouseMsg) (RichTextarea, bool) {
	if !r.palette.open {
		return r, false
	}

	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if msg.Action == tea.MouseActionPress && r.palette.index > 0 {
			r.palette.moveLeft()
			return r, true
		}
	case tea.MouseButtonWheelDown:
		if msg.Action == tea.MouseActionPress && r.palette.index < maxPaletteIndex {
			r.palette.moveRight()
			return r, true
		}
	case tea.MouseButtonLeft:
		if msg.Action != tea.MouseActionPress && msg.Action != tea.MouseActionMotion {
			return r, false
		}

		if msg.X < 4 {
			if msg.Action == tea.MouseActionPress {
				r.palette.toggleTarget()
			}
			return r, true
		}

		index := (msg.X - 4) / 3
		if index < 0 || index > maxPaletteIndex {
			return r, false
		}

		r.palette.index = index
		if msg.Action == tea.MouseActionPress {
			r = r.applyPaletteSelection()
		}
		return r, true
	}

	return r, false
}

// applyPaletteSelection writes the chosen colour into the document: to
// the selection when there is one, and to the pending attributes the
// next typed text takes when there is not. The palette closes either
// way.
func (r RichTextarea) applyPaletteSelection() RichTextarea {
	colour := r.palette.colour()
	if r.selection.Collapsed() {
		if r.palette.target == colourTargetForeground {
			r.pending.FG = colour
		} else {
			r.pending.BG = colour
		}
	} else {
		target := r.palette.target
		r.document.UpdateAttrs(r.selection, func(attrs richtext.Attrs) richtext.Attrs {
			if target == colourTargetForeground {
				attrs.FG = colour
			} else {
				attrs.BG = colour
			}
			return attrs
		})
	}
	r.palette.open = false

	return r
}

// PaletteVisible reports whether the colour palette is open.
func (r RichTextarea) PaletteVisible() bool {
	return r.palette.open
}

// PaletteTarget reports whether the palette is currently editing the
// foreground or background colour. The result is meaningful only when
// PaletteVisible reports true.
func (r RichTextarea) PaletteTarget() PaletteTarget {
	if r.palette.target == colourTargetBackground {
		return PaletteTargetBackground
	}

	return PaletteTargetForeground
}

// PaletteIndex returns the cursor position within the swatch row,
// meaningful only when PaletteVisible reports true.
func (r RichTextarea) PaletteIndex() int {
	return r.palette.index
}

// PaletteTarget identifies which colour slot the palette is editing.
type PaletteTarget int

// Palette target values.
const (
	PaletteTargetForeground PaletteTarget = iota
	PaletteTargetBackground
)

// PaletteView renders the colour palette as a popover row.
func (r RichTextarea) PaletteView(width int) string {
	if !r.palette.open {
		return ""
	}

	return r.palette.view(width)
}

// view renders the swatch row into the given width, scrolling so the
// active swatch stays visible and marking the swatches off either
// edge with a chevron.
func (p colourPalette) view(width int) string {
	target := "fg"
	if p.target == colourTargetBackground {
		target = "bg"
	}

	prefix := target + ": "
	const (
		swatchWidth     = 2
		separatorWidth  = 1
		chevronWidth    = 2 // glyph + trailing space
		swatchStride    = swatchWidth + separatorWidth
		minSwatchBudget = swatchWidth
	)

	available := width - lipgloss.Width(prefix)
	if available < minSwatchBudget {
		return theme.Dim.Width(width).Render(prefix)
	}

	first, last, leftChevron, rightChevron := paletteVisibleRange(p.index, paletteSwatchCount, available, chevronWidth, swatchStride)

	parts := make([]string, 0, paletteSwatchCount+2)
	if leftChevron {
		parts = append(parts, theme.Dim.Render("‹"))
	}
	for index := first; index <= last; index++ {
		parts = append(parts, p.swatch(index))
	}
	if rightChevron {
		parts = append(parts, theme.Dim.Render("›"))
	}

	return theme.Dim.Width(width).Render(prefix + strings.Join(parts, " "))
}

// paletteVisibleRange picks a window of swatch indexes [first, last]
// that fits the available width while keeping the active swatch
// visible. When swatches sit outside the window, the chevrons take
// their share of the budget too.
func paletteVisibleRange(active, count, available, chevronWidth, stride int) (int, int, bool, bool) {
	capacity := (available + 1) / stride // count of swatches that fit ignoring chevrons
	if capacity >= count {
		return 0, count - 1, false, false
	}

	first := max(active-capacity/2, 0)
	last := first + capacity - 1
	if last >= count {
		last = count - 1
		first = last - capacity + 1
	}

	leftChevron := first > 0
	rightChevron := last < count-1
	chevronCount := 0
	if leftChevron {
		chevronCount++
	}
	if rightChevron {
		chevronCount++
	}

	// Subtract the chevron budget and recompute if needed.
	if chevronCount > 0 {
		capacity = max((available-chevronCount*chevronWidth+1)/stride, 1)
		first = max(active-capacity/2, 0)
		last = first + capacity - 1
		if last >= count {
			last = count - 1
			first = last - capacity + 1
		}
		leftChevron = first > 0
		rightChevron = last < count-1
	}

	return first, last, leftChevron, rightChevron
}

// swatch returns the visual for a single palette swatch in fixed
// two-cell form, matching the mouse hit-test stride. Index 0 is the
// "no colour" entry rendered as a dim em-dash plus filler so the user
// reads "clear" rather than a cryptic colour label. Numbered swatches
// paint the colour the user is about to apply (foreground or
// background depending on the active target).
func (p colourPalette) swatch(index int) string {
	if index == 0 {
		style := theme.Dim
		if index == p.index {
			style = lipgloss.NewStyle().Reverse(true).Bold(true)
		}

		return style.Render("--")
	}

	colour := uint8(index - 1) //nolint:gosec // bounded by paletteSwatchCount
	label := fmt.Sprintf("%02d", colour)
	style := lipgloss.NewStyle()
	if p.target == colourTargetForeground {
		style = style.Foreground(lipgloss.ANSIColor(colour))
	} else {
		style = style.Background(lipgloss.ANSIColor(colour))
	}
	if index == p.index {
		style = style.Reverse(true).Bold(true)
	}

	return style.Render(label)
}
