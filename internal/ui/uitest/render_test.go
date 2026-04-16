package uitest

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRenderedLines_strips_ansi_and_trailing_spaces(t *testing.T) {
	got := RenderedLines("\x1b[31mhello  \x1b[0m\nworld   \n")

	require.Equal(t, []string{"hello", "world"}, got)
}

func TestNonEmptyLines_removes_blank_rows(t *testing.T) {
	got := NonEmptyLines("first\n\nsecond\n")

	require.Equal(t, []string{"first", "second"}, got)
}

func TestLastNonEmptyLine_returns_final_visible_line(t *testing.T) {
	got := LastNonEmptyLine("first\n\nsecond\n")

	require.Equal(t, "second", got)
}

func TestVisibleColumns_splits_layout_regions(t *testing.T) {
	got := VisibleColumns(" left │ middle │ right \n one │ two │ three \n")

	require.Equal(t, [][]string{
		{"left", "one"},
		{"middle", "two"},
		{"right", "three"},
	}, got)
}

func TestNonEmptyColumn_removes_blank_rows(t *testing.T) {
	got := NonEmptyColumn([]string{"first", "", "second"})

	require.Equal(t, []string{"first", "second"}, got)
}
