package components

import (
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"

	tea "charm.land/bubbletea/v2"
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

// Panel is a component whose content has an intrinsic width.
type Panel interface {
	ui.Component
	ContentWidth() int
}

// MainLayout splits the screen horizontally into a left panel, a
// content area in the middle, and an optional right panel.
type MainLayout struct {
	Sidebar  Panel
	Content  ui.Component
	NickList Panel

	NickListVisible bool

	windowSwitch WindowSwitchKeyMap
	drawer       observabilityDrawer
	hasDrawer    bool

	// bounds is the rectangle the parent last assigned to this component.
	// The layout is worked out from it again whenever a panel's
	// content changes, so it has to outlive the message that carried
	// it.
	bounds uv.Rectangle
}

// NewMainLayout creates a MainLayout with the given left panel and
// content component.
func NewMainLayout(sidebar Panel, content ui.Component) MainLayout {
	return MainLayout{
		Sidebar:         sidebar,
		Content:         content,
		NickListVisible: true,
		windowSwitch:    DefaultWindowSwitchKeyMap,
	}
}

// WithObservability adds the local logs and metrics drawer.
func (m MainLayout) WithObservability(metrics MetricsPane) MainLayout {
	m.drawer = newObservabilityDrawer().withMetrics(metrics)
	m.hasDrawer = true

	return m
}

// ObservabilityOpen reports whether the observability drawer is visible.
func (m MainLayout) ObservabilityOpen() bool {
	return m.hasDrawer && m.drawer.Open
}

// SetLogEntries updates the entries shown in the observability drawer.
func (m MainLayout) SetLogEntries(entries []observability.PanelEntry) MainLayout {
	if m.hasDrawer {
		m.drawer = m.drawer.SetLogEntries(entries)
	}

	return m
}

// Init implements ui.Component.
func (m MainLayout) Init() tea.Cmd {
	cmds := []tea.Cmd{m.Sidebar.Init(), m.Content.Init()}

	if m.NickList != nil {
		cmds = append(cmds, m.NickList.Init())
	}

	if m.hasDrawer {
		cmds = append(cmds, m.drawer.Init())
	}

	return tea.Batch(cmds...)
}

// Update implements ui.Component.
func (m MainLayout) Update(msg tea.Msg) (ui.Component, tea.Cmd) {
	if _, ok := msg.(NickListToggleMsg); ok {
		m.NickListVisible = !m.NickListVisible

		next, cmd := m.applyLayout()

		return next, cmd
	}

	// Window-switch keys (alt+1..9, alt+a, ctrl+n, ctrl+p) are global:
	// they target the sidebar directly and are consumed here rather
	// than reaching Content, so an alt+<digit> chord can never be
	// mistaken by the input editor for a literal character to insert.
	if key, ok := msg.(tea.KeyPressMsg); ok {
		if translated, matched := m.translateWindowSwitch(key); matched {
			sidebar, cmd := m.Sidebar.Update(translated)
			m.Sidebar = sidebar.(Panel)

			return m, cmd
		}

		if m.hasDrawer && m.drawer.consumesKey(key) {
			wasOpen := m.drawer.Open
			wasFullscreen := m.drawer.Fullscreen
			var sidebarCmd tea.Cmd

			if wasFullscreen {
				left, cmd := m.Sidebar.Update(msg)
				m.Sidebar = left.(Panel)
				sidebarCmd = cmd
			}

			updated, cmd := m.drawer.Update(msg)
			m.drawer = updated.(observabilityDrawer)

			if m.drawer.Open != wasOpen || m.drawer.Fullscreen != wasFullscreen {
				next, layoutCmd := m.applyLayout()

				return next, tea.Batch(sidebarCmd, cmd, layoutCmd)
			}

			return m, tea.Batch(sidebarCmd, cmd)
		}
	}

	var cmds []tea.Cmd

	if bounds, ok := msg.(ui.BoundsMsg); ok {
		m.bounds = bounds.Rect

		next, cmd := m.applyLayout()

		return next, cmd
	}

	if m.observabilityFullscreen() {
		switch msg.(type) {
		case tea.MouseMsg:
			left, leftCmd := m.Sidebar.Update(msg)
			m.Sidebar = left.(Panel)

			drawer, drawerCmd := m.drawer.Update(msg)
			m.drawer = drawer.(observabilityDrawer)

			return m, tea.Batch(leftCmd, drawerCmd)

		case tea.PasteMsg:
			drawer, cmd := m.drawer.Update(msg)
			m.drawer = drawer.(observabilityDrawer)

			return m, cmd
		}
	}

	left, cmd := m.Sidebar.Update(msg)
	m.Sidebar = left.(Panel)
	cmds = append(cmds, cmd)

	content, cmd := m.Content.Update(msg)
	m.Content = content
	cmds = append(cmds, cmd)

	if m.NickList != nil {
		r, cmd := m.NickList.Update(msg)
		m.NickList = r.(Panel)
		cmds = append(cmds, cmd)
	}

	if m.hasDrawer {
		drawer, cmd := m.drawer.Update(msg)
		m.drawer = drawer.(observabilityDrawer)
		cmds = append(cmds, cmd)
	}

	// The panels have taken the message, so this is where their new
	// widths are readable.
	if resizesPanels(msg) {
		next, cmd := m.applyLayout()
		m = next
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

// resizesPanels reports whether a message can change what the sidebar
// or the nick list renders, and so where the content area starts and
// ends. It lists the message arms of [ChannelSidebar.Update] and
// [NickList.Update]; a new arm in either belongs here too.
func resizesPanels(msg tea.Msg) bool {
	switch msg.(type) {
	case SetChannelsMsg,
		ChannelAddedMsg,
		ChannelRemovedMsg,
		ChannelActiveMsg,
		ChannelUnreadMsg,
		ChannelHasLifecycleMsg,
		NickListUpdatedMsg,
		NickListThinkingMsg:
		return true
	}

	return false
}

// applyLayout works the visible child rectangles out from the assigned
// bounds and hands each child its current rectangle.
//
// [MainLayout.Draw] derives the same three rects from the same child
// state on every frame, and the content area's width is what the chat
// view renders its transcript at. A child left holding a stale rect
// would therefore be told one width and asked to render at another,
// and a transcript cached against the width it was rendered at would
// miss on every frame.
func (m MainLayout) applyLayout() (MainLayout, tea.Cmd) {
	rects := m.rectangles(m.bounds)

	var cmds []tea.Cmd

	left, cmd := m.Sidebar.Update(ui.BoundsMsg{Rect: rects.sidebar})
	m.Sidebar = left.(Panel)
	cmds = append(cmds, cmd)

	if !m.observabilityFullscreen() {
		content, cmd := m.Content.Update(ui.BoundsMsg{Rect: rects.content})
		m.Content = content
		cmds = append(cmds, cmd)
	}

	if m.NickList != nil && !m.observabilityFullscreen() {
		r, cmd := m.NickList.Update(ui.BoundsMsg{Rect: rects.nickList})
		m.NickList = r.(Panel)
		cmds = append(cmds, cmd)
	}

	if m.hasDrawer && m.drawer.Open {
		drawer, cmd := m.drawer.Update(ui.BoundsMsg{Rect: rects.drawer})
		m.drawer = drawer.(observabilityDrawer)
		cmds = append(cmds, cmd)
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

type mainLayoutRects struct {
	sidebarOuter  uv.Rectangle
	sidebar       uv.Rectangle
	content       uv.Rectangle
	nickListOuter uv.Rectangle
	nickList      uv.Rectangle
	drawer        uv.Rectangle
}

func (m MainLayout) rectangles(area uv.Rectangle) mainLayoutRects {
	columns := area
	drawer := uv.Rectangle{}

	if m.hasDrawer && m.drawer.Open && !m.drawer.Fullscreen {
		drawerHeight := m.drawer.height(area.Dy())
		columns.Max.Y -= drawerHeight
		drawer = uv.Rect(area.Min.X, columns.Max.Y, area.Dx(), drawerHeight)
	}

	layout := m.computeLayout(columns.Dx())
	x := columns.Min.X

	sidebarOuter := uv.Rect(x, columns.Min.Y, layout.sidebarOuter, columns.Dy())
	sidebar := uv.Rect(x, columns.Min.Y, layout.sidebarInner, columns.Dy())
	x += layout.sidebarOuter

	content := uv.Rect(x, columns.Min.Y, layout.content, columns.Dy())
	x += layout.content

	nickListOuter := uv.Rect(x, columns.Min.Y, layout.nickListOuter, columns.Dy())
	nickList := uv.Rect(x, columns.Min.Y, 0, columns.Dy())
	if layout.nickListOuter > 0 {
		nickListX := x + theme.NickListBorder.GetBorderLeftSize()
		nickList = uv.Rect(nickListX, columns.Min.Y, layout.nickListInner, columns.Dy())
	}

	if m.observabilityFullscreen() {
		drawer = content
	}

	return mainLayoutRects{
		sidebarOuter:  sidebarOuter,
		sidebar:       sidebar,
		content:       content,
		nickListOuter: nickListOuter,
		nickList:      nickList,
		drawer:        drawer,
	}
}

func (m MainLayout) computeLayout(width int) layoutResult {
	if width < theme.MinTerminalWidth {
		// Below the compact threshold there is no room for a sidebar
		// or nick list alongside usable chat content, so both
		// collapse and Content takes the full width. This is the
		// same "shrink to nothing" outcome the deficit-driven
		// shrinking below already reaches for the nick list; here it
		// applies unconditionally rather than only once a shrunk
		// render still overflows its allotted space.
		return layoutResult{content: width}
	}

	sidebarFrame, _ := theme.SidebarBorder.GetFrameSize()

	// Fit the sidebar's content within its share of the screen.
	sidebarCap := int(float64(width) * maxSidebarFraction)
	sidebarInner := min(m.Sidebar.ContentWidth(), max(sidebarCap-sidebarFrame, 0))
	sidebarW := sidebarInner + sidebarFrame

	// Fit the right panel's content within its share of the screen.
	showNL := m.wantsNickList()
	nlW := 0
	nlFrame := 0

	if showNL {
		nlFrame, _ = theme.NickListBorder.GetFrameSize()
		nlCap := int(float64(width) * maxNickListFraction)
		nlW = min(m.NickList.ContentWidth(), max(nlCap-nlFrame, 0)) + nlFrame
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
		sidebarInner = min(sidebarInner, newSidebarInner)
		sidebarW = sidebarInner + sidebarFrame

		// Shrink right panel.
		if showNL {
			newNLInner := max(nlW-nlFrame-shrinkEach, 0)
			nlW = newNLInner + nlFrame
		}

		contentW = width - sidebarW - nlW
	}

	return layoutResult{
		sidebarInner:  sidebarInner,
		sidebarOuter:  sidebarW,
		nickListInner: max(nlW-nlFrame, 0),
		nickListOuter: nlW,
		content:       contentW,
		showNickList:  showNL,
	}
}

// translateWindowSwitch reports the sidebar message a window-switch
// keypress produces, if msg is one.
func (m MainLayout) translateWindowSwitch(msg tea.KeyPressMsg) (tea.Msg, bool) {
	if ui.Matches(msg, m.windowSwitch.Direct) {
		if idx, ok := directWindowIndex(msg); ok {
			return ActivateIndexMsg{Index: idx}, true
		}

		return nil, false
	}

	switch {
	case ui.Matches(msg, m.windowSwitch.NextActivity):
		return ActivateNextActivityMsg{}, true
	case ui.Matches(msg, m.windowSwitch.Next):
		return ActivateOffsetMsg{Delta: 1}, true
	case ui.Matches(msg, m.windowSwitch.Previous):
		return ActivateOffsetMsg{Delta: -1}, true
	}

	return nil, false
}

// directWindowIndex extracts the zero-based window index from an
// alt+1..alt+9 keypress.
func directWindowIndex(msg tea.KeyPressMsg) (int, bool) {
	if msg.Mod&keyChordModifiers != tea.ModAlt || msg.Code < '1' || msg.Code > '9' {
		return 0, false
	}

	return int(msg.Code - '1'), true
}

func (m MainLayout) wantsNickList() bool {
	return m.NickList != nil && m.NickListVisible && !m.observabilityFullscreen()
}

func (m MainLayout) observabilityFullscreen() bool {
	return m.hasDrawer && m.drawer.Open && m.drawer.Fullscreen
}

// Draw renders each layout region directly into its assigned rectangle.
func (m MainLayout) Draw(screen uv.Screen, area uv.Rectangle) {
	rects := m.rectangles(area)
	layout := m.computeLayout(area.Dx())

	if area.Dx() >= theme.MinTerminalWidth {
		drawString(screen, rects.sidebarOuter, theme.SidebarBorder.
			Width(layout.sidebarOuter).
			Height(rects.sidebarOuter.Dy()).
			Render(" "))
		m.Sidebar.Draw(screen, rects.sidebar)
	}

	if m.observabilityFullscreen() {
		m.drawer.Draw(screen, rects.drawer)

		return
	}

	m.Content.Draw(screen, rects.content)

	if layout.showNickList {
		nickListBorder := theme.NickListBorder.
			Width(layout.nickListOuter).
			Height(rects.nickListOuter.Dy())
		drawString(screen, rects.nickListOuter, nickListBorder.Render(" "))
		m.NickList.Draw(screen, rects.nickList)
	}

	if m.hasDrawer && m.drawer.Open {
		m.drawer.Draw(screen, rects.drawer)
	}
}

// KeyBindings implements ui.Keybinding.
func (m MainLayout) KeyBindings() []ui.KeyBinding {
	bindings := ui.CollectKeyBindings(m.Sidebar)

	if m.observabilityFullscreen() {
		bindings = append(bindings, m.drawer.KeyBindings()...)
	} else {
		if m.hasDrawer {
			bindings = append(bindings, m.drawer.KeyBindings()...)
		}
		bindings = append(bindings, ui.CollectKeyBindings(m.Content)...)
	}

	if m.wantsNickList() {
		bindings = append(bindings, ui.CollectKeyBindings(m.NickList)...)
	}

	bindings = append(bindings,
		m.windowSwitch.Direct,
		m.windowSwitch.NextActivity,
		m.windowSwitch.Next,
		m.windowSwitch.Previous,
	)

	return bindings
}

// StatusItems implements ui.StatusProvider.
func (m MainLayout) StatusItems() []ui.StatusItem {
	items := ui.CollectStatusItems(m.Sidebar)

	if !m.observabilityFullscreen() {
		items = append(items, ui.CollectStatusItems(m.Content)...)
	}

	if m.wantsNickList() {
		items = append(items, ui.CollectStatusItems(m.NickList)...)
	}

	if m.hasDrawer {
		items = append(items, m.drawer.StatusItems()...)
	}

	return items
}
