package components

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"

	tea "github.com/charmbracelet/bubbletea"
)

// SidebarWidth is the fraction of the total width given to the
// sidebar. The remainder goes to the content area.
const sidebarFraction = 0.2

// MinSidebarWidth is the narrowest the sidebar can be, in columns.
const minSidebarWidth = 16

// MaxSidebarWidth is the widest the sidebar can be, in columns.
const maxSidebarWidth = 30

// MainLayout splits the screen horizontally into a sidebar on the
// left and a content area on the right.
type MainLayout struct {
	Sidebar ui.Model
	Content ui.Model
}

// NewMainLayout creates a MainLayout with the given sidebar and
// content child models.
func NewMainLayout(sidebar, content ui.Model) MainLayout {
	return MainLayout{
		Sidebar: sidebar,
		Content: content,
	}
}

// Init implements ui.Model.
func (m MainLayout) Init() tea.Cmd {
	return tea.Batch(m.Sidebar.Init(), m.Content.Init())
}

// Update implements ui.Model.
func (m MainLayout) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	var cmds []tea.Cmd

	sidebar, cmd := m.Sidebar.Update(msg)
	m.Sidebar = sidebar
	cmds = append(cmds, cmd)

	content, cmd := m.Content.Update(msg)
	m.Content = content
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

// View implements ui.Model.
func (m MainLayout) View(width, height int) string {
	sw := sidebarWidth(width)
	cw := width - sw

	borderStyle := theme.SidebarBorder.
		Height(height)

	frameW, _ := borderStyle.GetFrameSize()
	innerSW := sw - frameW

	left := borderStyle.Render(m.Sidebar.View(innerSW, height))
	right := m.Content.View(cw, height)

	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

func sidebarWidth(totalWidth int) int {
	sw := int(float64(totalWidth) * sidebarFraction)

	if sw < minSidebarWidth {
		sw = minSidebarWidth
	}

	if sw > maxSidebarWidth {
		sw = maxSidebarWidth
	}

	if sw > totalWidth {
		sw = totalWidth
	}

	return sw
}
