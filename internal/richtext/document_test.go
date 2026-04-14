package richtext_test

import (
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
