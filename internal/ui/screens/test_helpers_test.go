package screens_test

import (
	"github.com/laney/modeloff/internal/ui/uitest"
)

func trimmedVisibleLines(view string) []string {
	return uitest.TrimmedVisibleLines(view)
}

func splitBodyAndStatus(view string) (string, string) {
	return uitest.SplitBodyAndStatus(view)
}

func compactLine(line string) string {
	return uitest.CompactLine(line)
}

func withoutTimestampPrefix(line string) string {
	return uitest.WithoutTimestampPrefix(line)
}

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
