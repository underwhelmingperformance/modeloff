package screens_test

import (
	"reflect"

	"github.com/laney/modeloff/internal/ui/uitest"
)

// normaliseContent compacts whitespace and strips timestamp prefixes
// from each column line, so full-slice assertions remain stable
// against non-deterministic timestamps.
func normaliseContent(lines []string) []string {
	out := make([]string, len(lines))

	for i, line := range lines {
		out[i] = uitest.CompactLine(uitest.WithoutTimestampPrefix(line))
	}

	return out
}

func normalisedVisibleColumns(view string) [][]string {
	body, _ := uitest.SplitBodyAndStatus(view)
	columns := uitest.VisibleColumns(body)
	out := make([][]string, 0, len(columns))
	for position, column := range columns {
		lines := uitest.NonEmptyColumn(column)
		if position == 1 {
			lines = normaliseContent(uitest.WithoutHeader(lines))
		}
		out = append(out, lines)
	}

	return out
}

func waitForVisibleColumns(tm *uitest.App, want [][]string) string {
	return tm.WaitForView(func(view string) bool {
		return reflect.DeepEqual(want, normalisedVisibleColumns(view))
	})
}
