package components_test

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui/components"
)

func TestPanelList_Render_shows_items(t *testing.T) {
	p := components.NewPanelList()

	got := ansi.Strip(p.Render(20, 10, components.PanelContent{
		Items:  []string{"alpha", "beta", "gamma"},
		Cursor: -1,
	}))

	require.Contains(t, got, "alpha")
	require.Contains(t, got, "beta")
	require.Contains(t, got, "gamma")
}

func TestPanelList_Render_empty_shows_placeholder(t *testing.T) {
	p := components.NewPanelList()

	got := ansi.Strip(p.Render(20, 10, components.PanelContent{
		Empty:  "Nothing here",
		Cursor: -1,
	}))

	require.Contains(t, got, "Nothing here")
}

func TestPanelList_Render_empty_default_placeholder(t *testing.T) {
	p := components.NewPanelList()

	got := ansi.Strip(p.Render(20, 10, components.PanelContent{
		Cursor: -1,
	}))

	require.Contains(t, got, "Empty")
}

func TestPanelList_Render_header(t *testing.T) {
	p := components.NewPanelList()

	got := ansi.Strip(p.Render(20, 10, components.PanelContent{
		Items:  []string{"item1"},
		Header: "Title",
		Cursor: -1,
	}))

	require.Contains(t, got, "Title")
	require.Contains(t, got, "item1")
}

func TestPanelList_Render_respects_height(t *testing.T) {
	p := components.NewPanelList()

	got := p.Render(20, 3, components.PanelContent{
		Items:  []string{"a", "b", "c", "d", "e"},
		Cursor: -1,
	})

	require.Equal(t, 3, lipgloss.Height(got))
}

func TestPanelList_Render_width_enforcement(t *testing.T) {
	p := components.NewPanelList()

	got := p.Render(15, 5, components.PanelContent{
		Items:  []string{"hi"},
		Cursor: -1,
	})

	require.Equal(t, 15, lipgloss.Width(got))
}

func TestPanelList_Render_cursor_scrolls_into_view(t *testing.T) {
	p := components.NewPanelList()

	items := make([]string, 20)
	for i := range items {
		items[i] = "item"
	}

	// Render with cursor at the end; viewport height is only 5.
	got := ansi.Strip(p.Render(20, 5, components.PanelContent{
		Items:  items,
		Cursor: 19,
	}))

	// The viewport should have scrolled so the cursor line is visible.
	require.Equal(t, 5, lipgloss.Height(got))
	require.Equal(t, 15, p.YOffset())
}

func TestPanelList_Render_header_reduces_list_height(t *testing.T) {
	p := components.NewPanelList()

	got := p.Render(20, 4, components.PanelContent{
		Items:  []string{"a", "b", "c", "d", "e"},
		Header: "Hdr",
		Cursor: -1,
	})

	require.Equal(t, 4, lipgloss.Height(got))

	stripped := ansi.Strip(got)
	require.Contains(t, stripped, "Hdr")
	require.Contains(t, stripped, "a")
}
