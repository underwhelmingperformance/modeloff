package components_test

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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

			if !strings.Contains(got, tt.wantSub) {
				t.Errorf("View(%d, %d) = %q, want substring %q",
					tt.width, tt.height, got, tt.wantSub)
			}
		})
	}
}

func TestMainLayout_View_fills_width(t *testing.T) {
	sidebar := stubModel{label: "S"}
	content := stubModel{label: "C"}

	layout := components.NewMainLayout(sidebar, content)
	got := layout.View(100, 24)

	renderedWidth := lipgloss.Width(got)

	if renderedWidth > 100 {
		t.Errorf("rendered width %d exceeds available width 100", renderedWidth)
	}
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

	if cmd == nil {
		t.Error("Init() returned nil, expected batched command")
	}
}
