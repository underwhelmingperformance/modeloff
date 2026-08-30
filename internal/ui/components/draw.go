package components

import (
	"image"

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
	c.input.Draw(screen, layout.InputRect)
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
	if b.locked {
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
