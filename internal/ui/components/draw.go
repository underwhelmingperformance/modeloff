package components

import (
	"image"
	"strings"

	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/laney/modeloff/internal/ui/theme"
)

func drawString(screen uv.Screen, area uv.Rectangle, content string) {
	uv.NewStyledString(content).Draw(screen, area)
}

func contains(area uv.Rectangle, x, y int) bool {
	return image.Pt(x, y).In(area)
}

func localPoint(area uv.Rectangle, x, y int) (int, int) {
	return x - area.Min.X, y - area.Min.Y
}

// Draw implements ui.Component.
func (s Sidebar[T, K]) Draw(screen uv.Screen, area uv.Rectangle) {
	drawString(screen, area, s.render(area.Dx(), area.Dy()))
}

// Draw implements ui.Component.
func (c ChannelSidebar) Draw(screen uv.Screen, area uv.Rectangle) {
	c.panel.Draw(screen, area)
}

// Draw implements ui.Component.
func (c ChatView[C]) Draw(screen uv.Screen, area uv.Rectangle) {
	layout := c.layoutRectsFor(area)
	if layout.MessageRect.Min.Y > area.Min.Y {
		headerArea := uv.Rect(area.Min.X, area.Min.Y, area.Dx(), layout.MessageRect.Min.Y-area.Min.Y)
		drawString(screen, headerArea, c.renderHeader(area.Dx()))
	}

	c.messages.Draw(screen, layout.MessageRect)

	// A modal layer takes every key the window receives, so the input
	// bar under it receives none. Its cursor is blurred while one is
	// open, leaving whatever the operator had typed there where it is.
	input := c.input
	_, modal := c.selector()
	input.blurred = modal
	input.Draw(screen, layout.InputRect)

	c.layers.Draw(screen, area)
}

// Draw implements ui.Component.
func (b InputBar) Draw(screen uv.Screen, area uv.Rectangle) {
	layout := b.layout(area)

	if !layout.palette.Empty() {
		drawString(screen, layout.palette, b.input.paletteView(layout.palette.Dx()))
	}

	if !layout.note.Empty() {
		drawString(screen, layout.note, theme.Dim.Render("Pasted text flattened to one line"))
	}

	nickLabel, lockBadge, prompt := b.inputPrefix()
	drawString(screen, layout.input, nickLabel+lockBadge+prompt)

	editor := b.input
	if b.locked || b.blurred {
		editor.cursor.Blur()
	}
	editor.Draw(screen, layout.editor)
}

// Draw implements ui.Component.
func (m MessageList[C]) Draw(screen uv.Screen, area uv.Rectangle) {
	drawString(screen, area, m.render(area.Dx(), area.Dy()))
}

// Draw implements ui.Component.
func (f FeedView) Draw(screen uv.Screen, area uv.Rectangle) {
	drawString(screen, area, f.render(area.Dx(), area.Dy()))
}

// Draw implements ui.Component.
func (m MetricsPane) Draw(screen uv.Screen, area uv.Rectangle) {
	m.feed.Draw(screen, area)
}

// Draw implements ui.Component.
func (n NickList) Draw(screen uv.Screen, area uv.Rectangle) {
	n.panel.Draw(screen, area)
}

// Draw implements ui.Component.
func (p Popover) Draw(screen uv.Screen, area uv.Rectangle) {
	drawString(screen, area, p.render(area.Dx()))
}

// Draw implements ui.Component. An area too short for the bordered pane
// the rest of the app uses gets one row: the adjustment field when that
// field's trimmed value is not empty, and the condensed form
// otherwise.
func (p PersonaSelector) Draw(screen uv.Screen, area uv.Rectangle) {
	if area.Empty() {
		return
	}

	if area.Dy() < personaSelectorChrome+1 {
		// One row holds the candidate or the adjustment, and the
		// adjustment wins: the field the operator is typing into is what
		// they need to see.
		if strings.TrimSpace(p.adjustment.Value()) != "" {
			row := uv.Rect(area.Min.X, area.Min.Y, area.Dx(), 1)
			field := row

			// The keys take their columns only when the row is wide
			// enough for them and `minCondensedField` besides. A
			// narrower row keeps the field, because an adjustment the
			// operator cannot read is one that has changed what Enter
			// does without their knowing.
			if keys := p.condensedKeys(); row.Dx() >= minCondensedField+lipgloss.Width(keys) {
				drawString(screen, uv.Rect(
					row.Max.X-lipgloss.Width(keys), row.Min.Y, lipgloss.Width(keys), 1,
				), theme.Dim.Render(keys))

				field = uv.Rect(row.Min.X, row.Min.Y, row.Dx()-lipgloss.Width(keys), 1)
			}

			drawAdjustmentField(screen, field, p)

			return
		}

		drawString(screen, area, p.condensed(area.Dx()))

		return
	}

	drawBorderedPane(screen, area, p.title(area.Dx()), true, func(content uv.Rectangle) {
		rows, field := p.bodyRows(content.Dx(), content.Dy())
		drawString(screen, content, strings.Join(rows, "\n"))

		if field < 0 || field >= content.Dy() {
			return
		}

		drawAdjustmentField(
			screen, uv.Rect(content.Min.X, content.Min.Y+field, content.Dx(), 1), p,
		)
	})
}

// minCondensedField is how many columns the condensed row reserves for
// the adjustment field before it gives any to the key list: enough for
// the field's label and for reading back what was typed.
const minCondensedField = 20

// drawAdjustmentField writes the selector's adjustment field into one
// row: its label, then the editor, which draws its own horizontally
// scrolled view of the text and the cursor in it.
func drawAdjustmentField(screen uv.Screen, area uv.Rectangle, p PersonaSelector) {
	const label = "adjust: "

	drawString(screen, area, theme.Dim.Render(label))

	p.adjustment.Draw(screen, uv.Rect(
		area.Min.X+lipgloss.Width(label), area.Min.Y,
		max(area.Dx()-lipgloss.Width(label), 0), 1,
	))
}

// Draw implements ui.Component.
func (r RichTextarea) Draw(screen uv.Screen, area uv.Rectangle) {
	drawString(screen, area, r.render(area.Dx(), area.Dy()))
}

func (d observabilityDrawer) Draw(screen uv.Screen, area uv.Rectangle) {
	layout := d.layout(area)
	drawBorderedPane(screen, layout.LogsRect, "Logs", d.Focus == workspaceFocusLogs, func(area uv.Rectangle) {
		d.Logs.Draw(screen, area)
	})

	drawBorderedPane(screen, layout.MetricsRect, "Metrics", d.Focus == workspaceFocusMetrics, func(area uv.Rectangle) {
		if !d.HasMetrics {
			placeholder := lipgloss.Place(area.Dx(), area.Dy(), lipgloss.Center, lipgloss.Center, "No metrics yet")
			drawString(screen, area, placeholder)
			return
		}

		d.Metrics.Draw(screen, area)
	})
}

func drawBorderedPane(
	screen uv.Screen,
	area uv.Rectangle,
	title string,
	focused bool,
	drawContent func(uv.Rectangle),
) {
	if area.Empty() {
		return
	}

	style := theme.PaneBorder
	if focused {
		style = theme.PaneBorderFocused
	}

	drawString(screen, area, style.Width(area.Dx()).Height(area.Dy()).Render(" "))

	contentArea := borderedContentRect(area)
	titleY := contentArea.Min.Y - 1
	if titleY >= area.Min.Y && titleY < area.Max.Y {
		titleArea := uv.Rect(contentArea.Min.X, titleY, contentArea.Dx(), 1)
		drawString(screen, titleArea, theme.Bold.Render(title))
	}

	if !contentArea.Empty() {
		drawContent(contentArea)
	}
}
