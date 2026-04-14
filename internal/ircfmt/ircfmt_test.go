package ircfmt_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ircfmt"
	"github.com/laney/modeloff/internal/richtext"
)

func colour(index uint8) *uint8 {
	value := index

	return &value
}

func TestParseHandlesSupportedControls(t *testing.T) {
	raw := "\x02bold\x02 \x1ditalic\x1d \x1funder\x1f \x16rev\x16 \x1estrike\x1e \x0304,07colours\x0f plain"

	doc := ircfmt.Parse(raw)

	require.Equal(t, "bold italic under rev strike colours plain", doc.Plain())
	require.Equal(t, []richtext.Span{
		{Text: "bold", Attrs: richtext.Attrs{Bold: true}},
		{Text: " ", Attrs: richtext.Attrs{}},
		{Text: "italic", Attrs: richtext.Attrs{Italic: true}},
		{Text: " ", Attrs: richtext.Attrs{}},
		{Text: "under", Attrs: richtext.Attrs{Underline: true}},
		{Text: " ", Attrs: richtext.Attrs{}},
		{Text: "rev", Attrs: richtext.Attrs{Reverse: true}},
		{Text: " ", Attrs: richtext.Attrs{}},
		{Text: "strike", Attrs: richtext.Attrs{Strike: true}},
		{Text: " ", Attrs: richtext.Attrs{}},
		{Text: "colours", Attrs: richtext.Attrs{FG: colour(4), BG: colour(7)}},
		{Text: " plain", Attrs: richtext.Attrs{}},
	}, doc.Line(0).Spans)
}

func TestParseMalformedColoursDoNotPanic(t *testing.T) {
	doc := ircfmt.Parse("a\x03x b\x0399oops c\x03,04still")

	require.Equal(t, "ax b99oops c,04still", doc.Plain())
	require.Equal(t, []richtext.Span{{
		Text:  "ax b99oops c,04still",
		Attrs: richtext.Attrs{},
	}}, doc.Line(0).Spans)
}

func TestEncodeParseRoundTripPreservesDocumentMeaning(t *testing.T) {
	doc := richtext.NewDocumentFromLines([]richtext.Line{
		{
			Spans: []richtext.Span{
				{Text: "hello ", Attrs: richtext.Attrs{}},
				{Text: "world", Attrs: richtext.Attrs{Bold: true, FG: colour(4)}},
			},
		},
		{
			Spans: []richtext.Span{
				{Text: "again", Attrs: richtext.Attrs{Underline: true, Strike: true}},
			},
		},
	})

	encoded := ircfmt.Encode(doc)
	decoded := ircfmt.Parse(encoded)

	require.Equal(t, doc.Plain(), decoded.Plain())
	require.Equal(t, []richtext.Line{doc.Line(0), doc.Line(1)}, []richtext.Line{decoded.Line(0), decoded.Line(1)})
}

func TestCanonicalRawRoundTrip(t *testing.T) {
	raw := "plain \x02bold\x0f \x0303,12colour\x0f\n\x1f\x1estruck\x0f"

	require.Equal(t, raw, ircfmt.Encode(ircfmt.Parse(raw)))
}

func TestEncodeUsesCanonicalTwoDigitColours(t *testing.T) {
	doc := richtext.NewDocumentFromLines([]richtext.Line{{
		Spans: []richtext.Span{
			{Text: "x", Attrs: richtext.Attrs{FG: colour(3), BG: colour(12)}},
		},
	}})

	require.Equal(t, "\x0303,12x\x0f", ircfmt.Encode(doc))
}

func TestStripRemovesFormatting(t *testing.T) {
	require.Equal(t, "hello world", ircfmt.Strip("\x02hello\x02 \x0304world\x0f"))
}
