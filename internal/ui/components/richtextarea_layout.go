package components

import (
	"github.com/rivo/uniseg"

	"github.com/laney/modeloff/internal/richtext"
)

// visualRow is one rendered row: the slice of a document line that
// fits the available width, with the display width it occupies.
type visualRow struct {
	Line  int
	Start int
	End   int
	Width int
}

// layoutRows turns the document into the rows the view renders. With
// wrapping off, a document line is one row however long it is, and the
// single-line viewport scrolls horizontally over it.
func (r RichTextarea) layoutRows(width int) []visualRow {
	width = max(width, 1)

	rows := make([]visualRow, 0, r.document.LineCount())
	for lineIndex := range r.document.LineCount() {
		clusters := r.document.LineClusters(lineIndex)
		if !r.config.Wrap {
			rows = append(rows, visualRow{
				Line:  lineIndex,
				Start: 0,
				End:   len(clusters),
				Width: r.document.LineDisplayWidth(lineIndex),
			})
			continue
		}

		if len(clusters) == 0 {
			rows = append(rows, visualRow{Line: lineIndex})
			continue
		}

		rows = wrapLineIntoRows(rows, lineIndex, clusters, width)
	}

	return rows
}

// wrapLineIntoRows breaks one document line into rows at the last
// permitted break before the width runs out, falling back to a hard
// break mid-word when a single run is wider than the whole row.
func wrapLineIntoRows(rows []visualRow, lineIndex int, clusters []richtext.Grapheme, width int) []visualRow {
	for start := 0; start < len(clusters); {
		rowStart := start
		rowWidth := 0
		lastBreak := rowStart
		lastBreakWidth := 0

		for start < len(clusters) {
			cluster := clusters[start]
			if rowWidth > 0 && rowWidth+cluster.Width > width {
				if lastBreak > rowStart {
					rows = append(rows, visualRow{
						Line:  lineIndex,
						Start: rowStart,
						End:   lastBreak,
						Width: lastBreakWidth,
					})
					start = lastBreak
				} else {
					rows = append(rows, visualRow{
						Line:  lineIndex,
						Start: rowStart,
						End:   start,
						Width: rowWidth,
					})
				}
				rowWidth = 0
				break
			}

			rowWidth += cluster.Width
			start++

			if cluster.LineBreakAfter == uniseg.LineCanBreak {
				lastBreak = start
				lastBreakWidth = rowWidth
			}

			if cluster.LineBreakAfter == uniseg.LineMustBreak {
				rows = append(rows, visualRow{
					Line:  lineIndex,
					Start: rowStart,
					End:   start,
					Width: rowWidth,
				})
				rowWidth = 0
				break
			}
		}

		if rowWidth > 0 {
			rows = append(rows, visualRow{
				Line:  lineIndex,
				Start: rowStart,
				End:   start,
				Width: rowWidth,
			})
		}
	}

	return rows
}

// moveCursor puts the cursor at next, extending the selection or
// collapsing it, and records the column a later vertical move aims
// for.
func (r *RichTextarea) moveCursor(next richtext.Position, extend bool) {
	r.position = r.document.ClampPosition(next)
	if extend {
		r.selection.Head = r.position
	} else {
		r.selection = richtext.Selection{Anchor: r.position, Head: r.position}
	}
	r.preferredColumn = r.cursorCellX(r.position)
	*r = r.ensureViewport()
}

// moveVertical answers where the cursor lands one visual row up or
// down, at the column the user last moved to horizontally. A
// single-line editor has nowhere to go.
func (r RichTextarea) moveVertical(delta int) richtext.Position {
	if r.config.SingleLine {
		return r.position
	}

	rows := r.layoutRows(max(r.width, 1))
	if len(rows) == 0 {
		return r.position
	}

	currentIndex := 0
	for index, row := range rows {
		if row.Line != r.position.Line {
			continue
		}
		if r.position.Cluster < row.Start || r.position.Cluster > row.End {
			continue
		}
		currentIndex = index
		break
	}

	targetIndex := max(currentIndex+delta, 0)
	if targetIndex >= len(rows) {
		targetIndex = len(rows) - 1
	}

	targetRow := rows[targetIndex]
	if r.preferredColumn == 0 {
		r.preferredColumn = r.cursorCellX(r.position)
	}

	return r.positionFromRowColumn(targetRow, r.preferredColumn)
}

// ensureViewport scrolls the visible window so the cursor is inside
// it: vertically for a wrapping editor, horizontally for a single-line
// one.
func (r RichTextarea) ensureViewport() RichTextarea {
	r.position = r.document.ClampPosition(r.position)
	if r.selection.Collapsed() {
		r.selection = richtext.Selection{Anchor: r.position, Head: r.position}
	}

	if !r.config.SingleLine {
		if r.width <= 0 {
			return r
		}

		availableRows := max(r.height, 1)
		if r.config.ShowFormattingStatus {
			availableRows--
		}
		if availableRows < 1 {
			availableRows = 1
		}

		currentRow := r.currentRowIndex(max(r.width, 1))
		if currentRow < r.yOffset {
			r.yOffset = currentRow
		}
		if currentRow >= r.yOffset+availableRows {
			r.yOffset = currentRow - availableRows + 1
		}
		if r.yOffset < 0 {
			r.yOffset = 0
		}

		return r
	}

	if r.width <= 0 {
		return r
	}

	width := r.width
	cursorCell := r.cursorCellX(r.position)
	cursorWidth := r.cursorClusterWidth(r.position)
	if cursorCell < r.xOffset {
		r.xOffset = cursorCell
	}
	if cursorCell+cursorWidth > r.xOffset+width {
		r.xOffset = cursorCell + cursorWidth - width
	}
	if r.xOffset < 0 {
		r.xOffset = 0
	}

	return r
}

// cursorClusterWidth reports the display width of the grapheme under
// the cursor. End-of-line positions report 1 (the cursor block alone).
func (r RichTextarea) cursorClusterWidth(position richtext.Position) int {
	position = r.document.ClampPosition(position)
	clusters := r.document.LineClusters(position.Line)
	if position.Cluster < len(clusters) {
		return clusters[position.Cluster].Width
	}

	return 1
}

// cursorCellX is the cursor's column in display cells, which is what
// a vertical move and the horizontal scroll both measure in.
func (r RichTextarea) cursorCellX(position richtext.Position) int {
	position = r.document.ClampPosition(position)
	width := 0
	for _, cluster := range r.document.LineClusters(position.Line)[:position.Cluster] {
		width += cluster.Width
	}

	return width
}

// positionFromPoint maps a terminal cell the user clicked to a
// document position, taking the scroll offsets and the status row into
// account.
func (r RichTextarea) positionFromPoint(x, y int) richtext.Position {
	rows := r.layoutRows(max(r.width, 1))
	if len(rows) == 0 {
		return richtext.Position{}
	}

	statusRows := 0
	if r.config.ShowFormattingStatus {
		statusRows++
	}

	rowIndex := max(y-statusRows+r.yOffset, 0)
	if rowIndex >= len(rows) {
		rowIndex = len(rows) - 1
	}

	return r.positionFromRowColumn(rows[rowIndex], x+r.xOffset)
}

// currentRowIndex is the visual row the cursor sits on.
func (r RichTextarea) currentRowIndex(width int) int {
	rows := r.layoutRows(width)
	for index, row := range rows {
		if row.Line != r.position.Line {
			continue
		}
		if r.position.Cluster < row.Start || r.position.Cluster > row.End {
			continue
		}
		return index
	}

	return 0
}

// positionFromRowColumn maps a column within one visual row to the
// nearest grapheme boundary, so a click in the right half of a wide
// grapheme lands after it.
func (r RichTextarea) positionFromRowColumn(row visualRow, column int) richtext.Position {
	clusters := r.document.LineClusters(row.Line)
	cell := 0
	for index := row.Start; index < row.End && index < len(clusters); index++ {
		next := cell + clusters[index].Width
		if column <= next {
			if column-cell <= next-column {
				return richtext.Position{Line: row.Line, Cluster: index}
			}
			return richtext.Position{Line: row.Line, Cluster: index + 1}
		}
		cell = next
	}

	return richtext.Position{Line: row.Line, Cluster: row.End}
}
