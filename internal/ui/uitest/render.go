package uitest

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// StripANSI removes terminal escape sequences from rendered output.
func StripANSI(view string) string {
	return ansi.Strip(view)
}

// RenderedLines splits rendered output into lines, preserving height
// while trimming trailing whitespace from each line.
func RenderedLines(view string) []string {
	stripped := StripANSI(strings.ReplaceAll(view, "\r\n", "\n"))
	lines := strings.Split(stripped, "\n")

	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}

	return lines
}

// NonEmptyLines returns the visible lines with blank rows removed.
func NonEmptyLines(view string) []string {
	lines := RenderedLines(view)
	out := make([]string, 0, len(lines))

	for _, line := range lines {
		if line == "" {
			continue
		}

		out = append(out, line)
	}

	return out
}

// LastNonEmptyLine returns the final visible line in the rendered
// output, or an empty string when there is none.
func LastNonEmptyLine(view string) string {
	lines := NonEmptyLines(view)
	if len(lines) == 0 {
		return ""
	}

	return lines[len(lines)-1]
}

// VisibleColumns splits each rendered line into column regions using
// the `│` box-drawing separator. This is deliberately lossy for
// content that contains a literal `│` character; tests relying on
// column segmentation should avoid such fixtures.
func VisibleColumns(view string) [][]string {
	lines := RenderedLines(view)
	maxColumns := 1

	for _, line := range lines {
		columns := strings.Count(line, "│") + 1
		if columns > maxColumns {
			maxColumns = columns
		}
	}

	out := make([][]string, maxColumns)

	for _, line := range lines {
		parts := strings.Split(line, "│")

		for i := range maxColumns {
			part := ""
			if i < len(parts) {
				part = strings.TrimSpace(parts[i])
			}

			out[i] = append(out[i], part)
		}
	}

	return out
}

// NonEmptyColumn returns the visible entries from a column extracted
// by VisibleColumns.
func NonEmptyColumn(lines []string) []string {
	out := make([]string, 0, len(lines))

	for _, line := range lines {
		if line == "" {
			continue
		}

		out = append(out, line)
	}

	return out
}

// TrimmedVisibleLines returns NonEmptyLines with whitespace trimmed on
// each side, for tests that compare against unpadded expectations.
func TrimmedVisibleLines(view string) []string {
	lines := NonEmptyLines(view)

	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}

	return lines
}

// SplitBodyAndStatus separates the chat body from the single-line
// status bar at the bottom of the view.
func SplitBodyAndStatus(view string) (string, string) {
	lines := RenderedLines(view)
	if len(lines) == 0 {
		return "", ""
	}

	status := strings.TrimSpace(lines[len(lines)-1])
	body := strings.Join(lines[:len(lines)-1], "\n")

	return body, status
}

// CompactLine collapses runs of whitespace within a line into single
// spaces, for comparing tokenised output against a human-readable
// expectation.
func CompactLine(line string) string {
	return strings.Join(strings.Fields(line), " ")
}

// WithoutTimestampPrefix strips a leading `[HH:MM:SS] ` (or similar)
// timestamp prefix from a rendered chat line, returning everything
// from the first `<nick>` token onward.
func WithoutTimestampPrefix(line string) string {
	if idx := strings.Index(line, " <"); idx >= 0 {
		return line[idx+1:]
	}

	return line
}

// NonBorderSegments splits each visible line on the `│` column
// separator and returns the non-empty, non-box-drawing segments. It
// is the shared building block for view segmenters that want to see
// "everything visible that isn't structural decoration".
//
// Like VisibleColumns, this split is deliberately lossy for content
// carrying a literal `│` — for current test fixtures this is fine.
func NonBorderSegments(view string) []string {
	lines := NonEmptyLines(view)
	out := make([]string, 0, len(lines))

	for _, line := range lines {
		for part := range strings.SplitSeq(line, "│") {
			cleaned := strings.TrimSpace(part)
			if cleaned == "" {
				continue
			}

			if strings.Trim(cleaned, "┌┐└┘─│├┤┬┴┼") == "" {
				continue
			}

			out = append(out, cleaned)
		}
	}

	return out
}
