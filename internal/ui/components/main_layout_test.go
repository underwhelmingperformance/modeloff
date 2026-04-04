package components_test

import (
	"fmt"
	"testing"

	bkey "github.com/charmbracelet/bubbles/key"
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

type keybindingStubModel struct {
	stubModel
	bindings []bkey.Binding
}

func (s keybindingStubModel) KeyBindings() []bkey.Binding {
	return s.bindings
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
		got := layout.View(79, 24)

		require.Contains(t, got, "Resize terminal to 80+ columns")
		require.NotContains(t, got, "sidebar:")
		require.NotContains(t, got, "content:")
	})

	t.Run("at threshold renders normally", func(t *testing.T) {
		got := layout.View(80, 24)

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

func TestMainLayout_View_preserves_height(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}
	layout := components.NewMainLayout(sidebar, content)

	got := layout.View(80, 24)

	require.Equal(t, 24, lipgloss.Height(got))
}

func TestMainLayout_View_three_pane_at_wide_width(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}
	nicklist := stubModel{label: "nicks"}

	layout := components.NewMainLayout(sidebar, content)
	layout.SetNickList(nicklist)

	got := layout.View(120, 24)

	require.Contains(t, got, "sidebar:")
	require.Contains(t, got, "content:")
	require.Contains(t, got, "nicks:")
}

func TestMainLayout_View_hides_nicklist_at_narrow_width(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}
	nicklist := stubModel{label: "nicks"}

	layout := components.NewMainLayout(sidebar, content)
	layout.SetNickList(nicklist)

	got := layout.View(80, 24)

	require.Contains(t, got, "sidebar:")
	require.Contains(t, got, "content:")
	require.NotContains(t, got, "nicks:")
}

func TestMainLayout_View_nicklist_toggle(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}
	nicklist := stubModel{label: "nicks"}

	layout := components.NewMainLayout(sidebar, content)
	layout.SetNickList(nicklist)

	// Initially visible at wide width.
	got := layout.View(120, 24)
	require.Contains(t, got, "nicks:")

	// Toggle off.
	updated, _ := layout.Update(components.NickListToggleMsg{})
	layout = updated.(components.MainLayout)

	got = layout.View(120, 24)
	require.NotContains(t, got, "nicks:")

	// Toggle back on.
	updated, _ = layout.Update(components.NickListToggleMsg{})
	layout = updated.(components.MainLayout)

	got = layout.View(120, 24)
	require.Contains(t, got, "nicks:")
}

func TestMainLayout_View_no_nicklist_without_set(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}

	layout := components.NewMainLayout(sidebar, content)

	got := layout.View(120, 24)

	require.Contains(t, got, "sidebar:")
	require.Contains(t, got, "content:")
}

func TestMainLayout_View_three_pane_fills_width(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}
	nicklist := stubModel{label: "nicks"}

	layout := components.NewMainLayout(sidebar, content)
	layout.SetNickList(nicklist)

	got := layout.View(120, 24)

	renderedWidth := lipgloss.Width(got)
	require.LessOrEqual(t, renderedWidth, 120,
		"three-pane rendered width must not exceed total width")
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

func TestMainLayout_KeyBindings_collects_from_children(t *testing.T) {
	sidebar := keybindingStubModel{
		stubModel: stubModel{label: "sidebar"},
		bindings: []bkey.Binding{
			bkey.NewBinding(bkey.WithKeys("ctrl+d"), bkey.WithHelp("^D", "channels")),
		},
	}
	content := keybindingStubModel{
		stubModel: stubModel{label: "content"},
		bindings: []bkey.Binding{
			bkey.NewBinding(bkey.WithKeys("pgup"), bkey.WithHelp("PgUp", "scroll")),
		},
	}

	layout := components.NewMainLayout(sidebar, content)

	require.Equal(t, []bkey.Binding{
		bkey.NewBinding(bkey.WithKeys("ctrl+d"), bkey.WithHelp("^D", "channels")),
		bkey.NewBinding(bkey.WithKeys("pgup"), bkey.WithHelp("PgUp", "scroll")),
	}, layout.KeyBindings())
}
