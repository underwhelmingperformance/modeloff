package screens_test

import (
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
