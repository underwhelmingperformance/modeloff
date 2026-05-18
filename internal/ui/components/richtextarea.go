package components

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/rivo/uniseg"

	"github.com/laney/modeloff/internal/richtext"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

// RichTextareaConfig configures the editor widget.
type RichTextareaConfig struct {
	SingleLine           bool
	Wrap                 bool
	AllowFormatting      bool
	ShowFormattingStatus bool
}

type colourTarget int

const (
	colourTargetForeground colourTarget = iota
	colourTargetBackground
)

type colourPalette struct {
	open   bool
	target colourTarget
	index  int
}

type visualRow struct {
	Line      int
	Start     int
	End       int
	Width     int
	CursorX   int
	CursorSet bool
}

// RichTextarea is a grapheme-aware rich text editor widget.
type RichTextarea struct {
	config RichTextareaConfig

	document  richtext.Document
	cursor    cursor.Model
	position  richtext.Position
	selection richtext.Selection
	pending   richtext.Attrs

	preferredColumn int
	xOffset         int
	yOffset         int

	width  int
	height int

	mouseSelecting bool
	palette        colourPalette
	lastClickAt    time.Time
	lastClickPos   richtext.Position
}

// NewRichTextarea creates a new editor.
func NewRichTextarea(config RichTextareaConfig) RichTextarea {
	cur := cursor.New()
	cur.Focus()
	cur.SetMode(cursor.CursorBlink)
	cur.TextStyle = lipgloss.NewStyle()

	editor := RichTextarea{
		config:   config,
		document: richtext.NewDocument(),
		cursor:   cur,
	}
	editor.selection = richtext.Selection{Anchor: editor.position, Head: editor.position}

	return editor
}

// Init implements ui.Model.
func (r RichTextarea) Init() tea.Cmd {
	return nil
}

// Update implements ui.Model.
func (r RichTextarea) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if updated, handled := r.handlePaletteKey(msg); handled {
			return updated, nil
		}

		if updated, handled := r.handleFormattingKey(msg); handled {
			return updated, nil
		}

		if updated, handled, cmd := r.handleEditorKey(msg); handled {
			return updated, cmd
		}

	case tea.MouseMsg:
		if updated, handled := r.handleMouse(msg); handled {
			return updated, nil
		}
	}

	var cmd tea.Cmd
	r.cursor, cmd = r.cursor.Update(msg)

	return r, cmd
}

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

// Value returns the plain-text value.
func (r RichTextarea) Value() string {
	return r.document.Plain()
}

// Document returns a copy of the current document.
func (r RichTextarea) Document() richtext.Document {
	return r.document.Clone()
}

// Cursor returns the cursor position in runes on the current line.
func (r RichTextarea) Cursor() int {
	return r.document.RuneIndex(r.position)
}

// SetPlainText replaces the document with plain text.
func (r RichTextarea) SetPlainText(text string) RichTextarea {
	r.document = richtext.NewDocumentFromText(text, richtext.Attrs{})
	r.position = richtext.Position{}
	r.selection = richtext.Selection{Anchor: r.position, Head: r.position}
	r.pending = richtext.Attrs{}
	r.xOffset = 0
	r.yOffset = 0

	return r
}

// SetDocument replaces the document and resets the selection.
func (r RichTextarea) SetDocument(document richtext.Document) RichTextarea {
	r.document = document.Clone()
	r.position = richtext.Position{}
	r.selection = richtext.Selection{Anchor: r.position, Head: r.position}
	r.pending = richtext.Attrs{}
	r.xOffset = 0
	r.yOffset = 0

	return r
}

// SetCursorFromRuneIndex moves the cursor to the given rune index on the current line.
func (r RichTextarea) SetCursorFromRuneIndex(index int) RichTextarea {
	line := r.position.Line
	r.position = r.document.PositionFromRuneIndex(line, index)
	r.selection = richtext.Selection{Anchor: r.position, Head: r.position}

	return r.ensureViewport()
}

// ReplaceRange replaces the given rune range on the current line with plain text.
func (r RichTextarea) ReplaceRange(start, end int, replacement string) RichTextarea {
	line := r.position.Line
	anchor := r.document.PositionFromRuneIndex(line, start)
	head := r.document.PositionFromRuneIndex(line, end)
	r.position = r.document.ReplaceText(richtext.Selection{Anchor: anchor, Head: head}, replacement, r.pending)
	r.selection = richtext.Selection{Anchor: r.position, Head: r.position}

	return r.ensureViewport()
}

// SetCursorFromCell moves the cursor to the nearest cell in the current viewport.
func (r RichTextarea) SetCursorFromCell(x int) RichTextarea {
	if x <= 0 {
		r.position = richtext.Position{Line: r.position.Line}
		r.selection = richtext.Selection{Anchor: r.position, Head: r.position}
		return r.ensureViewport()
	}

	line := r.position.Line
	clusters := r.document.LineClusters(line)
	cell := r.xOffset + x
	width := 0
	for index, cluster := range clusters {
		next := width + cluster.Width
		if cell <= next {
			if cell-width <= next-cell {
				r.position = richtext.Position{Line: line, Cluster: index}
			} else {
				r.position = richtext.Position{Line: line, Cluster: index + 1}
			}

			r.selection = richtext.Selection{Anchor: r.position, Head: r.position}
			return r.ensureViewport()
		}

		width = next
	}

	r.position = richtext.Position{Line: line, Cluster: len(clusters)}
	r.selection = richtext.Selection{Anchor: r.position, Head: r.position}

	return r.ensureViewport()
}

// SetAllowFormatting updates formatting availability.
func (r RichTextarea) SetAllowFormatting(allow bool) RichTextarea {
	r.config.AllowFormatting = allow
	if !allow {
		r.palette.open = false
	}

	return r
}

func (r RichTextarea) handlePaletteKey(msg tea.KeyMsg) (RichTextarea, bool) {
	if !r.palette.open {
		return r, false
	}

	switch msg.String() {
	case "esc":
		r.palette.open = false
		return r, true
	case "tab":
		if r.palette.target == colourTargetForeground {
			r.palette.target = colourTargetBackground
		} else {
			r.palette.target = colourTargetForeground
		}
		return r, true
	case "left":
		if r.palette.index > 0 {
			r.palette.index--
		}
		return r, true
	case "right":
		if r.palette.index < 16 {
			r.palette.index++
		}
		return r, true
	case "enter":
		return r.applyPaletteSelection(), true
	}

	return r, false
}

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

func (r RichTextarea) handleEditorKey(msg tea.KeyMsg) (RichTextarea, bool, tea.Cmd) {
	extendSelection := false
	switch msg.Type {
	case tea.KeyShiftLeft, tea.KeyShiftRight, tea.KeyShiftUp, tea.KeyShiftDown:
		extendSelection = true
	}

	switch msg.String() {
	case "alt+d":
		if !r.selection.Collapsed() {
			r.deleteSelection()
			return r, true, nil
		}
		end := r.document.MoveWordRight(r.position)
		r.position = r.document.Delete(richtext.Selection{Anchor: r.position, Head: end})
		r.selection = richtext.Selection{Anchor: r.position, Head: r.position}
		return r.ensureViewport(), true, nil
	case "ctrl+left":
		r.moveCursor(r.document.MoveWordLeft(r.position), extendSelection)
		return r, true, nil
	case "ctrl+right", "alt+f":
		r.moveCursor(r.document.MoveWordRight(r.position), extendSelection)
		return r, true, nil
	case "left", "shift+left":
		r.moveCursor(r.document.MoveLeft(r.position), extendSelection)
		return r, true, nil
	case "right", "shift+right":
		r.moveCursor(r.document.MoveRight(r.position), extendSelection)
		return r, true, nil
	case "home":
		r.moveCursor(r.document.MoveLineStart(r.position), extendSelection)
		return r, true, nil
	case "end":
		r.moveCursor(r.document.MoveLineEnd(r.position), extendSelection)
		return r, true, nil
	case "up", "shift+up":
		r.moveCursor(r.moveVertical(-1), extendSelection)
		return r, true, nil
	case "down", "shift+down":
		r.moveCursor(r.moveVertical(1), extendSelection)
		return r, true, nil
	case "ctrl+a":
		r.moveCursor(r.document.MoveLineStart(r.position), false)
		return r, true, nil
	case "ctrl+e":
		r.moveCursor(r.document.MoveLineEnd(r.position), false)
		return r, true, nil
	case "ctrl+k":
		if !r.selection.Collapsed() {
			r.deleteSelection()
			return r, true, nil
		}
		end := richtext.Position{
			Line:    r.position.Line,
			Cluster: r.document.LineClusterCount(r.position.Line),
		}
		r.position = r.document.Delete(richtext.Selection{Anchor: r.position, Head: end})
		r.selection = richtext.Selection{Anchor: r.position, Head: r.position}
		return r.ensureViewport(), true, nil
	case "ctrl+t":
		return r.transposeChars(), true, nil
	case "ctrl+w", "alt+backspace":
		if !r.selection.Collapsed() {
			r.deleteSelection()
			return r, true, nil
		}
		start := r.document.MoveWordLeft(r.position)
		r.position = r.document.Delete(richtext.Selection{Anchor: start, Head: r.position})
		r.selection = richtext.Selection{Anchor: r.position, Head: r.position}
		return r.ensureViewport(), true, nil
	case "backspace":
		if !r.selection.Collapsed() {
			r.deleteSelection()
			return r, true, nil
		}
		start := r.document.MoveLeft(r.position)
		r.position = r.document.Delete(richtext.Selection{Anchor: start, Head: r.position})
		r.selection = richtext.Selection{Anchor: r.position, Head: r.position}
		return r.ensureViewport(), true, nil
	case "delete":
		if !r.selection.Collapsed() {
			r.deleteSelection()
			return r, true, nil
		}
		end := r.document.MoveRight(r.position)
		r.position = r.document.Delete(richtext.Selection{Anchor: r.position, Head: end})
		r.selection = richtext.Selection{Anchor: r.position, Head: r.position}
		return r.ensureViewport(), true, nil
	case "enter":
		if r.config.SingleLine {
			return r, false, nil
		}
		r.insertText("\n")
		return r, true, nil
	}

	if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
		r.insertText(string(msg.Runes))
		return r, true, nil
	}

	return r, false, nil
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
		if time.Since(r.lastClickAt) < 500*time.Millisecond && position == r.lastClickPos {
			r.selection = r.wordSelection(position)
			r.position = r.selection.Head
			r.lastClickAt = time.Time{}
			return r.ensureViewport(), true
		}

		r.mouseSelecting = true
		r.position = position
		r.selection = richtext.Selection{Anchor: r.position, Head: r.position}
		r.lastClickAt = time.Now()
		r.lastClickPos = position
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

func (r *RichTextarea) insertText(text string) {
	if r.config.SingleLine {
		text = flattenNewlines(text)
	}

	if !r.selection.Collapsed() {
		r.position = r.document.ReplaceText(r.selection, text, r.pending)
	} else {
		r.position = r.document.ReplaceText(richtext.Selection{Anchor: r.position, Head: r.position}, text, r.pending)
	}
	r.selection = richtext.Selection{Anchor: r.position, Head: r.position}
	*r = r.ensureViewport()
}

// flattenNewlines collapses CR/LF runs into single spaces so a paste
// containing line breaks lands as one visible line. Carriage returns
// are dropped outright; line feeds become a single space each.
func flattenNewlines(text string) string {
	if !strings.ContainsAny(text, "\r\n") {
		return text
	}

	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch r {
		case '\r':
			continue
		case '\n':
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}

	return b.String()
}

// transposeChars swaps the two grapheme clusters immediately around
// the cursor and advances the cursor by one cluster, matching
// readline `transpose-chars`. With the cursor at end-of-line, swap
// the last two clusters and leave the cursor at the end. With fewer
// than two clusters left of the cursor, the buffer is unchanged.
func (r RichTextarea) transposeChars() RichTextarea {
	line := r.position.Line
	clusters := r.document.LineClusters(line)
	if len(clusters) < 2 {
		return r
	}

	end := min(r.position.Cluster, len(clusters))
	if end < 2 {
		return r
	}

	first := clusters[end-2]
	second := clusters[end-1]

	replacement := richtext.NewDocumentFromLines([]richtext.Line{{
		Spans: []richtext.Span{
			{Text: second.Text, Attrs: second.Attrs},
			{Text: first.Text, Attrs: first.Attrs},
		},
	}})

	selection := richtext.Selection{
		Anchor: richtext.Position{Line: line, Cluster: end - 2},
		Head:   richtext.Position{Line: line, Cluster: end},
	}
	r.position = r.document.Replace(selection, replacement)
	r.selection = richtext.Selection{Anchor: r.position, Head: r.position}

	return r.ensureViewport()
}

func (r *RichTextarea) deleteSelection() {
	r.position = r.document.Delete(r.selection)
	r.selection = richtext.Selection{Anchor: r.position, Head: r.position}
	*r = r.ensureViewport()
}

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

func (r RichTextarea) renderPalette(width int) string {
	swatches := make([]string, 0, 17)
	for index := 0; index <= 16; index++ {
		label := "df"
		style := lipgloss.NewStyle()
		if index > 0 {
			colour := uint8(index - 1)
			label = fmt.Sprintf("%02d", colour)
			if r.palette.target == colourTargetForeground {
				style = style.Foreground(lipgloss.ANSIColor(colour))
			} else {
				style = style.Background(lipgloss.ANSIColor(colour))
			}
		}
		if index == r.palette.index {
			style = style.Reverse(true).Bold(true)
		}

		swatches = append(swatches, style.Render(label))
	}

	target := "fg"
	if r.palette.target == colourTargetBackground {
		target = "bg"
	}

	return theme.Dim.Width(width).Render(target + ": " + strings.Join(swatches, " "))
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

	return r.renderPalette(width)
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

func (r RichTextarea) cursorCellX(position richtext.Position) int {
	position = r.document.ClampPosition(position)
	width := 0
	for _, cluster := range r.document.LineClusters(position.Line)[:position.Cluster] {
		width += cluster.Width
	}

	return width
}

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
		style = style.Foreground(lipgloss.ANSIColor(*attrs.FG))
	}
	if attrs.BG != nil {
		style = style.Background(lipgloss.ANSIColor(*attrs.BG))
	}

	return style
}

func paletteColour(index int) *uint8 {
	if index == 0 {
		return nil
	}

	colour := uint8(min(index-1, 255)) //nolint:gosec // index bounded by palette size

	return &colour
}

func (r RichTextarea) applyPaletteSelection() RichTextarea {
	colour := paletteColour(r.palette.index)
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

func (r RichTextarea) handlePaletteMouse(msg tea.MouseMsg) (RichTextarea, bool) {
	if !r.palette.open {
		return r, false
	}

	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if msg.Action == tea.MouseActionPress && r.palette.index > 0 {
			r.palette.index--
			return r, true
		}
	case tea.MouseButtonWheelDown:
		if msg.Action == tea.MouseActionPress && r.palette.index < 16 {
			r.palette.index++
			return r, true
		}
	case tea.MouseButtonLeft:
		if msg.Action != tea.MouseActionPress && msg.Action != tea.MouseActionMotion {
			return r, false
		}

		if msg.X < 4 {
			if msg.Action == tea.MouseActionPress {
				if r.palette.target == colourTargetForeground {
					r.palette.target = colourTargetBackground
				} else {
					r.palette.target = colourTargetForeground
				}
			}
			return r, true
		}

		index := (msg.X - 4) / 3
		if index < 0 || index > 16 {
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

func boolToInt(value bool) int {
	if value {
		return 1
	}

	return 0
}
