package components

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/ircfmt"
	"github.com/laney/modeloff/internal/richtext"
	"github.com/laney/modeloff/internal/ui/theme"
)

// View implements ui.Model.
func (r RichTextarea) View(width, height int) string {
	r.width = max(width, 0)
	r.height = max(height, 1)
	r = r.ensureViewport()

	if r.width == 0 {
		return ""
	}

	rows := r.layoutRows(r.width)
	if r.config.SingleLine && len(rows) == 0 {
		rows = []visualRow{{Line: 0}}
	}

	startRow := r.yOffset
	if startRow >= len(rows) {
		startRow = max(len(rows)-1, 0)
	}

	maxRows := max(len(rows)-startRow, 0)

	availableRows := max(r.height, 1)
	if r.config.ShowFormattingStatus {
		availableRows--
	}
	if availableRows < 1 {
		availableRows = 1
	}

	if maxRows > availableRows {
		maxRows = availableRows
	}

	parts := make([]string, 0, 1+boolToInt(r.config.ShowFormattingStatus))

	if r.config.ShowFormattingStatus {
		parts = append(parts, r.renderStatus(r.width))
	}

	renderedRows := make([]string, 0, maxRows)
	for _, row := range rows[startRow : startRow+maxRows] {
		renderedRows = append(renderedRows, r.renderRow(row, r.width))
	}
	if len(renderedRows) == 0 {
		renderedRows = append(renderedRows, lipgloss.NewStyle().Width(r.width).Render(""))
	}

	parts = append(parts, lipgloss.JoinVertical(lipgloss.Left, renderedRows...))

	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// renderRow paints one visual row: each grapheme in its own
// attributes, reversed where the selection covers it, with the cursor
// block drawn over the grapheme it sits on.
func (r RichTextarea) renderRow(row visualRow, width int) string {
	width = max(width, 1)
	clusters := r.document.LineClusters(row.Line)
	selectionStart, selectionEnd := r.selection.Normalized()

	var (
		builder      strings.Builder
		cellPosition int
		cursorDrawn  bool
	)

	for clusterIndex := row.Start; clusterIndex <= row.End; clusterIndex++ {
		if !cursorDrawn && r.position.Line == row.Line && r.position.Cluster == clusterIndex {
			clusterText := " "
			if clusterIndex < len(clusters) {
				clusterText = clusters[clusterIndex].Text
			}
			if !r.config.SingleLine || cellPosition >= r.xOffset {
				r.cursor.SetChar(clusterText)
				builder.WriteString(r.cursor.View())
			}
			cursorDrawn = true
			if clusterIndex == len(clusters) {
				break
			}
		}

		if clusterIndex >= len(clusters) {
			break
		}

		cluster := clusters[clusterIndex]
		nextCell := cellPosition + cluster.Width
		if r.config.SingleLine && nextCell <= r.xOffset {
			cellPosition = nextCell
			continue
		}
		if r.config.SingleLine && cellPosition-r.xOffset >= width {
			break
		}

		style := styleForAttrs(cluster.Attrs)
		if selectionContains(selectionStart, selectionEnd, richtext.Position{Line: row.Line, Cluster: clusterIndex}) {
			style = style.Reverse(true)
		}

		if !cursorDrawn || r.position.Line != row.Line || r.position.Cluster != clusterIndex {
			builder.WriteString(style.Render(cluster.Text))
		}
		cellPosition = nextCell
	}

	line := builder.String()
	if line == "" && cursorDrawn {
		line = r.cursor.View()
	}

	return lipgloss.NewStyle().Width(width).Render(line)
}

func (r RichTextarea) renderStatus(width int) string {
	return theme.Dim.Width(width).Render(r.StatusText())
}

// activeAttrs returns the formatting attributes active at the cursor.
func (r RichTextarea) activeAttrs() richtext.Attrs {
	if !r.selection.Collapsed() {
		return r.document.AttrsBefore(r.selection.Head)
	}

	return r.pending
}

// StatusText returns a compact summary of the active formatting state.
func (r RichTextarea) StatusText() string {
	active := r.activeAttrs()

	var bits []string
	if active.Bold {
		bits = append(bits, "B")
	}
	if active.Italic {
		bits = append(bits, "I")
	}
	if active.Underline {
		bits = append(bits, "U")
	}
	if active.Reverse {
		bits = append(bits, "R")
	}
	if active.Strike {
		bits = append(bits, "S")
	}
	if active.FG != nil {
		bits = append(bits, fmt.Sprintf("fg:%02d", *active.FG))
	}
	if active.BG != nil {
		bits = append(bits, fmt.Sprintf("bg:%02d", *active.BG))
	}
	if !r.selection.Collapsed() {
		bits = append(bits, "sel")
	}

	if len(bits) == 0 {
		return "plain"
	}

	return strings.Join(bits, " ")
}

func selectionContains(start, end, position richtext.Position) bool {
	if start == end {
		return false
	}

	if lessPosition(position, start) {
		return false
	}
	if !lessPosition(position, end) {
		return false
	}

	return true
}

func lessPosition(left, right richtext.Position) bool {
	if left.Line != right.Line {
		return left.Line < right.Line
	}

	return left.Cluster < right.Cluster
}

func styleForAttrs(attrs richtext.Attrs) lipgloss.Style {
	style := lipgloss.NewStyle()
	if attrs.Bold {
		style = style.Bold(true)
	}
	if attrs.Italic {
		style = style.Italic(true)
	}
	if attrs.Underline {
		style = style.Underline(true)
	}
	if attrs.Reverse {
		style = style.Reverse(true)
	}
	if attrs.Strike {
		style = style.Strikethrough(true)
	}
	if attrs.FG != nil {
		style = style.Foreground(ircColour(*attrs.FG))
	}
	if attrs.BG != nil {
		style = style.Background(ircColour(*attrs.BG))
	}

	return style
}

// ircColour renders an IRC colour index as a lipgloss colour. Indices
// 16-98 carry the modern formatting spec's fixed RGB value, which is
// independent of any terminal theme; indices 0-15 have no spec-defined
// RGB value and render through the terminal's own ANSI palette instead,
// per the design system's rule of using ANSI colours so the user's theme
// still governs the base palette.
func ircColour(index uint8) lipgloss.TerminalColor {
	if hex, ok := ircfmt.ExtendedRGB(index); ok {
		return lipgloss.Color(hex)
	}

	return lipgloss.ANSIColor(index)
}

func boolToInt(value bool) int {
	if value {
		return 1
	}

	return 0
}
