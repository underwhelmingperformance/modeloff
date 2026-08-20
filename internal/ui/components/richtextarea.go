package components

import (
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/richtext"
	"github.com/laney/modeloff/internal/ui"
)

// RichTextareaConfig configures the editor widget.
type RichTextareaConfig struct {
	SingleLine           bool
	Wrap                 bool
	AllowFormatting      bool
	ShowFormattingStatus bool
}

// RichTextarea is a grapheme-aware rich text editor widget. It holds
// the document, the cursor and the selection over it, the viewport
// offsets the view scrolls by, and three pieces of state with a life
// of their own: the colour palette, the kill ring and the multi-click
// counter.
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
	clicks         clickTracker

	kills killRing
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

// Update implements ui.Model. A key is offered to the palette, then
// to the formatting bindings, then to the editor proper; anything none
// of them takes goes to the cursor model, which is what keeps the
// blink running.
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

// SelectedText returns the plain text of the current selection, or
// an empty string when nothing is selected.
func (r RichTextarea) SelectedText() string {
	return r.selectionText(r.selection)
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

// selectionText returns the plain text covered by the selection. It
// spans line breaks with `\n`.
func (r RichTextarea) selectionText(selection richtext.Selection) string {
	start, end := selection.Normalized()
	start = r.document.ClampPosition(start)
	end = r.document.ClampPosition(end)
	if start == end {
		return ""
	}

	if start.Line == end.Line {
		return clustersText(r.document.LineClusters(start.Line)[start.Cluster:end.Cluster])
	}

	parts := []string{clustersText(r.document.LineClusters(start.Line)[start.Cluster:])}
	for line := start.Line + 1; line < end.Line; line++ {
		parts = append(parts, r.document.LinePlain(line))
	}
	parts = append(parts, clustersText(r.document.LineClusters(end.Line)[:end.Cluster]))

	return strings.Join(parts, "\n")
}

func clustersText(clusters []richtext.Grapheme) string {
	var b strings.Builder
	for _, cluster := range clusters {
		b.WriteString(cluster.Text)
	}

	return b.String()
}
