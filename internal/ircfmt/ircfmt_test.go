package ircfmt_test

import (
	"fmt"
	"strconv"
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

// TestParseColourCodes covers how Parse resolves the digits following a
// Colour byte: a bare clear, the 0-99 range the modern formatting spec
// defines (including 99, the default-colour sentinel), the empty-foreground
// form that sets only a background, and the cases where a following comma
// or extra digit is left as literal text because no valid code claims it.
func TestParseColourCodes(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantPlain string
		wantSpans []richtext.Span
	}{
		{
			name:      "bare colour byte clears without consuming following text",
			raw:       "a\x03x",
			wantPlain: "ax",
			wantSpans: []richtext.Span{{Text: "ax", Attrs: richtext.Attrs{}}},
		},
		{
			name:      "colour 99 is the default sentinel and carries no visible colour",
			raw:       "b\x0399oops",
			wantPlain: "boops",
			wantSpans: []richtext.Span{{Text: "boops", Attrs: richtext.Attrs{}}},
		},
		{
			name:      "a comma with no foreground digits before it resets both colours and is left as literal text",
			raw:       "c\x03,04still",
			wantPlain: "c,04still",
			wantSpans: []richtext.Span{{Text: "c,04still", Attrs: richtext.Attrs{}}},
		},
		{
			name:      "a comma with no valid background digits is left as literal text",
			raw:       "\x034,hello",
			wantPlain: ",hello",
			wantSpans: []richtext.Span{{Text: ",hello", Attrs: richtext.Attrs{FG: colour(4)}}},
		},
		{
			name:      "colour 16 is accepted as the extended palette's lowest index",
			raw:       "\x0316ext",
			wantPlain: "ext",
			wantSpans: []richtext.Span{{Text: "ext", Attrs: richtext.Attrs{FG: colour(16)}}},
		},
		{
			name:      "colour 98 is accepted as the extended palette's highest index",
			raw:       "\x0398ext",
			wantPlain: "ext",
			wantSpans: []richtext.Span{{Text: "ext", Attrs: richtext.Attrs{FG: colour(98)}}},
		},
		{
			name:      "a colour code consumes at most two digits",
			raw:       "\x03100x",
			wantPlain: "0x",
			wantSpans: []richtext.Span{{Text: "0x", Attrs: richtext.Attrs{FG: colour(10)}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := ircfmt.Parse(tt.raw)

			require.Equal(t, tt.wantPlain, doc.Plain())
			require.Equal(t, tt.wantSpans, doc.Line(0).Spans)
		})
	}
}

// TestExtendedRGB pins the modern formatting spec's fixed RGB value for
// the extended palette (16-98), and confirms 0-15 and 99 report ok=false
// so a caller keeps rendering those through its own indexed-colour path.
func TestExtendedRGB(t *testing.T) {
	tests := []struct {
		name    string
		index   uint8
		wantHex string
		wantOK  bool
	}{
		{name: "index 0 is the legacy palette, not the extended one", index: 0, wantHex: "", wantOK: false},
		{name: "index 15 is the legacy palette's highest index", index: 15, wantHex: "", wantOK: false},
		{name: "index 16 is the extended palette's lowest index", index: 16, wantHex: "#470000", wantOK: true},
		{name: "index 52 is a spot value mid-range", index: 52, wantHex: "#ff0000", wantOK: true},
		{name: "index 60 is a spot value mid-range", index: 60, wantHex: "#0000ff", wantOK: true},
		{name: "index 88 is a spot value mid-range", index: 88, wantHex: "#000000", wantOK: true},
		{name: "index 98 is the extended palette's highest index", index: 98, wantHex: "#ffffff", wantOK: true},
		{name: "index 99, the default-colour sentinel, has no RGB value", index: 99, wantHex: "", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hex, ok := ircfmt.ExtendedRGB(tt.index)

			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.wantHex, hex)
		})
	}
}

func TestParseStripsControlCharactersNotPartOfIrcFormatting(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantPlain string
		wantSpans []richtext.Span
	}{
		{
			name:      "ESC introducing a CSI sequence is stripped, the rest of the sequence is plain text",
			raw:       "safe\x1b[31mtext",
			wantPlain: "safe[31mtext",
			wantSpans: []richtext.Span{{Text: "safe[31mtext", Attrs: richtext.Attrs{}}},
		},
		{
			name:      "NUL is stripped",
			raw:       "a\x00b",
			wantPlain: "ab",
			wantSpans: []richtext.Span{{Text: "ab", Attrs: richtext.Attrs{}}},
		},
		{
			name:      "CR is stripped",
			raw:       "a\x0db",
			wantPlain: "ab",
			wantSpans: []richtext.Span{{Text: "ab", Attrs: richtext.Attrs{}}},
		},
		{
			name:      "DEL is stripped",
			raw:       "a\x7fb",
			wantPlain: "ab",
			wantSpans: []richtext.Span{{Text: "ab", Attrs: richtext.Attrs{}}},
		},
		{
			name:      "the C1 control NEL is stripped",
			raw:       "a\u0085b",
			wantPlain: "ab",
			wantSpans: []richtext.Span{{Text: "ab", Attrs: richtext.Attrs{}}},
		},
		{
			name:      "the C1 control CSI is stripped",
			raw:       "a\u009bb",
			wantPlain: "ab",
			wantSpans: []richtext.Span{{Text: "ab", Attrs: richtext.Attrs{}}},
		},
		{
			name:      "tab passes through unchanged",
			raw:       "a\tb",
			wantPlain: "a\tb",
			wantSpans: []richtext.Span{{Text: "a\tb", Attrs: richtext.Attrs{}}},
		},
		{
			name:      "stripping does not disturb ircfmt's own formatting codes",
			raw:       "\x02bold\x1b[0m\x02",
			wantPlain: "bold[0m",
			wantSpans: []richtext.Span{{Text: "bold[0m", Attrs: richtext.Attrs{Bold: true}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := ircfmt.Parse(tt.raw)

			require.Equal(t, tt.wantPlain, doc.Plain())
			require.Equal(t, tt.wantSpans, doc.Line(0).Spans)
		})
	}
}

func TestEncodeBackgroundOnlyColourUsesDefaultForegroundSentinel(t *testing.T) {
	doc := richtext.NewDocumentFromLines([]richtext.Line{{
		Spans: []richtext.Span{{Text: "x", Attrs: richtext.Attrs{BG: colour(9)}}},
	}})

	encoded := ircfmt.Encode(doc)
	roundTripped := ircfmt.Parse(encoded)

	require.Equal(t, "\x0399,09x\x0f", encoded)
	require.Equal(t, doc.Line(0).Spans, roundTripped.Line(0).Spans)
}

func TestEncodeParseRoundTripDoesNotDropDigitsAfterColourClear(t *testing.T) {
	doc := richtext.NewDocumentFromLines([]richtext.Line{{
		Spans: []richtext.Span{
			{Text: "count: ", Attrs: richtext.Attrs{FG: colour(4)}},
			{Text: "15 items left", Attrs: richtext.Attrs{}},
		},
	}})

	roundTripped := ircfmt.Parse(ircfmt.Encode(doc))

	require.Equal(t, doc.Line(0).Spans, roundTripped.Line(0).Spans)
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

// colourPairs is a boundary-covering set of (FG, BG) values: unset, the
// smallest and largest legacy (0-15) and extended (16-98) indices, a
// background-only pair, and both set together. It is deliberately not the
// full 100x100 colour space; combined with the exhaustive bool-attribute
// cross product below and with the exhaustive from/to pairing in the
// transition test, it exercises every branch parseColours, parseColourCode
// and writeColourCodes can take.
func colourPairs() []struct{ FG, BG *uint8 } {
	return []struct{ FG, BG *uint8 }{
		{nil, nil},
		{colour(0), nil},
		{colour(9), nil},
		{colour(15), nil},
		{colour(16), nil},
		{colour(98), nil},
		{nil, colour(9)},
		{colour(3), colour(4)},
	}
}

// edgeTexts are the text shapes that could interact with a preceding colour
// code: digit-leading with one or two digits, comma-leading, a digit and a
// comma together, and ordinary text as a control.
func edgeTexts() []string {
	return []string{
		"9",
		"15 items left",
		"99 problems",
		"5,10 pair",
		",leading comma",
		"plain text",
	}
}

func allBoolAttrCombos() []richtext.Attrs {
	combos := make([]richtext.Attrs, 0, 32)
	for bits := range 32 {
		combos = append(combos, richtext.Attrs{
			Bold:      bits&1 != 0,
			Italic:    bits&2 != 0,
			Underline: bits&4 != 0,
			Reverse:   bits&8 != 0,
			Strike:    bits&16 != 0,
		})
	}

	return combos
}

func colourLabel(v *uint8) string {
	if v == nil {
		return "nil"
	}

	return strconv.Itoa(int(*v))
}

// TestParseEncodeRoundTripSingleSpan is the round-trip property test for
// fix 1: Parse(Encode(doc)) must reproduce doc for every combination of the
// five boolean attributes with every boundary colour pair, against every
// text shape a colour clear could run into.
func TestParseEncodeRoundTripSingleSpan(t *testing.T) {
	for _, boolAttrs := range allBoolAttrCombos() {
		for _, cp := range colourPairs() {
			attrs := boolAttrs
			attrs.FG = cp.FG
			attrs.BG = cp.BG

			for _, text := range edgeTexts() {
				name := fmt.Sprintf("bold=%v/italic=%v/underline=%v/reverse=%v/strike=%v/fg=%s/bg=%s/text=%q",
					attrs.Bold, attrs.Italic, attrs.Underline, attrs.Reverse, attrs.Strike,
					colourLabel(attrs.FG), colourLabel(attrs.BG), text)

				t.Run(name, func(t *testing.T) {
					doc := richtext.NewDocumentFromLines([]richtext.Line{{
						Spans: []richtext.Span{{Text: text, Attrs: attrs}},
					}})

					roundTripped := ircfmt.Parse(ircfmt.Encode(doc))

					require.Equal(t, doc.Line(0).Spans, roundTripped.Line(0).Spans)
				})
			}
		}
	}
}

// TestParseEncodeRoundTripSpanTransitions is the multi-span half of the
// round-trip property: fix 1's bug only shows up at the boundary between a
// coloured span and the next one, so this exercises every boundary colour
// pair transitioning into every other one, with the second span's text
// drawn from the same digit/comma-leading edge cases.
func TestParseEncodeRoundTripSpanTransitions(t *testing.T) {
	pairs := colourPairs()
	texts := edgeTexts()

	for _, from := range pairs {
		fromAttrs := richtext.Attrs{FG: from.FG, BG: from.BG}

		for _, to := range pairs {
			toAttrs := richtext.Attrs{FG: to.FG, BG: to.BG}

			for _, text := range texts {
				name := fmt.Sprintf("from(fg=%s,bg=%s)/to(fg=%s,bg=%s)/text=%q",
					colourLabel(from.FG), colourLabel(from.BG),
					colourLabel(to.FG), colourLabel(to.BG), text)

				t.Run(name, func(t *testing.T) {
					doc := richtext.NewDocumentFromLines([]richtext.Line{{
						Spans: []richtext.Span{
							{Text: "prefix", Attrs: fromAttrs},
							{Text: text, Attrs: toAttrs},
						},
					}})

					roundTripped := ircfmt.Parse(ircfmt.Encode(doc))

					require.Equal(t, doc.Line(0).Spans, roundTripped.Line(0).Spans)
				})
			}
		}
	}
}

// TestParseEncodeRoundTripSpanTransitionsWithBoldToggle folds a bold
// transition into the colour transition, so a full reset (fix 1 must still
// hold once needsReset fires and clears the colour along with the rest of
// the attributes) is covered too.
func TestParseEncodeRoundTripSpanTransitionsWithBoldToggle(t *testing.T) {
	boldTransitions := []struct{ From, To bool }{
		{From: false, To: true},
		{From: true, To: false},
		{From: true, To: true},
		{From: false, To: false},
	}
	pairs := colourPairs()
	texts := edgeTexts()

	for _, bt := range boldTransitions {
		for _, cp := range pairs {
			fromAttrs := richtext.Attrs{Bold: bt.From, FG: cp.FG, BG: cp.BG}
			toAttrs := richtext.Attrs{Bold: bt.To}

			for _, text := range texts {
				name := fmt.Sprintf("bold(%v->%v)/from-colour(fg=%s,bg=%s)/text=%q",
					bt.From, bt.To, colourLabel(cp.FG), colourLabel(cp.BG), text)

				t.Run(name, func(t *testing.T) {
					doc := richtext.NewDocumentFromLines([]richtext.Line{{
						Spans: []richtext.Span{
							{Text: "prefix", Attrs: fromAttrs},
							{Text: text, Attrs: toAttrs},
						},
					}})

					roundTripped := ircfmt.Parse(ircfmt.Encode(doc))

					require.Equal(t, doc.Line(0).Spans, roundTripped.Line(0).Spans)
				})
			}
		}
	}
}
