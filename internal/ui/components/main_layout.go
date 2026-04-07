package components

import (
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"

	tea "github.com/charmbracelet/bubbletea"
)

// maxSidebarFraction caps the sidebar at this fraction of terminal
// width.
const maxSidebarFraction = 0.2

// maxNickListFraction caps the nick list at this fraction of
// terminal width.
const maxNickListFraction = 0.15

// minMainWidth is the narrowest the main content area can be before
// sidebars are asked to shrink.
const minMainWidth = 40

// NickListToggleMsg is sent when the user toggles the nick list
// visibility.
type NickListToggleMsg struct{}

type nickListPreference interface {
	WantsNickListHidden() bool
}

// MainLayout splits the screen horizontally into a left panel, a
// content area in the middle, and an optional right panel.
type MainLayout struct {
	Sidebar  ui.Model
	Content  ui.Model
	NickList ui.Model

	NickListVisible bool
}

// NewMainLayout creates a MainLayout with the given left panel and
// content child models.
func NewMainLayout(sidebar, content ui.Model) MainLayout {
	return MainLayout{
		Sidebar:         sidebar,
		Content:         content,
		NickListVisible: true,
	}
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
		layout := m.computeLayout(size.Width, size.Height)

		left, cmd := m.Sidebar.Update(ui.BoundsMsg{
			Rect: ui.Rect{X: 0, Y: 0, Width: layout.sidebarInner, Height: size.Height},
		})
		m.Sidebar = left
		cmds = append(cmds, cmd)

		content, cmd := m.Content.Update(ui.BoundsMsg{
			Rect: ui.Rect{X: layout.sidebarOuter, Y: 0, Width: layout.content, Height: size.Height},
		})
		m.Content = content
		cmds = append(cmds, cmd)

		if m.NickList != nil && layout.nickListInner > 0 {
			r, cmd := m.NickList.Update(ui.BoundsMsg{
				Rect: ui.Rect{
					X:      layout.sidebarOuter + layout.content,
					Y:      0,
					Width:  layout.nickListInner,
					Height: size.Height,
				},
			})
			m.NickList = r
			cmds = append(cmds, cmd)
		}
	}

	// WindowSizeMsg is fully handled above via BoundsMsg; don't
	// forward it to children where embedded viewports would
	// misinterpret it as their own dimensions.
	if _, ok := msg.(tea.WindowSizeMsg); !ok {
		left, cmd := m.Sidebar.Update(msg)
		m.Sidebar = left
		cmds = append(cmds, cmd)

		content, cmd := m.Content.Update(msg)
		m.Content = content
		cmds = append(cmds, cmd)

		if m.NickList != nil {
			r, cmd := m.NickList.Update(msg)
			m.NickList = r
			cmds = append(cmds, cmd)
		}
	}

	return m, tea.Batch(cmds...)
}

type layoutResult struct {
	sidebarInner  int
	sidebarOuter  int
	nickListInner int
	nickListOuter int
	content       int
	showNickList  bool
}

func (m MainLayout) computeLayout(width, height int) layoutResult {
	sidebarBorder := theme.SidebarBorder.Height(height)
	sidebarFrame, _ := sidebarBorder.GetFrameSize()

	// Render sidebar unconstrained (capped by fraction).
	sidebarCap := int(float64(width) * maxSidebarFraction)
	sidebarView := sidebarBorder.Render(
		m.Sidebar.View(sidebarCap, height))
	sidebarW := lipgloss.Width(sidebarView)

	// Render right panel unconstrained (capped by fraction), if shown.
	showNL := m.wantsNickList()
	nlW := 0
	nlFrame := 0

	if showNL {
		nlBorder := theme.NickListBorder.Height(height)
		nlFrame, _ = nlBorder.GetFrameSize()
		nlCap := int(float64(width) * maxNickListFraction)
		nlView := nlBorder.Render(
			m.NickList.View(nlCap, height))
		nlW = lipgloss.Width(nlView)
	}

	contentW := width - sidebarW - nlW

	// If content is too narrow, distribute shrinkage.
	if contentW < minMainWidth {
		deficit := minMainWidth - contentW
		panels := 1
		if showNL {
			panels = 2
		}

		shrinkEach := (deficit + panels - 1) / panels

		// Shrink sidebar.
		newSidebarInner := max(sidebarW-sidebarFrame-shrinkEach, 0)
		shrunkSidebar := sidebarBorder.Render(
			m.Sidebar.View(newSidebarInner, height))
		sidebarW = lipgloss.Width(shrunkSidebar)

		// Shrink right panel.
		if showNL {
			newNLInner := max(nlW-nlFrame-shrinkEach, 0)
			nlBorder := theme.NickListBorder.Height(height)
			shrunkNL := nlBorder.Render(
				m.NickList.View(newNLInner, height))

			if lipgloss.Width(shrunkNL) > nlW-shrinkEach {
				showNL = false
				nlW = 0
			} else {
				nlW = lipgloss.Width(shrunkNL)
			}
		}

		contentW = width - sidebarW - nlW
	}

	return layoutResult{
		sidebarInner:  max(sidebarW-sidebarFrame, 0),
		sidebarOuter:  sidebarW,
		nickListInner: max(nlW-nlFrame, 0),
		nickListOuter: nlW,
		content:       contentW,
		showNickList:  showNL,
	}
}

func (m MainLayout) wantsNickList() bool {
	if m.NickList == nil || !m.NickListVisible {
		return false
	}

	if preference, ok := m.Content.(nickListPreference); ok {
		return !preference.WantsNickListHidden()
	}

	return true
}

// View implements ui.Model.
func (m MainLayout) View(width, height int) string {
	if width < theme.MinTerminalWidth {
		return theme.NarrowTerminalView(width, height)
	}

	sidebarBorder := theme.SidebarBorder.Height(height)
	sidebarCap := int(float64(width) * maxSidebarFraction)
	left := sidebarBorder.Render(m.Sidebar.View(sidebarCap, height))
	sidebarW := lipgloss.Width(left)

	showNL := m.wantsNickList()
	var nlView string
	nlW := 0

	if showNL {
		nlBorder := theme.NickListBorder.Height(height)
		nlCap := int(float64(width) * maxNickListFraction)
		nlView = nlBorder.Render(m.NickList.View(nlCap, height))
		nlW = lipgloss.Width(nlView)
	}

	contentW := width - sidebarW - nlW

	// Shrink sidebars if main area is too narrow.
	if contentW < minMainWidth {
		deficit := minMainWidth - contentW
		panels := 1
		if showNL {
			panels = 2
		}

		shrinkEach := (deficit + panels - 1) / panels
		sidebarFrame, _ := sidebarBorder.GetFrameSize()

		newSidebarInner := max(sidebarW-sidebarFrame-shrinkEach, 0)
		left = sidebarBorder.Render(m.Sidebar.View(newSidebarInner, height))
		sidebarW = lipgloss.Width(left)

		if showNL {
			nlBorder := theme.NickListBorder.Height(height)
			nlFrame, _ := nlBorder.GetFrameSize()
			newNLInner := max(nlW-nlFrame-shrinkEach, 0)
			shrunk := nlBorder.Render(m.NickList.View(newNLInner, height))

			if lipgloss.Width(shrunk) > nlW-shrinkEach {
				showNL = false
				nlW = 0
				nlView = ""
			} else {
				nlView = shrunk
				nlW = lipgloss.Width(nlView)
			}
		}

		contentW = width - sidebarW - nlW
	}

	content := m.Content.View(contentW, height)

	if showNL {
		return lipgloss.JoinHorizontal(lipgloss.Top, left, content, nlView)
	}

	return lipgloss.JoinHorizontal(lipgloss.Top, left, content)
}

// KeyBindings implements ui.Keybinding.
func (m MainLayout) KeyBindings() []key.Binding {
	bindings := ui.CollectKeyBindings(m.Sidebar, m.Content)

	if m.NickList != nil && m.NickListVisible {
		bindings = append(bindings, ui.CollectKeyBindings(m.NickList)...)
	}

	return bindings
}

// StatusItems implements ui.StatusProvider.
func (m MainLayout) StatusItems() []ui.StatusItem {
	items := ui.CollectStatusItems(m.Sidebar, m.Content)

	if m.NickList != nil && m.NickListVisible {
		items = append(items, ui.CollectStatusItems(m.NickList)...)
	}

	return items
}
