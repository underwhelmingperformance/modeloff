package components

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"

	tea "github.com/charmbracelet/bubbletea"
)

// sidebarFraction is the fraction of the total width given to the
// sidebar. The remainder goes to the content area.
const sidebarFraction = 0.2

// minSidebarWidth is the narrowest the sidebar can be, in columns.
const minSidebarWidth = 16

// maxSidebarWidth is the widest the sidebar can be, in columns.
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
	if width < theme.MinTerminalWidth {
		return theme.NarrowTerminalView(width, height)
	}

	bar := statusBar(width)
	barHeight := lipgloss.Height(bar)
	contentHeight := height - barHeight

	sw := sidebarWidth(width)
	cw := width - sw

	borderStyle := theme.SidebarBorder.
		Height(contentHeight)

	frameW, _ := borderStyle.GetFrameSize()
	innerSW := sw - frameW

	left := borderStyle.Render(m.Sidebar.View(innerSW, contentHeight))
	right := m.Content.View(cw, contentHeight)

	main := lipgloss.JoinHorizontal(lipgloss.Top, left, right)

	return lipgloss.JoinVertical(lipgloss.Left, main, bar)
}

// statusBar renders a single-line bar showing keyboard shortcuts.
// At narrow widths it abbreviates to fit.
func statusBar(width int) string {
	type shortcut struct {
		key  string
		desc string
	}

	full := []shortcut{
		{"^D/U", "nav"},
		{"^O", "select"},
		{"PgUp/Dn", "scroll"},
		{"/", "commands"},
		{"^C", "quit"},
	}

	parts := make([]string, len(full))
	for i, s := range full {
		parts[i] = fmt.Sprintf("%s %s", s.key, s.desc)
	}

	text := strings.Join(parts, "  ")

	// Abbreviate if too wide.
	if lipgloss.Width(text) > width {
		short := []string{"^D/U nav", "^O sel", "PgUp/Dn", "/ cmds", "^C quit"}
		text = strings.Join(short, " ")
	}

	return theme.Dim.Render(text)
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
