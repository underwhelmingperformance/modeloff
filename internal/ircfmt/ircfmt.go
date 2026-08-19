// Package ircfmt encodes and decodes IRC formatting control codes.
package ircfmt

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/laney/modeloff/internal/richtext"
)

// IRC formatting control characters.
const (
	Bold          = '\x02'
	Colour        = '\x03'
	Reset         = '\x0f'
	Reverse       = '\x16'
	Italic        = '\x1d'
	Strike        = '\x1e'
	Underline     = '\x1f'
	maxColourCode = 99

	// defaultColourCode is the modern formatting spec's sentinel for "no
	// colour". Attrs already uses a nil FG/BG to mean the same thing, so
	// normaliseColour maps this code to nil on both the read and the
	// write side: Parse never stores 99 literally, and Encode treats an
	// Attrs holding a literal 99 (built by something other than Parse)
	// the same as nil rather than round-tripping it as a distinct
	// colour the wire format has no room to represent. Encode also uses
	// this code to clear a colour without emitting an unmarked colour
	// byte that digit-leading text could run into.
	defaultColourCode = 99
)

// extendedPaletteHex is the modern formatting spec's fixed RGB value for
// each extended-palette colour code, 16 through 98 inclusive, indexed by
// code-16 (https://modern.ircdocs.horse/formatting.html#colors-16-98). It
// is a spec constant, unrelated to any terminal's own palette or to
// ANSI-256 index order.
var extendedPaletteHex = [...]string{
	"470000", "472100", "474700", "324700", "004700", "00472c", "004747",
	"002747", "000047", "2e0047", "470047", "47002a", "740000", "743a00",
	"747400", "517400", "007400", "007449", "007474", "004074", "000074",
	"4b0074", "740074", "740045", "b50000", "b56300", "b5b500", "7db500",
	"00b500", "00b571", "00b5b5", "0063b5", "0000b5", "7500b5", "b500b5",
	"b5006b", "ff0000", "ff8c00", "ffff00", "b2ff00", "00ff00", "00ffa0",
	"00ffff", "008cff", "0000ff", "a500ff", "ff00ff", "ff0098", "ff5959",
	"ffb459", "ffff71", "cfff60", "6fff6f", "65ffc9", "6dffff", "59b4ff",
	"5959ff", "c459ff", "ff66ff", "ff59bc", "ff9c9c", "ffd39c", "ffff9c",
	"e2ff9c", "9cff9c", "9cffdb", "9cffff", "9cd3ff", "9c9cff", "dc9cff",
	"ff9cff", "ff94d3", "000000", "131313", "282828", "363636", "4d4d4d",
	"656565", "818181", "9f9f9f", "bcbcbc", "e2e2e2", "ffffff",
}

// ExtendedRGB reports the modern formatting spec's fixed RGB colour for an
// IRC colour index, as a lipgloss-consumable "#RRGGBB" hex string. It
// covers only the extended palette, codes 16-98: ok is false for 0-15,
// which the spec leaves to the terminal's own ANSI colours rather than
// assigning them an RGB value, so a renderer keeps sending those through
// its existing indexed-colour path, and false for 99, the default-colour
// sentinel Parse never stores as a value.
func ExtendedRGB(index uint8) (hex string, ok bool) {
	if index < 16 || int(index) > 16+len(extendedPaletteHex)-1 {
		return "", false
	}

	return "#" + extendedPaletteHex[index-16], true
}

// Parse decodes IRC formatting codes into a rich-text document. The input is
// untrusted wire text: it may come from another model or from anything a
// model chose to relay. Parse strips every C0 and C1 control byte except the
// formatting codes above (and tab, which passes through as ordinary
// whitespace), so the returned document is safe to hand to a terminal
// renderer.
func Parse(raw string) richtext.Document {
	var (
		lines     = []richtext.Line{{}}
		current   richtext.Attrs
		spanText  strings.Builder
		spanAttrs = current
		flushSpan func()
	)

	flushSpan = func() {
		if spanText.Len() == 0 {
			spanAttrs = cloneAttrs(current)
			return
		}

		line := &lines[len(lines)-1]
		line.Spans = append(line.Spans, richtext.Span{
			Text:  spanText.String(),
			Attrs: cloneAttrs(spanAttrs),
		})
		spanText.Reset()
		spanAttrs = cloneAttrs(current)
	}

	// IRC control codes and the other C0/C1 controls Parse strips are all
	// ASCII or the two-byte UTF-8 encoding of U+0080-U+009F, so scanning
	// the string byte-by-byte is safe here. Everything else is copied
	// through unchanged and later segmented into graphemes by richtext.
	for index := 0; index < len(raw); {
		r := raw[index]

		switch r {
		case '\n':
			flushSpan()
			lines = append(lines, richtext.Line{})
			index++
			continue

		case Bold:
			flushSpan()
			current.Bold = !current.Bold
			spanAttrs = cloneAttrs(current)
			index++
			continue

		case Italic:
			flushSpan()
			current.Italic = !current.Italic
			spanAttrs = cloneAttrs(current)
			index++
			continue

		case Underline:
			flushSpan()
			current.Underline = !current.Underline
			spanAttrs = cloneAttrs(current)
			index++
			continue

		case Reverse:
			flushSpan()
			current.Reverse = !current.Reverse
			spanAttrs = cloneAttrs(current)
			index++
			continue

		case Strike:
			flushSpan()
			current.Strike = !current.Strike
			spanAttrs = cloneAttrs(current)
			index++
			continue

		case Reset:
			flushSpan()
			current = richtext.Attrs{}
			spanAttrs = current
			index++
			continue

		case Colour:
			flushSpan()
			nextIndex, fg, bg, ok := parseColours(raw, index+1)
			if !ok {
				current.FG = nil
				current.BG = nil
				spanAttrs = cloneAttrs(current)
				index++
				continue
			}

			current.FG = fg
			current.BG = bg
			spanAttrs = cloneAttrs(current)
			index = nextIndex
			continue
		}

		if skip := stripControlLen(raw, index); skip > 0 {
			index += skip
			continue
		}

		spanText.WriteByte(raw[index])
		index++
	}

	flushSpan()

	return richtext.NewDocumentFromLines(lines)
}

// stripControlLen reports how many bytes at index form a control character
// that Parse must not pass through to a terminal, or 0 if raw[index] is not
// one. The formatting codes above are matched by the switch in Parse before
// this runs, so this only ever sees other C0 controls (0x00-0x1F, excluding
// tab), DEL (0x7F), and C1 controls, which appear in UTF-8 as the two-byte
// sequences 0xC2 0x80 through 0xC2 0x9F.
func stripControlLen(raw string, index int) int {
	b := raw[index]

	if (b < 0x20 || b == 0x7f) && b != '\t' {
		return 1
	}

	if b == 0xc2 && index+1 < len(raw) {
		next := raw[index+1]
		if next >= 0x80 && next <= 0x9f {
			return 2
		}
	}

	return 0
}

// Strip removes IRC formatting codes from the raw text.
func Strip(raw string) string {
	doc := Parse(raw)
	return doc.Plain()
}

// Encode converts a rich-text document into IRC formatting codes.
func Encode(doc richtext.Document) string {
	var (
		builder strings.Builder
		current richtext.Attrs
	)

	for lineIndex := range doc.LineCount() {
		if lineIndex > 0 {
			builder.WriteByte('\n')
			current = richtext.Attrs{}
		}

		for _, span := range doc.Line(lineIndex).Spans {
			current = writeTransition(&builder, current, span.Attrs)
			builder.WriteString(span.Text)
		}

		if !current.Reset() {
			builder.WriteRune(Reset)
		}
	}

	return builder.String()
}

func writeTransition(builder *strings.Builder, from, to richtext.Attrs) richtext.Attrs {
	if from.Equals(to) {
		return from
	}

	if needsReset(from, to) {
		builder.WriteRune(Reset)
		from = richtext.Attrs{}
	}

	if from.Bold != to.Bold {
		builder.WriteRune(Bold)
		from.Bold = to.Bold
	}
	if from.Italic != to.Italic {
		builder.WriteRune(Italic)
		from.Italic = to.Italic
	}
	if from.Underline != to.Underline {
		builder.WriteRune(Underline)
		from.Underline = to.Underline
	}
	if from.Reverse != to.Reverse {
		builder.WriteRune(Reverse)
		from.Reverse = to.Reverse
	}
	if from.Strike != to.Strike {
		builder.WriteRune(Strike)
		from.Strike = to.Strike
	}

	if !equalColour(from.FG, to.FG) || !equalColour(from.BG, to.BG) {
		builder.WriteRune(Colour)
		writeColourCodes(builder, to.FG, to.BG)
		from.FG = cloneColour(to.FG)
		from.BG = cloneColour(to.BG)
	}

	return from
}

// writeColourCodes writes the colour digits that follow a Colour byte, in
// one of the two forms the modern formatting spec defines for a code with
// digits after it: foreground alone, or foreground and background joined
// by a comma. The spec has no form for a background with no foreground
// digits before it, so a background-only Attrs still writes foreground
// digits, using the default-colour sentinel in place of a real value.
// normaliseColour guards fg and bg here the same way Parse guards them on
// the way in, so a literal 99 set by anything other than Parse encodes
// exactly as nil would. Foreground digits are always written and are
// always two digits (formatColour is fixed-width and the sentinel is
// two digits itself), so this never leaves the Colour byte bare: a bare
// colour byte immediately followed by digit-leading text would have those
// digits misread as a colour code on the next parse.
func writeColourCodes(builder *strings.Builder, fg, bg *uint8) {
	fg = normaliseColour(fg)
	bg = normaliseColour(bg)

	if fg != nil {
		builder.WriteString(formatColour(*fg))
	} else {
		builder.WriteString(formatColour(defaultColourCode))
	}

	if bg != nil {
		builder.WriteByte(',')
		builder.WriteString(formatColour(*bg))
	}
}

func needsReset(from, to richtext.Attrs) bool {
	if (from.Bold && !to.Bold) ||
		(from.Italic && !to.Italic) ||
		(from.Underline && !to.Underline) ||
		(from.Reverse && !to.Reverse) ||
		(from.Strike && !to.Strike) {
		return true
	}

	return false
}

// parseColours parses the colour digits following a Colour byte at start.
// It reports ok only when start begins with valid foreground digits. A
// comma at start, with no foreground digits before it, is left for the
// caller: the modern formatting spec defines that shape as resetting both
// colours and rendering everything from the comma onward as literal text,
// which is what Parse's Colour case already does for any other ok=false
// result.
func parseColours(raw string, start int) (next int, fg, bg *uint8, ok bool) {
	index, first, firstOK := parseColourCode(raw, start)
	if !firstOK {
		return start, nil, nil, false
	}

	fg = normaliseColour(first)
	next = index
	ok = true

	if next < len(raw) && raw[next] == ',' {
		if afterComma, second, secondOK := parseColourCode(raw, next+1); secondOK {
			next = afterComma
			bg = normaliseColour(second)
		}
	}

	return next, fg, bg, ok
}

// normaliseColour maps defaultColourCode to nil, so ircfmt never stores the
// spec's default-colour sentinel as a literal value.
func normaliseColour(value *uint8) *uint8 {
	if value != nil && *value == defaultColourCode {
		return nil
	}

	return value
}

func parseColourCode(raw string, start int) (int, *uint8, bool) {
	if start >= len(raw) || !unicode.IsDigit(rune(raw[start])) {
		return start, nil, false
	}

	end := start + 1
	if end < len(raw) && unicode.IsDigit(rune(raw[end])) {
		end++
	}

	value, err := strconv.Atoi(raw[start:end])
	if err != nil || value < 0 || value > maxColourCode {
		return start, nil, false
	}

	colour := uint8(value)

	return end, &colour, true
}

func formatColour(index uint8) string {
	return strconv.FormatInt(int64(index/10), 10) + strconv.FormatInt(int64(index%10), 10)
}

func cloneAttrs(attrs richtext.Attrs) richtext.Attrs {
	return richtext.Attrs{
		Bold:      attrs.Bold,
		Italic:    attrs.Italic,
		Underline: attrs.Underline,
		Reverse:   attrs.Reverse,
		Strike:    attrs.Strike,
		FG:        cloneColour(attrs.FG),
		BG:        cloneColour(attrs.BG),
	}
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
