package ui_test

import (
	"strings"

	"github.com/laney/modeloff/internal/ui/uitest"
)

// inputLine extracts the visible text of the input bar from a chat
// screen render, with surrounding box borders and padding removed.
// The input bar is the bordered workspace's own bottom row, not the
// status bar SplitBodyAndStatus strips off — a status bar row, when
// there is one, sits below it.
func inputLine(view string) string {
	body, _ := uitest.SplitBodyAndStatus(view)

	lines := uitest.RenderedLines(body)
	if len(lines) == 0 {
		return ""
	}

	return strings.Trim(lines[len(lines)-1], "│ \t")
}

func visibleBodySegments(view string) []string {
	body, _ := uitest.SplitBodyAndStatus(view)

	return uitest.NonBorderSegments(body)
}

func bodyColumns(view string) [][]string {
	body, _ := uitest.SplitBodyAndStatus(view)

	return uitest.VisibleColumns(body)
}

func sidebarColumn(view string) []string {
	columns := bodyColumns(view)
	if len(columns) == 0 {
		return nil
	}

	return uitest.NonEmptyColumn(columns[0])
}

func contentColumn(view string) []string {
	columns := bodyColumns(view)
	if len(columns) < 2 {
		return nil
	}

	return uitest.NonEmptyColumn(columns[1])
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
