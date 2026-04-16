package components_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
	"github.com/laney/modeloff/internal/ui/uitest"
)

func visibleLines(view string) []string {
	lines := uitest.NonEmptyLines(view)

	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}

	return lines
}

func renderedLines(view string) []string {
	return uitest.RenderedLines(view)
}

func visibleColumns(view string) [][]string {
	return uitest.VisibleColumns(view)
}

func nonEmptyColumn(lines []string) []string {
	return uitest.NonEmptyColumn(lines)
}

func inputBarModel(t *testing.T, m ui.Model) components.InputBar {
	t.Helper()

	bar, ok := m.(components.InputBar)
	require.True(t, ok, "expected components.InputBar, got %T", m)

	return bar
}

func inputValue(t *testing.T, m ui.Model) string {
	t.Helper()

	return inputBarModel(t, m).Value()
}
