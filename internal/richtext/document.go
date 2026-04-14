// Package richtext provides a styled document model for IRC-formatted text.
package richtext

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

// Attrs describes the styling applied to a span of text.
type Attrs struct {
	Bold      bool
	Italic    bool
	Underline bool
	Reverse   bool
	Strike    bool
	FG        *uint8
	BG        *uint8
}

// Equals reports whether the two attribute sets are identical.
func (a Attrs) Equals(other Attrs) bool {
	return a.Bold == other.Bold &&
		a.Italic == other.Italic &&
		a.Underline == other.Underline &&
		a.Reverse == other.Reverse &&
		a.Strike == other.Strike &&
		equalColour(a.FG, other.FG) &&
		equalColour(a.BG, other.BG)
}

// Reset reports whether the attributes carry no styling.
func (a Attrs) Reset() bool {
	return a.Equals(Attrs{})
}

// Span is a run of text with a shared style.
type Span struct {
	Text  string
	Attrs Attrs
}

// Line is a logical line in the document.
type Line struct {
	Spans []Span
}

// Document is a line-oriented rich text document.
type Document struct {
	lines  []Line
	caches []lineCache
}

// Position identifies a grapheme-cluster position in the document.
type Position struct {
	Line    int
	Cluster int
}

// Selection identifies a range in the document.
type Selection struct {
	Anchor Position
	Head   Position
}

// Grapheme is a rendered grapheme cluster in a line cache.
type Grapheme struct {
	Text              string
	Attrs             Attrs
	Width             int
	WordBoundaryAfter bool
	LineBreakAfter    int
	RuneCount         int
}

type lineCache struct {
	valid     bool
	graphemes []Grapheme
	plain     string
	width     int
}

// NewDocument returns an empty document with a single empty line.
func NewDocument() Document {
	doc := Document{
		lines: []Line{{}},
	}
	doc.invalidateAll()

	return doc
}

// NewDocumentFromLines builds a document from the supplied logical lines.
func NewDocumentFromLines(lines []Line) Document {
	doc := Document{
		lines: cloneLines(lines),
	}
	doc.invalidateAll()

	return doc
}

// Plain returns the document as plain text.
func (d *Document) Plain() string {
	d.ensureNormalised()

	lines := make([]string, 0, len(d.lines))
	for lineIndex := range d.lines {
		lines = append(lines, d.LinePlain(lineIndex))
	}

	return strings.Join(lines, "\n")
}

// Clone returns a deep copy of the document.
func (d *Document) Clone() Document {
	d.ensureNormalised()

	clone := Document{
		lines: make([]Line, 0, len(d.lines)),
	}

	for _, line := range d.lines {
		spans := make([]Span, 0, len(line.Spans))
		for _, span := range line.Spans {
			spans = append(spans, Span{
				Text:  span.Text,
				Attrs: cloneAttrs(span.Attrs),
			})
		}

		clone.lines = append(clone.lines, Line{Spans: spans})
	}

	clone.invalidateAll()

	return clone
}

// NewDocumentFromText builds a document from plain text and the supplied attrs.
func NewDocumentFromText(text string, attrs Attrs) Document {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 {
		return NewDocument()
	}

	doc := Document{
		lines: make([]Line, 0, len(lines)),
	}

	for _, line := range lines {
		if line == "" {
			doc.lines = append(doc.lines, Line{})
			continue
		}

		doc.lines = append(doc.lines, Line{
			Spans: []Span{{
				Text:  line,
				Attrs: cloneAttrs(attrs),
			}},
		})
	}

	doc.invalidateAll()

	return doc
}

// Collapsed reports whether the selection is empty.
func (s Selection) Collapsed() bool {
	return s.Anchor == s.Head
}

// Normalized returns the range endpoints in document order.
func (s Selection) Normalized() (Position, Position) {
	if lessPosition(s.Head, s.Anchor) {
		return s.Head, s.Anchor
	}

	return s.Anchor, s.Head
}

// ClampPosition constrains the given position to the document.
func (d *Document) ClampPosition(pos Position) Position {
	d.ensureNormalised()

	if pos.Line < 0 {
		pos.Line = 0
	}
	if pos.Line >= len(d.lines) {
		pos.Line = len(d.lines) - 1
	}

	clusterCount := d.LineClusterCount(pos.Line)
	if pos.Cluster < 0 {
		pos.Cluster = 0
	}
	if pos.Cluster > clusterCount {
		pos.Cluster = clusterCount
	}

	return pos
}

// LineCount reports the number of logical lines in the document.
func (d *Document) LineCount() int {
	d.ensureNormalised()

	return len(d.lines)
}

// Line returns a copy of the given logical line.
func (d *Document) Line(line int) Line {
	d.ensureNormalised()

	if line < 0 {
		line = 0
	}
	if line >= len(d.lines) {
		line = len(d.lines) - 1
	}

	return cloneLine(d.lines[line])
}

// LinePlain returns the plain text for the given line.
func (d *Document) LinePlain(line int) string {
	return d.lineCache(line).plain
}

// LineClusters returns the grapheme clusters for the given line.
func (d *Document) LineClusters(line int) []Grapheme {
	cache := d.lineCache(line)
	graphemes := make([]Grapheme, len(cache.graphemes))
	copy(graphemes, cache.graphemes)

	return graphemes
}

// LineClusterCount reports the number of grapheme clusters in the line.
func (d *Document) LineClusterCount(line int) int {
	return len(d.lineCache(line).graphemes)
}

// LineDisplayWidth reports the rendered width of the line.
func (d *Document) LineDisplayWidth(line int) int {
	return d.lineCache(line).width
}

// PositionFromRuneIndex converts a rune offset within the given line to a cluster position.
func (d *Document) PositionFromRuneIndex(line, runeIndex int) Position {
	pos := d.ClampPosition(Position{Line: line})
	if runeIndex <= 0 {
		return pos
	}

	count := 0
	for index, grapheme := range d.lineCache(pos.Line).graphemes {
		count += grapheme.RuneCount
		if runeIndex < count {
			pos.Cluster = index
			return pos
		}
		if runeIndex == count {
			pos.Cluster = index + 1
			return pos
		}
	}

	pos.Cluster = d.LineClusterCount(pos.Line)

	return pos
}

// RuneIndex converts a cluster position into a rune index within its line.
func (d *Document) RuneIndex(pos Position) int {
	pos = d.ClampPosition(pos)
	count := 0
	for _, grapheme := range d.lineCache(pos.Line).graphemes[:pos.Cluster] {
		count += grapheme.RuneCount
	}

	return count
}

// AttrsBefore returns the attrs that should be used when inserting at the supplied position.
func (d *Document) AttrsBefore(pos Position) Attrs {
	pos = d.ClampPosition(pos)
	cache := d.lineCache(pos.Line)

	if pos.Cluster > 0 {
		return cloneAttrs(cache.graphemes[pos.Cluster-1].Attrs)
	}
	if len(cache.graphemes) > 0 {
		return cloneAttrs(cache.graphemes[0].Attrs)
	}

	return Attrs{}
}

// ReplaceText replaces the given range with plain text using the supplied attrs.
func (d *Document) ReplaceText(selection Selection, text string, attrs Attrs) Position {
	return d.Replace(selection, NewDocumentFromText(text, attrs))
}

// Replace replaces the given range with the replacement document.
func (d *Document) Replace(selection Selection, replacement Document) Position {
	d.ensureNormalised()
	replacement.ensureNormalised()

	start, end := selection.Normalized()
	start = d.ClampPosition(start)
	end = d.ClampPosition(end)

	prefix := d.LineClusters(start.Line)[:start.Cluster]
	suffix := d.LineClusters(end.Line)[end.Cluster:]
	replacementLines := replacement.clusterLines()

	newLines := make([]Line, 0, len(d.lines)-(end.Line-start.Line)+len(replacementLines))
	newLines = append(newLines, d.lines[:start.Line]...)

	switch len(replacementLines) {
	case 0:
		newLines = append(newLines, clustersToLine(appendClusterSlices(prefix, suffix)))
	default:
		firstLine := appendClusterSlices(prefix, replacementLines[0])
		lastIndex := len(replacementLines) - 1
		lastLine := appendClusterSlices(replacementLines[lastIndex], suffix)

		if len(replacementLines) == 1 {
			newLines = append(newLines, clustersToLine(appendClusterSlices(prefix, appendClusterSlices(replacementLines[0], suffix))))
		} else {
			newLines = append(newLines, clustersToLine(firstLine))
			for _, line := range replacementLines[1:lastIndex] {
				newLines = append(newLines, clustersToLine(line))
			}
			newLines = append(newLines, clustersToLine(lastLine))
		}
	}

	newLines = append(newLines, d.lines[end.Line+1:]...)
	d.lines = newLines
	d.invalidateAll()
	d.ensureNormalised()

	if len(replacementLines) == 0 {
		return start
	}

	if len(replacementLines) == 1 {
		return Position{
			Line:    start.Line,
			Cluster: len(prefix) + len(replacementLines[0]),
		}
	}

	return Position{
		Line:    start.Line + len(replacementLines) - 1,
		Cluster: len(replacementLines[len(replacementLines)-1]),
	}
}

// Delete removes the selected range and returns the resulting cursor position.
func (d *Document) Delete(selection Selection) Position {
	return d.Replace(selection, NewDocument())
}

// UpdateAttrs applies the attr mapper to every cluster within the selected range.
func (d *Document) UpdateAttrs(selection Selection, fn func(Attrs) Attrs) {
	d.ensureNormalised()

	start, end := selection.Normalized()
	start = d.ClampPosition(start)
	end = d.ClampPosition(end)

	if start == end {
		return
	}

	lines := d.clusterLines()

	for lineIndex := start.Line; lineIndex <= end.Line; lineIndex++ {
		from := 0
		if lineIndex == start.Line {
			from = start.Cluster
		}

		to := len(lines[lineIndex])
		if lineIndex == end.Line {
			to = end.Cluster
		}

		for clusterIndex := from; clusterIndex < to; clusterIndex++ {
			lines[lineIndex][clusterIndex].Attrs = cloneAttrs(fn(lines[lineIndex][clusterIndex].Attrs))
		}
	}

	d.lines = make([]Line, 0, len(lines))
	for _, line := range lines {
		d.lines = append(d.lines, clustersToLine(line))
	}
	d.invalidateAll()
	d.ensureNormalised()
}

// MoveLeft moves left by one grapheme cluster.
func (d *Document) MoveLeft(pos Position) Position {
	pos = d.ClampPosition(pos)
	if pos.Cluster > 0 {
		pos.Cluster--
		return pos
	}

	if pos.Line == 0 {
		return pos
	}

	pos.Line--
	pos.Cluster = d.LineClusterCount(pos.Line)

	return pos
}

// MoveRight moves right by one grapheme cluster.
func (d *Document) MoveRight(pos Position) Position {
	pos = d.ClampPosition(pos)
	if pos.Cluster < d.LineClusterCount(pos.Line) {
		pos.Cluster++
		return pos
	}

	if pos.Line == len(d.lines)-1 {
		return pos
	}

	pos.Line++
	pos.Cluster = 0

	return pos
}

// MoveLineStart moves to the start of the current line.
func (d *Document) MoveLineStart(pos Position) Position {
	pos = d.ClampPosition(pos)
	pos.Cluster = 0

	return pos
}

// MoveLineEnd moves to the end of the current line.
func (d *Document) MoveLineEnd(pos Position) Position {
	pos = d.ClampPosition(pos)
	pos.Cluster = d.LineClusterCount(pos.Line)

	return pos
}

// MoveWordRight moves to the next Unicode word boundary.
func (d *Document) MoveWordRight(pos Position) Position {
	pos = d.ClampPosition(pos)

	for {
		cache := d.lineCache(pos.Line)
		for _, segment := range wordSegments(cache.graphemes) {
			if segment.end <= pos.Cluster {
				continue
			}
			if !segment.wordLike {
				continue
			}

			return Position{Line: pos.Line, Cluster: segment.end}
		}

		if pos.Line == len(d.lines)-1 {
			return Position{Line: pos.Line, Cluster: len(cache.graphemes)}
		}

		pos.Line++
		pos.Cluster = 0

		if d.LineClusterCount(pos.Line) == 0 {
			return pos
		}
	}
}

// MoveWordLeft moves to the previous Unicode word boundary.
func (d *Document) MoveWordLeft(pos Position) Position {
	pos = d.ClampPosition(pos)

	for {
		cache := d.lineCache(pos.Line)
		segments := wordSegments(cache.graphemes)
		for index := len(segments) - 1; index >= 0; index-- {
			segment := segments[index]
			if segment.start >= pos.Cluster {
				continue
			}
			if !segment.wordLike {
				continue
			}

			return Position{Line: pos.Line, Cluster: segment.start}
		}

		if pos.Line == 0 {
			return Position{}
		}

		pos.Line--
		pos.Cluster = d.LineClusterCount(pos.Line)
		if pos.Cluster == 0 {
			return pos
		}
	}
}

func (d *Document) ensureNormalised() {
	if len(d.lines) == 0 {
		d.lines = []Line{{}}
	}

	if len(d.caches) != len(d.lines) {
		d.caches = make([]lineCache, len(d.lines))
	}

	for index := range d.lines {
		d.lines[index] = normaliseLine(d.lines[index])
	}
}

func (d *Document) invalidateAll() {
	d.caches = make([]lineCache, len(d.lines))
}

func (d *Document) lineCache(line int) lineCache {
	d.ensureNormalised()

	if line < 0 {
		line = 0
	}
	if line >= len(d.lines) {
		line = len(d.lines) - 1
	}

	cache := d.caches[line]
	if !cache.valid {
		cache = buildLineCache(d.lines[line])
		d.caches[line] = cache
	}

	return cache
}

func buildLineCache(line Line) lineCache {
	var (
		cache = lineCache{valid: true}
		state = -1
	)

	for _, span := range line.Spans {
		rest := span.Text
		for rest != "" {
			clusterText, next, boundaries, nextState := uniseg.StepString(rest, state)
			if clusterText == "" {
				break
			}

			grapheme := Grapheme{
				Text:              clusterText,
				Attrs:             cloneAttrs(span.Attrs),
				Width:             boundaries >> uniseg.ShiftWidth,
				WordBoundaryAfter: boundaries&uniseg.MaskWord != 0,
				LineBreakAfter:    boundaries & uniseg.MaskLine,
				RuneCount:         utf8.RuneCountInString(clusterText),
			}

			cache.graphemes = append(cache.graphemes, grapheme)
			cache.plain += clusterText
			cache.width += grapheme.Width

			rest = next
			state = nextState
		}
	}

	return cache
}

func (d *Document) clusterLines() [][]Grapheme {
	d.ensureNormalised()

	lines := make([][]Grapheme, 0, len(d.lines))
	for lineIndex := range d.lines {
		lines = append(lines, d.LineClusters(lineIndex))
	}

	return lines
}

func clustersToLine(clusters []Grapheme) Line {
	line := Line{}
	if len(clusters) == 0 {
		return line
	}

	var current Span
	for index, cluster := range clusters {
		if index == 0 || !current.Attrs.Equals(cluster.Attrs) {
			if index > 0 {
				line.Spans = append(line.Spans, current)
			}

			current = Span{
				Text:  cluster.Text,
				Attrs: cloneAttrs(cluster.Attrs),
			}
			continue
		}

		current.Text += cluster.Text
	}

	line.Spans = append(line.Spans, current)

	return line
}

func normaliseLine(line Line) Line {
	spans := make([]Span, 0, len(line.Spans))
	for _, span := range line.Spans {
		if span.Text == "" {
			continue
		}

		if len(spans) > 0 && spans[len(spans)-1].Attrs.Equals(span.Attrs) {
			spans[len(spans)-1].Text += span.Text
			continue
		}

		spans = append(spans, Span{
			Text:  span.Text,
			Attrs: cloneAttrs(span.Attrs),
		})
	}

	line.Spans = spans

	return line
}

func appendClusterSlices(parts ...[]Grapheme) []Grapheme {
	total := 0
	for _, part := range parts {
		total += len(part)
	}

	result := make([]Grapheme, 0, total)
	for _, part := range parts {
		for _, cluster := range part {
			result = append(result, Grapheme{
				Text:              cluster.Text,
				Attrs:             cloneAttrs(cluster.Attrs),
				Width:             cluster.Width,
				WordBoundaryAfter: cluster.WordBoundaryAfter,
				LineBreakAfter:    cluster.LineBreakAfter,
				RuneCount:         cluster.RuneCount,
			})
		}
	}

	return result
}

func lessPosition(left, right Position) bool {
	if left.Line != right.Line {
		return left.Line < right.Line
	}

	return left.Cluster < right.Cluster
}

func cloneAttrs(attrs Attrs) Attrs {
	return Attrs{
		Bold:      attrs.Bold,
		Italic:    attrs.Italic,
		Underline: attrs.Underline,
		Reverse:   attrs.Reverse,
		Strike:    attrs.Strike,
		FG:        cloneColour(attrs.FG),
		BG:        cloneColour(attrs.BG),
	}
}

func cloneLine(line Line) Line {
	return Line{Spans: cloneSpans(line.Spans)}
}

func cloneLines(lines []Line) []Line {
	clones := make([]Line, 0, len(lines))
	for _, line := range lines {
		clones = append(clones, cloneLine(line))
	}

	return clones
}

func cloneSpans(spans []Span) []Span {
	clones := make([]Span, 0, len(spans))
	for _, span := range spans {
		clones = append(clones, Span{
			Text:  span.Text,
			Attrs: cloneAttrs(span.Attrs),
		})
	}

	return clones
}

func cloneColour(colour *uint8) *uint8 {
	if colour == nil {
		return nil
	}

	value := *colour

	return &value
}

func equalColour(left, right *uint8) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}

	return *left == *right
}

func isWordGrapheme(text string) bool {
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) {
			return true
		}

		if r == '_' {
			return true
		}
	}

	return false
}

type wordSegment struct {
	start    int
	end      int
	wordLike bool
}

func wordSegments(graphemes []Grapheme) []wordSegment {
	if len(graphemes) == 0 {
		return nil
	}

	segments := make([]wordSegment, 0, len(graphemes))
	start := 0
	wordLike := false
	for index, grapheme := range graphemes {
		wordLike = wordLike || isWordGrapheme(grapheme.Text)
		if !grapheme.WordBoundaryAfter && index < len(graphemes)-1 {
			continue
		}

		segments = append(segments, wordSegment{
			start:    start,
			end:      index + 1,
			wordLike: wordLike,
		})
		start = index + 1
		wordLike = false
	}

	if start < len(graphemes) {
		segments = append(segments, wordSegment{
			start:    start,
			end:      len(graphemes),
			wordLike: wordLike,
		})
	}

	return segments
}
