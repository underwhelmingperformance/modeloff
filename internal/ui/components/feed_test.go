package components_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui/components"
	"github.com/laney/modeloff/internal/ui/uitest"
)

func TestFeedView_few_lines_bottom_aligned(t *testing.T) {
	fv := components.NewFeedView("No logs yet", "new logs")
	fv = fv.SetLines([]string{"log line one", "log line two"})

	view, _, _ := fv.View(80, 24)
	lines := uitest.RenderedLines(view)

	// Bottom-aligned: the two log lines occupy the last two rows of the
	// 24-row viewport, preceded by 22 blank rows.
	want := make([]string, 24)
	want[22] = "log line one"
	want[23] = "log line two"
	require.Equal(t, want, lines)
}
