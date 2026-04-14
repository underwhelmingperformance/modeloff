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
	maxColourCode = 15
)

// Parse decodes IRC formatting codes into a rich-text document.
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

	// IRC control codes are single-byte ASCII control characters, so scanning
	// the UTF-8 string byte-by-byte is safe here. Any non-control bytes are
	// copied through unchanged and later segmented into graphemes by richtext.
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

		spanText.WriteByte(raw[index])
		index++
	}

	flushSpan()

	return richtext.NewDocumentFromLines(lines)
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
		if to.FG != nil {
			builder.WriteString(formatColour(*to.FG))
			if to.BG != nil {
				builder.WriteByte(',')
				builder.WriteString(formatColour(*to.BG))
			}
		}
		from.FG = cloneColour(to.FG)
		from.BG = cloneColour(to.BG)
	}

	return from
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

func parseColours(raw string, start int) (next int, fg, bg *uint8, ok bool) {
	index, first, firstOK := parseColourCode(raw, start)
	if !firstOK {
		return start, nil, nil, false
	}

	fg = first
	next = index
	ok = true

	if next < len(raw) && raw[next] == ',' {
		var secondOK bool
		next, bg, secondOK = parseColourCode(raw, next+1)
		if !secondOK {
			bg = nil
		}
	}

	return next, fg, bg, ok
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
