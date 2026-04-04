package components

import (
	"github.com/charmbracelet/bubbles/key"
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

// nickListFraction is the fraction of the total width given to the
// nick list panel.
const nickListFraction = 0.15

// minNickListWidth is the narrowest the nick list can be, in columns.
const minNickListWidth = 14

// maxNickListWidth is the widest the nick list can be, in columns.
const maxNickListWidth = 24

// minWidthForNickList is the minimum terminal width at which the nick
// list is shown. Below this width, the nick list is hidden regardless
// of the toggle state.
const minWidthForNickList = 100

// NickListToggleMsg is sent when the user toggles the nick list
// panel visibility.
type NickListToggleMsg struct{}

// MainLayout splits the screen horizontally into a sidebar on the
// left, a content area in the middle, and an optional nick list on
// the right.
type MainLayout struct {
	Sidebar  ui.Model
	Content  ui.Model
	NickList ui.Model

	NickListVisible bool
}

// NewMainLayout creates a MainLayout with the given sidebar and
// content child models. The nick list is nil by default.
func NewMainLayout(sidebar, content ui.Model) MainLayout {
	return MainLayout{
		Sidebar:         sidebar,
		Content:         content,
		NickListVisible: true,
	}
}

// SetNickList sets the nick list panel for the layout.
func (m *MainLayout) SetNickList(nl ui.Model) {
	m.NickList = nl
}

// Init implements ui.Model.
func (m MainLayout) Init() tea.Cmd {
	cmds := []tea.Cmd{m.Sidebar.Init(), m.Content.Init()}

	if m.NickList != nil {
		cmds = append(cmds, m.NickList.Init())
	}

	return tea.Batch(cmds...)
}

// Update implements ui.Model.
func (m MainLayout) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	if _, ok := msg.(NickListToggleMsg); ok {
		m.NickListVisible = !m.NickListVisible
		return m, nil
	}

	var cmds []tea.Cmd

	if size, ok := msg.(tea.WindowSizeMsg); ok {
		sw := sidebarWidth(size.Width)
		cw := size.Width - sw

		// Children receive BoundsMsg before WindowSizeMsg so they can
		// update hit-testing and other absolute-layout state first.
		borderStyle := theme.SidebarBorder.Height(size.Height)
		frameW, _ := borderStyle.GetFrameSize()
		innerSW := sw - frameW
		if innerSW < 0 {
			innerSW = 0
		}

		sidebar, cmd := m.Sidebar.Update(ui.BoundsMsg{
			Rect: ui.Rect{X: 0, Y: 0, Width: innerSW, Height: size.Height},
		})
		m.Sidebar = sidebar
		cmds = append(cmds, cmd)

		content, cmd := m.Content.Update(ui.BoundsMsg{
			Rect: ui.Rect{X: sw, Y: 0, Width: cw, Height: size.Height},
		})
		m.Content = content
		cmds = append(cmds, cmd)
	}

	sidebar, cmd := m.Sidebar.Update(msg)
	m.Sidebar = sidebar
	cmds = append(cmds, cmd)

	content, cmd := m.Content.Update(msg)
	m.Content = content
	cmds = append(cmds, cmd)

	if m.NickList != nil {
		nl, cmd := m.NickList.Update(msg)
		m.NickList = nl
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

// View implements ui.Model.
func (m MainLayout) View(width, height int) string {
	if width < theme.MinTerminalWidth {
		return theme.NarrowTerminalView(width, height)
	}

	sw := sidebarWidth(width)

	borderStyle := theme.SidebarBorder.
		Height(height)
	frameW, _ := borderStyle.GetFrameSize()
	innerSW := sw - frameW

	left := borderStyle.Render(m.Sidebar.View(innerSW, height))

	showNickList := m.NickList != nil && m.NickListVisible && width >= minWidthForNickList
	nlw := 0

	if showNickList {
		nlw = nickListWidth(width)
	}

	cw := width - sw - nlw

	right := m.Content.View(cw, height)

	var main string

	if showNickList {
		nlBorderStyle := theme.NickListBorder.Height(height)
		nlFrameW, _ := nlBorderStyle.GetFrameSize()
		innerNLW := nlw - nlFrameW

		nlView := nlBorderStyle.Render(m.NickList.View(innerNLW, height))

		main = lipgloss.JoinHorizontal(lipgloss.Top, left, right, nlView)
	} else {
		main = lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	}

	return main
}

// KeyBindings implements ui.Keybinding.
func (m MainLayout) KeyBindings() []key.Binding {
	bindings := ui.CollectKeyBindings(m.Sidebar, m.Content)

	if m.NickList != nil && m.NickListVisible {
		bindings = append(bindings, ui.CollectKeyBindings(m.NickList)...)
	}

	return bindings
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

func nickListWidth(totalWidth int) int {
	nw := int(float64(totalWidth) * nickListFraction)

	if nw < minNickListWidth {
		nw = minNickListWidth
	}

	if nw > maxNickListWidth {
		nw = maxNickListWidth
	}

	return nw
}
