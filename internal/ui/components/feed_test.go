package components_test

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui/components"
)

func TestFeedView_few_lines_bottom_aligned(t *testing.T) {
	fv := components.NewFeedView("No logs yet", "new logs")
	fv = fv.SetLines([]string{"log line one", "log line two"})

	view, _, _ := fv.View(80, 24)
	stripped := ansi.Strip(view)
	lines := strings.Split(stripped, "\n")

	// Find the first line containing content.
	contentLine := -1

	for i, line := range lines {
		if strings.Contains(line, "log line one") {
			contentLine = i

			break
		}
	}

	require.NotEqual(t, -1, contentLine, "'log line one' should appear in the view")

	// Two lines of content in a 24-line viewport should be
	// anchored near the bottom, not at the top.
	require.Greater(t, contentLine, 10,
		"with two lines in a 24-line viewport, content should be bottom-aligned, not top-aligned; found at line %d", contentLine)
}
