package richtext_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/laney/modeloff/internal/richtext"
)

// pastedLine is the worst case the editor's SingleLine mode produces: a
// multi-KB paste flattens to one long line, and every further keystroke
// edits that line.
const pastedLineSize = 8192

// BenchmarkTypeAfterLongPaste measures the cost of a single keystroke typed
// at the end of a document that already holds one multi-KB line, which is
// what SingleLine mode produces from a large paste.
func BenchmarkTypeAfterLongPaste(b *testing.B) {
	pasted := strings.Repeat("x", pastedLineSize)

	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		doc := richtext.NewDocumentFromText(pasted, richtext.Attrs{})
		pos := richtext.Position{Line: 0, Cluster: doc.LineClusterCount(0)}
		b.StartTimer()

		doc.ReplaceText(richtext.Selection{Anchor: pos, Head: pos}, "y", richtext.Attrs{})
	}
}

func largeMultiLineText(lineCount int) string {
	lines := make([]string, lineCount)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d: some realistic chat text with a handful of words in it.", i)
	}

	return strings.Join(lines, "\n")
}

// BenchmarkEditOneLineInLargeDocument measures the cost of a single-character
// edit to one line of a realistic multi-line document. Only the edited
// line's cache should need rebuilding; the other lines should be untouched.
func BenchmarkEditOneLineInLargeDocument(b *testing.B) {
	text := largeMultiLineText(500)

	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		doc := richtext.NewDocumentFromText(text, richtext.Attrs{})
		pos := richtext.Position{Line: 250, Cluster: 5}
		b.StartTimer()

		doc.ReplaceText(richtext.Selection{Anchor: pos, Head: pos}, "X", richtext.Attrs{})
	}
}

// BenchmarkToggleFormattingOneLineInLargeDocument measures the cost of
// toggling formatting (e.g. bold) over a small selection within one line of
// a realistic multi-line document.
func BenchmarkToggleFormattingOneLineInLargeDocument(b *testing.B) {
	text := largeMultiLineText(500)
	selection := richtext.Selection{
		Anchor: richtext.Position{Line: 250, Cluster: 5},
		Head:   richtext.Position{Line: 250, Cluster: 9},
	}

	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		doc := richtext.NewDocumentFromText(text, richtext.Attrs{})
		b.StartTimer()

		doc.UpdateAttrs(selection, func(attrs richtext.Attrs) richtext.Attrs {
			attrs.Bold = !attrs.Bold
			return attrs
		})
	}
}

// BenchmarkReadAccessorsOnWarmDocument measures repeated calls to read
// accessors on a document that has not changed since the previous call, so
// the cache is warm and nothing should need renormalising or reallocating.
func BenchmarkReadAccessorsOnWarmDocument(b *testing.B) {
	doc := richtext.NewDocumentFromText(largeMultiLineText(500), richtext.Attrs{})
	// Warm the cache before timing.
	_ = doc.LineCount()
	_ = doc.LineClusterCount(250)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = doc.LineCount()
		_ = doc.ClampPosition(richtext.Position{Line: 250, Cluster: 3})
		_ = doc.LineClusterCount(250)
	}
}
