package components_test

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
)

// stubModel is a minimal ui.Model for testing layout behaviour.
type stubModel struct {
	label string
}

func (s stubModel) Init() tea.Cmd { return nil }

func (s stubModel) Update(tea.Msg) (ui.Model, tea.Cmd) {
	return s, nil
}

func (s stubModel) View(width, height int) string {
	return fmt.Sprintf("%s:%dx%d", s.label, width, height)
}

func TestMainLayout_View_responsive(t *testing.T) {
	tests := []struct {
		name    string
		width   int
		height  int
		wantSub string
	}{
		{
			name:    "sidebar and content are both rendered",
			width:   80,
			height:  24,
			wantSub: "sidebar:",
		},
		{
			name:    "content is rendered",
			width:   80,
			height:  24,
			wantSub: "content:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sidebar := stubModel{label: "sidebar"}
			content := stubModel{label: "content"}

			layout := components.NewMainLayout(sidebar, content)
			got := layout.View(tt.width, tt.height)

			require.Contains(t, got, tt.wantSub,
				"View(%d, %d) should contain %q",
				tt.width, tt.height, tt.wantSub)
		})
	}
}

func TestMainLayout_View_narrow_terminal(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}
	layout := components.NewMainLayout(sidebar, content)

	t.Run("below threshold shows resize message", func(t *testing.T) {
		got := layout.View(39, 24)

		require.Contains(t, got, "Resize terminal to 40+ columns")
		require.NotContains(t, got, "sidebar:")
		require.NotContains(t, got, "content:")
	})

	t.Run("at threshold renders normally", func(t *testing.T) {
		got := layout.View(40, 24)

		require.NotContains(t, got, "Resize terminal")
		require.Contains(t, got, "sidebar:")
		require.Contains(t, got, "content:")
	})
}

func TestMainLayout_View_fills_width(t *testing.T) {
	sidebar := stubModel{label: "S"}
	content := stubModel{label: "C"}

	layout := components.NewMainLayout(sidebar, content)
	got := layout.View(100, 24)

	renderedWidth := lipgloss.Width(got)
	require.LessOrEqual(t, renderedWidth, 100)
}

func TestMainLayout_View_has_status_bar(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}
	layout := components.NewMainLayout(sidebar, content)

	got := layout.View(80, 24)

	require.Contains(t, got, "nav")
	require.Contains(t, got, "quit")
	require.Contains(t, got, "scroll")
}

func TestMainLayout_View_status_bar_preserves_height(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}
	layout := components.NewMainLayout(sidebar, content)

	got := layout.View(80, 24)

	require.Equal(t, 24, lipgloss.Height(got))
}

func TestMainLayout_View_status_bar_abbreviates_narrow(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}
	layout := components.NewMainLayout(sidebar, content)

	got := layout.View(45, 24)

	// Should still contain shortcuts even when narrow.
	require.Contains(t, got, "nav")
	require.Contains(t, got, "quit")
}

// initModel is a stubModel that returns a command from Init.
type initModel struct {
	stubModel
	initMsg string
}

func (m initModel) Init() tea.Cmd {
	msg := m.initMsg
	return func() tea.Msg { return msg }
}

func TestMainLayout_Init_batches_children(t *testing.T) {
	sidebar := initModel{stubModel: stubModel{label: "sidebar"}, initMsg: "s"}
	content := initModel{stubModel: stubModel{label: "content"}, initMsg: "c"}

	layout := components.NewMainLayout(sidebar, content)
	cmd := layout.Init()

	require.NotNil(t, cmd)
}
