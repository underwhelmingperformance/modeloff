package components

import (
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/language"

	"github.com/laney/modeloff/internal/observability"
)

func TestRenderLogEntries_respects_timestamp_format(t *testing.T) {
	entries := []observability.PanelEntry{{
		Timestamp: time.Date(2026, 4, 7, 9, 30, 0, 0, time.UTC),
		Level:     "INFO",
		Message:   "hello",
	}}
	format := "%X"

	lines := renderLogEntries(entries, 120, &format, language.BritishEnglish)

	require.Equal(t, []string{"09:30:00 INFO hello"}, stripLines(lines))
}

func TestRenderLogEntries_can_disable_timestamps(t *testing.T) {
	entries := []observability.PanelEntry{{
		Timestamp: time.Date(2026, 4, 7, 9, 30, 0, 0, time.UTC),
		Level:     "INFO",
		Message:   "hello",
	}}
	disabled := ""

	lines := renderLogEntries(entries, 120, &disabled, language.BritishEnglish)

	require.Equal(t, []string{"INFO hello"}, stripLines(lines))
}

func stripLines(lines []string) []string {
	stripped := make([]string, 0, len(lines))

	for _, line := range lines {
		stripped = append(stripped, trimLine(ansi.Strip(line)))
	}

	return stripped
}

func trimLine(line string) string {
	for len(line) > 0 && line[len(line)-1] == ' ' {
		line = line[:len(line)-1]
	}

	return line
}
