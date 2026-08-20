package components

import (
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

// obsProvider is implemented by content models that can render an
// observability drawer spanning the full layout width.
type obsProvider interface {
	ObsView(width, height int) string
	ObsHeight(totalHeight int) int
}

// MainLayout splits the screen horizontally into a left panel, a
// content area in the middle, and an optional right panel.
type MainLayout struct {
	Sidebar  ui.Model
	Content  ui.Model
	NickList ui.Model

	NickListVisible bool

	windowSwitch WindowSwitchKeyMap

	// size is the terminal size the last [tea.WindowSizeMsg] reported.
	// The layout is worked out from it again whenever a panel's
	// content changes, so it has to outlive the message that carried
	// it.
	size tea.WindowSizeMsg

	// issued is the last rect each child was given. Both panels size
	// themselves to their content, so the boundary between them and
	// the content area moves without any terminal resize; a child is
	// told its rect again exactly when that boundary has moved.
	issued issuedBounds
}

// issuedBounds is the set of rects the children were last told about.
// `known` separates "no rect issued yet" from "issued a zero rect",
// which is what a terminal one row tall would produce.
type issuedBounds struct {
	sidebar  ui.Rect
	content  ui.Rect
	nickList ui.Rect
	known    bool
}

// NewMainLayout creates a MainLayout with the given left panel and
// content child models.
func NewMainLayout(sidebar, content ui.Model) MainLayout {
	return MainLayout{
		Sidebar:         sidebar,
		Content:         content,
		NickListVisible: true,
		windowSwitch:    DefaultWindowSwitchKeyMap,
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

		next, cmd := m.applyLayout()

		return next, cmd
	}

	// Window-switch keys (alt+1..9, alt+a, ctrl+n, ctrl+p) are global:
	// they target the sidebar directly and are consumed here rather
	// than reaching Content, so an alt+<digit> chord can never be
	// mistaken by the input editor for a literal character to insert.
	if key, ok := msg.(tea.KeyMsg); ok {
		if translated, matched := m.translateWindowSwitch(key); matched {
			sidebar, cmd := m.Sidebar.Update(translated)
			m.Sidebar = sidebar

			return m, cmd
		}
	}

	var cmds []tea.Cmd

	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m.size = size

		next, cmd := m.applyLayout()
		m = next
		cmds = append(cmds, cmd)

		// WindowSizeMsg is fully handled through BoundsMsg; don't
		// forward it to children where embedded viewports would
		// misinterpret it as their own dimensions.
		return m, tea.Batch(cmds...)
	}

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

// applyLayout works the three child rects out from the terminal size
// and hands each child the rect it has not already been given.
//
// [MainLayout.View] derives the same three rects from the same child
// state on every frame, and the content area's width is what the chat
// view renders its transcript at. A child left holding a stale rect
// would therefore be told one width and asked to render at another,
// and a transcript cached against the width it was rendered at would
// miss on every frame.
func (m MainLayout) applyLayout() (MainLayout, tea.Cmd) {
	if m.size.Width <= 0 || m.size.Height <= 0 {
		return m, nil
	}

	colHeight := m.columnHeight(m.size.Height)
	layout := m.computeLayout(m.size.Width, colHeight)

	next := issuedBounds{
		sidebar: ui.Rect{X: 0, Y: 0, Width: layout.sidebarInner, Height: colHeight},
		content: ui.Rect{X: layout.sidebarOuter, Y: 0, Width: layout.content, Height: m.size.Height},
		nickList: ui.Rect{
			X:      layout.sidebarOuter + layout.content,
			Y:      0,
			Width:  layout.nickListInner,
			Height: colHeight,
		},
		known: true,
	}

	var cmds []tea.Cmd

	if !m.issued.known || m.issued.sidebar != next.sidebar {
		left, cmd := m.Sidebar.Update(ui.BoundsMsg{Rect: next.sidebar})
		m.Sidebar = left
		cmds = append(cmds, cmd)
	}

	if !m.issued.known || m.issued.content != next.content {
		content, cmd := m.Content.Update(ui.BoundsMsg{Rect: next.content})
		m.Content = content
		cmds = append(cmds, cmd)
	}

	if m.NickList != nil && layout.nickListInner > 0 &&
		(!m.issued.known || m.issued.nickList != next.nickList) {
		r, cmd := m.NickList.Update(ui.BoundsMsg{Rect: next.nickList})
		m.NickList = r
		cmds = append(cmds, cmd)
	}

	m.issued = next

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

// translateWindowSwitch reports the sidebar message a window-switch
// keypress produces, if msg is one.
func (m MainLayout) translateWindowSwitch(msg tea.KeyMsg) (tea.Msg, bool) {
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
func directWindowIndex(msg tea.KeyMsg) (int, bool) {
	if !msg.Alt || msg.Type != tea.KeyRunes || len(msg.Runes) != 1 {
		return 0, false
	}

	r := msg.Runes[0]
	if r < '1' || r > '9' {
		return 0, false
	}

	return int(r - '1'), true
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

func (m MainLayout) columnHeight(totalHeight int) int {
	if obs, ok := m.Content.(obsProvider); ok {
		return totalHeight - obs.ObsHeight(totalHeight)
	}

	return totalHeight
}

// View implements ui.Model.
func (m MainLayout) View(width, height int) string {
	obsH := 0
	var obsView string

	if obs, ok := m.Content.(obsProvider); ok {
		obsH = obs.ObsHeight(height)
		if obsH > 0 {
			obsView = obs.ObsView(width, obsH)
		}
	}

	colHeight := height - obsH

	if width < theme.MinTerminalWidth {
		// Collapse both side panels and give Content the full width,
		// so every screen still renders at any width, per the
		// "always responsive" rule. Content being usable at that
		// width is a property of Content's own View, not of this
		// layout.
		content := m.Content.View(width, colHeight)
		if obsH > 0 {
			return lipgloss.JoinVertical(lipgloss.Left, content, obsView)
		}

		return content
	}

	sidebarBorder := theme.SidebarBorder.Height(colHeight)
	sidebarCap := int(float64(width) * maxSidebarFraction)
	left := sidebarBorder.Render(m.Sidebar.View(sidebarCap, colHeight))
	sidebarW := lipgloss.Width(left)

	showNL := m.wantsNickList()
	var nlView string
	nlW := 0

	if showNL {
		nlBorder := theme.NickListBorder.Height(colHeight)
		nlCap := int(float64(width) * maxNickListFraction)
		nlView = nlBorder.Render(m.NickList.View(nlCap, colHeight))
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
		left = sidebarBorder.Render(m.Sidebar.View(newSidebarInner, colHeight))
		sidebarW = lipgloss.Width(left)

		if showNL {
			nlBorder := theme.NickListBorder.Height(colHeight)
			nlFrame, _ := nlBorder.GetFrameSize()
			newNLInner := max(nlW-nlFrame-shrinkEach, 0)
			shrunk := nlBorder.Render(m.NickList.View(newNLInner, colHeight))

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

	content := m.Content.View(contentW, colHeight)

	var columns string
	if showNL {
		columns = lipgloss.JoinHorizontal(lipgloss.Top, left, content, nlView)
	} else {
		columns = lipgloss.JoinHorizontal(lipgloss.Top, left, content)
	}

	if obsH > 0 {
		return lipgloss.JoinVertical(lipgloss.Left, columns, obsView)
	}

	return columns
}

// KeyBindings implements ui.Keybinding.
func (m MainLayout) KeyBindings() []ui.KeyBinding {
	bindings := ui.CollectKeyBindings(m.Sidebar, m.Content)

	if m.NickList != nil && m.NickListVisible {
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
	items := ui.CollectStatusItems(m.Sidebar, m.Content)

	if m.NickList != nil && m.NickListVisible {
		items = append(items, ui.CollectStatusItems(m.NickList)...)
	}

	return items
}
