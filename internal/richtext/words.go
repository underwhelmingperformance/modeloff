package richtext

import (
	"unicode"
)

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
