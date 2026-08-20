package richtext_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/richtext"
)

func colour(index uint8) *uint8 {
	value := index

	return &value
}

func TestSelectionHelpers(t *testing.T) {
	selection := richtext.Selection{
		Anchor: richtext.Position{Line: 2, Cluster: 4},
		Head:   richtext.Position{Line: 1, Cluster: 3},
	}

	start, end := selection.Normalized()

	require.False(t, selection.Collapsed())
	require.Equal(t, richtext.Position{Line: 1, Cluster: 3}, start)
	require.Equal(t, richtext.Position{Line: 2, Cluster: 4}, end)
	require.True(t, (richtext.Selection{}).Collapsed())
}

func TestDocumentCloneAndNormalisation(t *testing.T) {
	doc := richtext.NewDocumentFromLines([]richtext.Line{{
		Spans: []richtext.Span{
			{Text: "he", Attrs: richtext.Attrs{Underline: true}},
			{Text: "llo", Attrs: richtext.Attrs{Underline: true}},
			{Text: "", Attrs: richtext.Attrs{Bold: true}},
		},
	}})

	clone := doc.Clone()
	clone.UpdateAttrs(richtext.Selection{
		Anchor: richtext.Position{Line: 0, Cluster: 0},
		Head:   richtext.Position{Line: 0, Cluster: 5},
	}, func(attrs richtext.Attrs) richtext.Attrs {
		attrs.Bold = true
		return attrs
	})

	require.Equal(t, "hello", doc.Plain())
	require.Equal(t, []richtext.Span{{
		Text:  "hello",
		Attrs: richtext.Attrs{Underline: true},
	}}, doc.Line(0).Spans)
	require.Equal(t, []richtext.Span{{
		Text:  "hello",
		Attrs: richtext.Attrs{Bold: true, Underline: true},
	}}, clone.Line(0).Spans)
}

func TestDocumentClampPositionAndLinePlain(t *testing.T) {
	doc := richtext.NewDocumentFromText("hello\nworld", richtext.Attrs{})

	require.Equal(t, "hello", doc.LinePlain(0))
	require.Equal(t, "world", doc.LinePlain(1))
	require.Equal(t, richtext.Position{}, doc.ClampPosition(richtext.Position{Line: -1, Cluster: -2}))
	require.Equal(t, richtext.Position{Line: 1, Cluster: 5}, doc.ClampPosition(richtext.Position{Line: 9, Cluster: 99}))
}

func TestDocumentReplaceTextAcrossLines(t *testing.T) {
	doc := richtext.NewDocumentFromText("hello\nworld", richtext.Attrs{})

	cursor := doc.ReplaceText(richtext.Selection{
		Anchor: richtext.Position{Line: 0, Cluster: 2},
		Head:   richtext.Position{Line: 1, Cluster: 3},
	}, "lp\nW", richtext.Attrs{Bold: true})

	require.Equal(t, richtext.Position{Line: 1, Cluster: 1}, cursor)
	require.Equal(t, "help\nWld", doc.Plain())
	require.Equal(t, []richtext.Line{
		{
			Spans: []richtext.Span{
				{Text: "he", Attrs: richtext.Attrs{}},
				{Text: "lp", Attrs: richtext.Attrs{Bold: true}},
			},
		},
		{
			Spans: []richtext.Span{
				{Text: "W", Attrs: richtext.Attrs{Bold: true}},
				{Text: "ld", Attrs: richtext.Attrs{}},
			},
		},
	}, []richtext.Line{doc.Line(0), doc.Line(1)})
}

func TestDocumentReplaceWithDocument(t *testing.T) {
	doc := richtext.NewDocumentFromText("alpha\nbeta", richtext.Attrs{})
	replacement := richtext.NewDocumentFromLines([]richtext.Line{
		{
			Spans: []richtext.Span{
				{Text: "X", Attrs: richtext.Attrs{Italic: true}},
				{Text: "Y", Attrs: richtext.Attrs{Underline: true}},
			},
		},
		{
			Spans: []richtext.Span{
				{Text: "Z", Attrs: richtext.Attrs{FG: colour(4)}},
			},
		},
	})

	cursor := doc.Replace(richtext.Selection{
		Anchor: richtext.Position{Line: 0, Cluster: 2},
		Head:   richtext.Position{Line: 1, Cluster: 2},
	}, replacement)

	require.Equal(t, richtext.Position{Line: 1, Cluster: 1}, cursor)
	require.Equal(t, "alXY\nZta", doc.Plain())
	require.Equal(t, []richtext.Line{
		{
			Spans: []richtext.Span{
				{Text: "al", Attrs: richtext.Attrs{}},
				{Text: "X", Attrs: richtext.Attrs{Italic: true}},
				{Text: "Y", Attrs: richtext.Attrs{Underline: true}},
			},
		},
		{
			Spans: []richtext.Span{
				{Text: "Z", Attrs: richtext.Attrs{FG: colour(4)}},
				{Text: "ta", Attrs: richtext.Attrs{}},
			},
		},
	}, []richtext.Line{doc.Line(0), doc.Line(1)})
}

func TestDocumentUpdateAttrsOnRange(t *testing.T) {
	doc := richtext.NewDocumentFromText("abcdef", richtext.Attrs{})

	doc.UpdateAttrs(richtext.Selection{
		Anchor: richtext.Position{Line: 0, Cluster: 1},
		Head:   richtext.Position{Line: 0, Cluster: 4},
	}, func(attrs richtext.Attrs) richtext.Attrs {
		attrs.Strike = true
		attrs.FG = colour(4)
		return attrs
	})

	require.Equal(t, []richtext.Span{
		{Text: "a", Attrs: richtext.Attrs{}},
		{Text: "bcd", Attrs: richtext.Attrs{Strike: true, FG: colour(4)}},
		{Text: "ef", Attrs: richtext.Attrs{}},
	}, doc.Line(0).Spans)
}

func TestDocumentCountsGraphemeClusters(t *testing.T) {
	doc := richtext.NewDocumentFromText("a🏳️‍🌈e\u0301界", richtext.Attrs{})

	require.Equal(t, 4, doc.LineClusterCount(0))
	require.Equal(t, 6, doc.LineDisplayWidth(0))
	require.Equal(t, richtext.Position{Line: 0, Cluster: 2}, doc.PositionFromRuneIndex(0, 5))
	require.Equal(t, 5, doc.RuneIndex(richtext.Position{Line: 0, Cluster: 2}))
}

func TestDocumentMoveAcrossLineBoundaries(t *testing.T) {
	doc := richtext.NewDocumentFromText("ab\ncd", richtext.Attrs{})

	require.Equal(t, richtext.Position{Line: 0, Cluster: 1}, doc.MoveRight(richtext.Position{}))
	require.Equal(t, richtext.Position{Line: 1, Cluster: 0}, doc.MoveRight(richtext.Position{Line: 0, Cluster: 2}))
	require.Equal(t, richtext.Position{Line: 0, Cluster: 2}, doc.MoveLeft(richtext.Position{Line: 1, Cluster: 0}))
	require.Equal(t, richtext.Position{Line: 1, Cluster: 2}, doc.MoveLineEnd(richtext.Position{Line: 1, Cluster: 0}))
	require.Equal(t, richtext.Position{Line: 1, Cluster: 0}, doc.MoveLineStart(richtext.Position{Line: 1, Cluster: 2}))
}

func TestDocumentWordMovementUsesUnicodeBoundaries(t *testing.T) {
	doc := richtext.NewDocumentFromText("one two  ثلاثة", richtext.Attrs{})

	require.Equal(t, richtext.Position{Line: 0, Cluster: 3}, doc.MoveWordRight(richtext.Position{}))
	require.Equal(t, richtext.Position{Line: 0, Cluster: 7}, doc.MoveWordRight(richtext.Position{Line: 0, Cluster: 3}))
	require.Equal(t, richtext.Position{Line: 0, Cluster: 9}, doc.MoveWordLeft(richtext.Position{Line: 0, Cluster: doc.LineClusterCount(0)}))
}

func TestDocumentAttrsBefore(t *testing.T) {
	doc := richtext.NewDocumentFromLines([]richtext.Line{{
		Spans: []richtext.Span{
			{Text: "ab", Attrs: richtext.Attrs{Bold: true}},
			{Text: "cd", Attrs: richtext.Attrs{Underline: true}},
		},
	}})

	require.Equal(t, richtext.Attrs{Bold: true}, doc.AttrsBefore(richtext.Position{Line: 0, Cluster: 1}))
	require.Equal(t, richtext.Attrs{Bold: true}, doc.AttrsBefore(richtext.Position{Line: 0, Cluster: 0}))
	require.Equal(t, richtext.Attrs{Underline: true}, doc.AttrsBefore(richtext.Position{Line: 0, Cluster: 4}))
}

func TestDocumentGraphemeClustersSpanningAttributeBoundary(t *testing.T) {
	tests := []struct {
		name          string
		firstSpan     string
		secondSpan    string
		wantClusters  int
		wantFirstText string
	}{
		{
			name:          "combining mark after a formatting toggle",
			firstSpan:     "e",
			secondSpan:    "\u0301",
			wantClusters:  1,
			wantFirstText: "e\u0301",
		},
		{
			name:          "ZWJ emoji sequence split across a formatting toggle",
			firstSpan:     "\U0001F468",
			secondSpan:    "\u200D\U0001F33E",
			wantClusters:  1,
			wantFirstText: "\U0001F468\u200D\U0001F33E",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := richtext.NewDocumentFromLines([]richtext.Line{{
				Spans: []richtext.Span{
					{Text: tt.firstSpan, Attrs: richtext.Attrs{Bold: true}},
					{Text: tt.secondSpan, Attrs: richtext.Attrs{Italic: true}},
				},
			}})

			clusters := doc.LineClusters(0)

			require.Equal(t, tt.wantClusters, doc.LineClusterCount(0))
			require.Equal(t, tt.wantFirstText, clusters[0].Text)
			require.Equal(t, richtext.Attrs{Bold: true}, clusters[0].Attrs)
			require.Equal(t, tt.wantFirstText, doc.LinePlain(0))
		})
	}
}

func TestDocumentDeleteCollapsesRange(t *testing.T) {
	doc := richtext.NewDocumentFromText("abc\ndef", richtext.Attrs{})

	cursor := doc.Delete(richtext.Selection{
		Anchor: richtext.Position{Line: 0, Cluster: 1},
		Head:   richtext.Position{Line: 1, Cluster: 1},
	})

	require.Equal(t, richtext.Position{Line: 0, Cluster: 1}, cursor)
	require.Equal(t, "aef", doc.Plain())
	require.Equal(t, []richtext.Line{{
		Spans: []richtext.Span{{Text: "aef", Attrs: richtext.Attrs{}}},
	}}, []richtext.Line{doc.Line(0)})
}

// TestDocumentUpdateAttrsOverMultipleLines guards the per-line rewrite in
// UpdateAttrs: only the lines within the selection should be touched, and
// each touched line's from/to cluster bounds should default correctly at
// the selection's own start and end lines. TestDocumentUpdateAttrsOnRange
// covers a single-line selection; these cases exercise a selection crossing
// two lines, a selection spanning a fully-covered middle line, and a
// selection whose bounds land exactly on both line ends.
func TestDocumentUpdateAttrsOverMultipleLines(t *testing.T) {
	updated := richtext.Attrs{Strike: true, FG: colour(4)}
	apply := func(attrs richtext.Attrs) richtext.Attrs {
		attrs.Strike = true
		attrs.FG = colour(4)
		return attrs
	}

	tests := []struct {
		name      string
		lines     string
		anchor    richtext.Position
		head      richtext.Position
		wantLines []richtext.Line
	}{
		{
			name:   "selection crosses two lines, partial at both ends",
			lines:  "abcdef\nghijkl",
			anchor: richtext.Position{Line: 0, Cluster: 3},
			head:   richtext.Position{Line: 1, Cluster: 3},
			wantLines: []richtext.Line{
				{Spans: []richtext.Span{{Text: "abc", Attrs: richtext.Attrs{}}, {Text: "def", Attrs: updated}}},
				{Spans: []richtext.Span{{Text: "ghi", Attrs: updated}, {Text: "jkl", Attrs: richtext.Attrs{}}}},
			},
		},
		{
			name:   "selection spans a fully-covered middle line and leaves the line after it untouched",
			lines:  "abcdef\nghijkl\nmnopqr\nstuvwx",
			anchor: richtext.Position{Line: 0, Cluster: 4},
			head:   richtext.Position{Line: 2, Cluster: 2},
			wantLines: []richtext.Line{
				{Spans: []richtext.Span{{Text: "abcd", Attrs: richtext.Attrs{}}, {Text: "ef", Attrs: updated}}},
				{Spans: []richtext.Span{{Text: "ghijkl", Attrs: updated}}},
				{Spans: []richtext.Span{{Text: "mn", Attrs: updated}, {Text: "opqr", Attrs: richtext.Attrs{}}}},
				{Spans: []richtext.Span{{Text: "stuvwx", Attrs: richtext.Attrs{}}}},
			},
		},
		{
			name:   "selection bounds land exactly on both line ends",
			lines:  "abc\ndef\nghi",
			anchor: richtext.Position{Line: 0, Cluster: 0},
			head:   richtext.Position{Line: 2, Cluster: 3},
			wantLines: []richtext.Line{
				{Spans: []richtext.Span{{Text: "abc", Attrs: updated}}},
				{Spans: []richtext.Span{{Text: "def", Attrs: updated}}},
				{Spans: []richtext.Span{{Text: "ghi", Attrs: updated}}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := richtext.NewDocumentFromText(tt.lines, richtext.Attrs{})

			doc.UpdateAttrs(richtext.Selection{Anchor: tt.anchor, Head: tt.head}, apply)

			gotLines := make([]richtext.Line, doc.LineCount())
			for i := range gotLines {
				gotLines[i] = doc.Line(i)
			}

			require.Equal(t, tt.wantLines, gotLines)
		})
	}
}

// TestDocumentReplaceAndDeleteAtDocumentBoundaries guards Replace and Delete
// at the exact start and end of a document, where there is no prefix
// (respectively suffix) cluster on the edited line at all.
func TestDocumentReplaceAndDeleteAtDocumentBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		initial    string
		apply      func(doc *richtext.Document) richtext.Position
		wantCursor richtext.Position
		wantPlain  string
	}{
		{
			name:    "insert at the very start of the document",
			initial: "bcd\nefg",
			apply: func(doc *richtext.Document) richtext.Position {
				pos := richtext.Position{Line: 0, Cluster: 0}
				return doc.ReplaceText(richtext.Selection{Anchor: pos, Head: pos}, "a", richtext.Attrs{Bold: true})
			},
			wantCursor: richtext.Position{Line: 0, Cluster: 1},
			wantPlain:  "abcd\nefg",
		},
		{
			name:    "insert at the very end of the document",
			initial: "bcd\nefg",
			apply: func(doc *richtext.Document) richtext.Position {
				pos := richtext.Position{Line: 1, Cluster: 3}
				return doc.ReplaceText(richtext.Selection{Anchor: pos, Head: pos}, "h", richtext.Attrs{Bold: true})
			},
			wantCursor: richtext.Position{Line: 1, Cluster: 4},
			wantPlain:  "bcd\nefgh",
		},
		{
			name:    "delete at the very start of the document",
			initial: "abcd\nefg",
			apply: func(doc *richtext.Document) richtext.Position {
				return doc.Delete(richtext.Selection{
					Anchor: richtext.Position{Line: 0, Cluster: 0},
					Head:   richtext.Position{Line: 0, Cluster: 1},
				})
			},
			wantCursor: richtext.Position{Line: 0, Cluster: 0},
			wantPlain:  "bcd\nefg",
		},
		{
			name:    "delete at the very end of the document",
			initial: "abcd\nefg",
			apply: func(doc *richtext.Document) richtext.Position {
				return doc.Delete(richtext.Selection{
					Anchor: richtext.Position{Line: 1, Cluster: 2},
					Head:   richtext.Position{Line: 1, Cluster: 3},
				})
			},
			wantCursor: richtext.Position{Line: 1, Cluster: 2},
			wantPlain:  "abcd\nef",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := richtext.NewDocumentFromText(tt.initial, richtext.Attrs{})

			cursor := tt.apply(&doc)

			require.Equal(t, tt.wantCursor, cursor)
			require.Equal(t, tt.wantPlain, doc.Plain())
		})
	}
}

// TestDocumentReplaceInMiddleLinesCarriesUntouchedPrefixAndSuffixCaches
// guards Replace's cache splice in a document of three or more lines, where
// a middle range is replaced and both the lines before start.Line and the
// lines after end.Line are non-empty, so both the prefix and the suffix of
// d.caches carry an already-built cache entry forward rather than an
// invalid zero value. The prefix and suffix lines are read once before the
// mutation specifically to warm their caches, and read again afterwards: a
// splice that misaligned d.caches against d.lines would return the wrong
// line's stale content here, not merely an unbuilt one.
func TestDocumentReplaceInMiddleLinesCarriesUntouchedPrefixAndSuffixCaches(t *testing.T) {
	tests := []struct {
		name         string
		initialLines []string
		warm         []int
		selection    richtext.Selection
		replacement  string
		wantCursor   richtext.Position
		wantLines    []string
	}{
		{
			name:         "single middle line replaced, line count unchanged",
			initialLines: []string{"aaaa", "bbbb", "cccc", "dddd"},
			warm:         []int{0, 3},
			selection: richtext.Selection{
				Anchor: richtext.Position{Line: 1, Cluster: 0},
				Head:   richtext.Position{Line: 1, Cluster: 4},
			},
			replacement: "XYZ",
			wantCursor:  richtext.Position{Line: 1, Cluster: 3},
			wantLines:   []string{"aaaa", "XYZ", "cccc", "dddd"},
		},
		{
			name:         "a run of middle lines replaced by fewer lines, the suffix shifts down",
			initialLines: []string{"aaaa", "bbbb", "cccc", "dddd", "eeee"},
			warm:         []int{0, 4},
			selection: richtext.Selection{
				Anchor: richtext.Position{Line: 1, Cluster: 0},
				Head:   richtext.Position{Line: 3, Cluster: 4},
			},
			replacement: "XY",
			wantCursor:  richtext.Position{Line: 1, Cluster: 2},
			wantLines:   []string{"aaaa", "XY", "eeee"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := richtext.NewDocumentFromText(strings.Join(tt.initialLines, "\n"), richtext.Attrs{})

			for _, line := range tt.warm {
				_ = doc.LinePlain(line)
			}

			cursor := doc.ReplaceText(tt.selection, tt.replacement, richtext.Attrs{})

			require.Equal(t, tt.wantCursor, cursor)
			require.Equal(t, strings.Join(tt.wantLines, "\n"), doc.Plain())

			gotLines := make([]string, doc.LineCount())
			for i := range gotLines {
				gotLines[i] = doc.LinePlain(i)
			}

			require.Equal(t, tt.wantLines, gotLines)
		})
	}
}
