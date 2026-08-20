package components

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/laney/modeloff/internal/set"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

// defaultItemStyle is the padding applied to every line in a
// sidebar.
var defaultItemStyle = lipgloss.NewStyle().PaddingLeft(1).PaddingRight(1)

// ViewState describes the selection/activation state of a sidebar
// item, passed to the view function so it can style accordingly.
type ViewState int

// Sidebar item view states.
const (
	StateNone ViewState = iota
	StateSelected
	StateActive
	StateActiveSelected
)

// SidebarConfig holds the functions that parameterise a
// Sidebar's behaviour.
type SidebarConfig[T any, K comparable] struct {
	Key        func(T) K
	View       func(T, ViewState, int) string
	OnActivate func(T) tea.Cmd

	// HasActivity reports whether an item has unseen activity worth
	// jumping to. It is optional: a sidebar that never sets it simply
	// ignores ActivateNextActivityMsg.
	HasActivity func(T) bool

	// Section returns the group label an item is rendered under. A
	// label row is drawn once, immediately above the first item whose
	// Section differs from the previous item's. Items already sorted
	// into contiguous runs by that label (as [domain.Window]'s
	// ordering already groups DMs after channels) form visible groups
	// this way, without the sidebar doing any sorting of its own. The
	// empty string renders no label. Section is optional: a nil
	// Section draws no labels at all.
	Section func(T) string
}

// sidebarRow is one line Sidebar.View draws: either a group label
// (ItemIndex -1) or a pointer back to the item at ItemIndex in the
// sorted set's iteration order. Cursor placement and mouse
// hit-testing both walk this same row order, computed by rowLayout,
// so a click lands on the item actually drawn at that row and a
// label row is never mistaken for one.
type sidebarRow struct {
	ItemIndex int
	Label     string
}

// rowLayout walks the sorted items in order and returns one entry
// per line View will draw, inserting a label row wherever cfg.Section
// changes to a new non-empty value. It does not depend on width, so
// both View (which also needs rendered text) and the mouse
// hit-tester (which needs only the mapping) can call it cheaply.
func (s Sidebar[T, K]) rowLayout() []sidebarRow {
	if s.items == nil {
		return nil
	}

	rows := make([]sidebarRow, 0, s.items.Len())
	lastSection := ""
	idx := 0

	for item := range s.items.All() {
		if s.cfg.Section != nil {
			if sec := s.cfg.Section(item); sec != lastSection && sec != "" {
				rows = append(rows, sidebarRow{ItemIndex: -1, Label: sec})
				lastSection = sec
			} else {
				lastSection = sec
			}
		}

		rows = append(rows, sidebarRow{ItemIndex: idx})
		idx++
	}

	return rows
}

// ActivateIndexMsg asks a sidebar to activate the item at the given
// zero-based position, driving the alt+1..alt+9 direct window
// switch. An out-of-range index, or a sidebar with no OnActivate, is
// a no-op.
type ActivateIndexMsg struct {
	Index int
}

// ActivateOffsetMsg asks a sidebar to activate the item at the given
// signed offset from the currently active item (or the cursor, if
// nothing is active yet), wrapping around the ends of the list. Used
// for ctrl+n/ctrl+p (next/previous window). A sidebar with no
// OnActivate is a no-op.
type ActivateOffsetMsg struct {
	Delta int
}

// ActivateNextActivityMsg asks a sidebar to activate the next item,
// scanning forward and wrapping from the active item, whose
// SidebarConfig.HasActivity predicate reports true. Used for alt+a
// (next window with activity). A sidebar with no OnActivate or no
// HasActivity predicate is a no-op.
type ActivateNextActivityMsg struct{}

// Sidebar renders a scrollable, sorted list of items with
// cursor and active tracking by identity key. It is backed by a
// *set.Sorted[T] and never copies or rebuilds the item list.
type Sidebar[T set.Lesser[T], K comparable] struct {
	items     *set.Sorted[T]
	cfg       SidebarConfig[T, K]
	cursor    K
	active    K
	cursorIdx int
	activeIdx int
	hasActive bool
	viewport  viewport.Model
	header    string
	empty     string
	bounds    ui.Rect
	minWidth  int
	itemStyle lipgloss.Style
	keyMap    SidebarKeyMap
}

// NewSidebar creates a sidebar backed by the given sorted set.
func NewSidebar[T set.Lesser[T], K comparable](
	items *set.Sorted[T],
	cfg SidebarConfig[T, K],
) Sidebar[T, K] {
	return Sidebar[T, K]{
		items:     items,
		cfg:       cfg,
		activeIdx: -1,
		viewport:  viewport.New(0, 0),
		itemStyle: defaultItemStyle,
		keyMap:    DefaultSidebarKeyMap,
	}
}

// SetItems replaces the backing sorted set. The cursor and active
// keys are preserved if they still exist; otherwise the cursor
// clamps to the nearest neighbour.
func (s Sidebar[T, K]) SetItems(items *set.Sorted[T]) Sidebar[T, K] {
	s.items = items
	s.revalidate()

	return s
}

// SetHeader returns a sidebar with the given header text.
func (s Sidebar[T, K]) SetHeader(h string) Sidebar[T, K] {
	s.header = h

	return s
}

// SetEmpty returns a sidebar with the given empty placeholder.
func (s Sidebar[T, K]) SetEmpty(e string) Sidebar[T, K] {
	s.empty = e

	return s
}

// SetMinWidth returns a sidebar with a minimum rendering width.
func (s Sidebar[T, K]) SetMinWidth(w int) Sidebar[T, K] {
	s.minWidth = w

	return s
}

// SetKeyMap returns a sidebar with custom key bindings.
func (s Sidebar[T, K]) SetKeyMap(km SidebarKeyMap) Sidebar[T, K] {
	s.keyMap = km

	return s
}

// SetActiveKey sets the active item by key and moves the cursor
// to it. Returns the sidebar unchanged if the key is not found.
func (s Sidebar[T, K]) SetActiveKey(k K) Sidebar[T, K] {
	idx := s.findIndex(k)
	if idx < 0 {
		return s
	}

	s.active = k
	s.activeIdx = idx
	s.hasActive = true
	s.cursor = k
	s.cursorIdx = idx

	return s
}

// CursorKey returns the identity key of the item under the cursor.
func (s Sidebar[T, K]) CursorKey() K {
	return s.cursor
}

// ActiveKey returns the identity key of the active item.
func (s Sidebar[T, K]) ActiveKey() K {
	return s.active
}

// Init implements ui.Model.
func (s Sidebar[T, K]) Init() tea.Cmd {
	return nil
}

// Update implements ui.Model.
func (s Sidebar[T, K]) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case ui.BoundsMsg:
		s.bounds = msg.Rect

		return s, nil

	case tea.KeyMsg:
		switch {
		case ui.Matches(msg, s.keyMap.Down):
			s.moveCursor(1)

			return s, nil
		case ui.Matches(msg, s.keyMap.Up):
			s.moveCursor(-1)

			return s, nil
		case ui.Matches(msg, s.keyMap.Select):
			cmd := s.activateIndex(s.cursorIdx)

			return s, cmd
		}

	case tea.MouseMsg:
		return s.handleMouse(msg)

	case ActivateIndexMsg:
		if s.cfg.OnActivate == nil {
			return s, nil
		}

		return s, s.activateAt(msg.Index)

	case ActivateOffsetMsg:
		if s.cfg.OnActivate == nil || s.items == nil || s.items.Len() == 0 {
			return s, nil
		}

		return s, s.activateAt(s.wrapIndex(s.baseIndex() + msg.Delta))

	case ActivateNextActivityMsg:
		if s.cfg.OnActivate == nil || s.cfg.HasActivity == nil {
			return s, nil
		}

		return s.activateNextActivity()
	}

	return s, nil
}

// renderRows renders every row rowLayout produced, a group label or
// an item styled by its selection/activation state, and reports the
// widest rendered row plus the row the cursor item landed on (-1 if
// the cursor's key matched no row), so View can place the viewport's
// scroll offset by row rather than by item index. A row whose item
// no longer exists in the sorted set is dropped.
func (s Sidebar[T, K]) renderRows(rows []sidebarRow, width, pad int) (rendered []string, naturalW int, cursorRow int) {
	rendered = make([]string, 0, len(rows))
	cursorRow = -1

	for _, row := range rows {
		text, isCursor, ok := s.renderRow(row, width-pad)
		if !ok {
			continue
		}

		if isCursor {
			cursorRow = len(rendered)
		}

		rendered = append(rendered, text)

		if rw := lipgloss.Width(text); rw > naturalW {
			naturalW = rw
		}
	}

	return rendered, naturalW, cursorRow
}

// renderRow renders one row: a group label, or an item styled by its
// selection/activation state. ok is false only when row names an
// item no longer present in the sorted set, in which case text and
// isCursor are meaningless. isCursor reports whether this item's key
// is the cursor's, the signal renderRows uses to place the
// viewport's scroll offset.
func (s Sidebar[T, K]) renderRow(row sidebarRow, itemWidth int) (text string, isCursor, ok bool) {
	if row.ItemIndex < 0 {
		return theme.SidebarSection.Render(row.Label), false, true
	}

	item, ok := s.items.GetAt(row.ItemIndex)
	if !ok {
		return "", false, false
	}

	k := s.cfg.Key(item)

	return s.cfg.View(item, s.stateFor(k), itemWidth), k == s.cursor, true
}

// stateFor reports the ViewState an item's key renders with: active
// and/or under the cursor.
func (s Sidebar[T, K]) stateFor(k K) ViewState {
	switch {
	case k == s.active && k == s.cursor:
		return StateActiveSelected
	case k == s.active:
		return StateActive
	case k == s.cursor:
		return StateSelected
	default:
		return StateNone
	}
}

// View implements ui.Model.
func (s Sidebar[T, K]) View(width, height int) string {
	if s.minWidth > 0 && width < s.minWidth {
		width = s.minWidth
	}

	pad := s.padding()

	if s.items == nil || s.items.Len() == 0 {
		empty := s.empty
		if empty == "" {
			empty = "Empty"
		}

		return lipgloss.Place(width, height,
			lipgloss.Center, lipgloss.Center,
			theme.Dim.Render(empty))
	}

	// Render each row (group labels and items alike), tracking the
	// widest, and remember which row the cursor item landed on so the
	// viewport can scroll to its row rather than its item index.
	renderedRows, naturalW, cursorRow := s.renderRows(s.rowLayout(), width, pad)

	if s.header != "" {
		if hw := lipgloss.Width(s.header); hw > naturalW {
			naturalW = hw
		}
	}

	panelW := min(naturalW+pad, width)
	contentW := panelW - pad

	var headerStr string
	var headerHeight int

	if s.header != "" {
		headerStr = s.itemStyle.
			Width(panelW).
			Render(theme.Dim.Render(theme.Bold.Render(s.header)))
		headerHeight = lipgloss.Height(headerStr)
	}

	listHeight := max(height-headerHeight, 0)
	lineStyle := s.itemStyle.MaxWidth(panelW)

	var b strings.Builder

	for i, text := range renderedRows {
		if lipgloss.Width(text) > contentW {
			text = ansi.Truncate(text, contentW, "…")
		}

		b.WriteString(lineStyle.Render(text))

		if i < len(renderedRows)-1 {
			b.WriteByte('\n')
		}
	}

	s.viewport.Width = panelW
	s.viewport.Height = listHeight
	s.viewport.SetContent(b.String())

	if cursorRow >= 0 {
		if cursorRow < s.viewport.YOffset {
			s.viewport.SetYOffset(cursorRow)
		} else if cursorRow >= s.viewport.YOffset+listHeight {
			s.viewport.SetYOffset(cursorRow - listHeight + 1)
		}
	}

	if headerStr != "" {
		return lipgloss.JoinVertical(lipgloss.Left, headerStr, s.viewport.View())
	}

	return s.viewport.View()
}

// KeyBindings returns the sidebar's key bindings for the status bar.
func (s Sidebar[T, K]) KeyBindings() []ui.KeyBinding {
	hasItems := s.items != nil && s.items.Len() > 0

	downHelp := s.keyMap.Down.Help()
	upHelp := s.keyMap.Up.Help()
	combinedKey := downHelp.Key + "/" + upHelp.Key
	combinedDesc := downHelp.Desc

	return []ui.KeyBinding{
		ui.WithBindingEnabled(
			ui.Bind(key.NewBinding(
				key.WithKeys(append(s.keyMap.Up.Keys(), s.keyMap.Down.Keys()...)...),
				key.WithHelp(combinedKey, combinedDesc),
			)),
			hasItems,
		),
		ui.WithBindingEnabled(s.keyMap.Select, hasItems),
	}
}

func (s *Sidebar[T, K]) moveCursor(delta int) {
	if s.items == nil || s.items.Len() == 0 {
		return
	}

	newIdx := s.cursorIdx + delta
	newIdx = max(0, min(newIdx, s.items.Len()-1))

	if newIdx == s.cursorIdx {
		return
	}

	s.cursorIdx = newIdx

	if item, ok := s.items.GetAt(newIdx); ok {
		s.cursor = s.cfg.Key(item)
	}
}

func (s *Sidebar[T, K]) activateIndex(idx int) tea.Cmd {
	if s.items == nil || idx < 0 || idx >= s.items.Len() {
		return nil
	}

	s.activeIdx = idx
	s.hasActive = true

	item, ok := s.items.GetAt(idx)
	if !ok {
		return nil
	}

	s.active = s.cfg.Key(item)

	if s.cfg.OnActivate != nil {
		return s.cfg.OnActivate(item)
	}

	return nil
}

// activateAt moves the cursor to idx and activates the item there.
// Used by a direct mouse click and by the messages that jump straight
// to an item (ActivateIndexMsg, ActivateOffsetMsg,
// ActivateNextActivityMsg).
func (s *Sidebar[T, K]) activateAt(idx int) tea.Cmd {
	if s.items == nil || idx < 0 || idx >= s.items.Len() {
		return nil
	}

	s.cursorIdx = idx
	if item, ok := s.items.GetAt(idx); ok {
		s.cursor = s.cfg.Key(item)
	}

	return s.activateIndex(idx)
}

// baseIndex is the item ActivateOffsetMsg and ActivateNextActivityMsg
// count from: the active item if there is one, otherwise the cursor.
func (s Sidebar[T, K]) baseIndex() int {
	if s.hasActive {
		return s.activeIdx
	}

	return s.cursorIdx
}

// wrapIndex wraps idx into [0, s.items.Len()).
func (s Sidebar[T, K]) wrapIndex(idx int) int {
	n := s.items.Len()
	if n == 0 {
		return 0
	}

	return ((idx % n) + n) % n
}

func (s Sidebar[T, K]) activateNextActivity() (Sidebar[T, K], tea.Cmd) {
	n := s.items.Len()
	if n == 0 {
		return s, nil
	}

	start := s.baseIndex()

	for i := 1; i <= n; i++ {
		idx := s.wrapIndex(start + i)

		item, ok := s.items.GetAt(idx)
		if ok && s.cfg.HasActivity(item) {
			return s, s.activateAt(idx)
		}
	}

	return s, nil
}

func (s *Sidebar[T, K]) revalidate() {
	if s.items == nil || s.items.Len() == 0 {
		s.cursorIdx = 0
		s.activeIdx = -1
		s.hasActive = false

		return
	}

	// Revalidate cursor.
	s.cursorIdx = s.findIndex(s.cursor)

	// Revalidate active.
	if s.hasActive {
		idx := s.findIndex(s.active)
		if idx >= 0 {
			s.activeIdx = idx
		} else {
			s.activeIdx = -1
			s.hasActive = false
		}
	}
}

func (s Sidebar[T, K]) findIndex(k K) int {
	if s.items == nil {
		return -1
	}

	idx := 0

	for item := range s.items.All() {
		if s.cfg.Key(item) == k {
			return idx
		}

		idx++
	}

	// Key not found — clamp to last valid position.
	if s.items.Len() > 0 {
		return min(idx-1, s.items.Len()-1)
	}

	return 0
}

func (s Sidebar[T, K]) handleMouse(msg tea.MouseMsg) (Sidebar[T, K], tea.Cmd) {
	switch {
	case msg.Button == tea.MouseButtonWheelUp:
		if !s.bounds.Contains(msg.X, msg.Y) {
			return s, nil
		}

		s.moveCursor(-1)

		return s, nil

	case msg.Button == tea.MouseButtonWheelDown:
		if !s.bounds.Contains(msg.X, msg.Y) {
			return s, nil
		}

		s.moveCursor(1)

		return s, nil

	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft:
		if !s.bounds.Contains(msg.X, msg.Y) {
			return s, nil
		}

		_, localY := s.bounds.Local(msg.X, msg.Y)
		headerHeight := s.renderHeaderHeight()
		rowIdx := localY - headerHeight + s.viewport.YOffset

		rows := s.rowLayout()
		if rowIdx < 0 || rowIdx >= len(rows) {
			return s, nil
		}

		itemIdx := rows[rowIdx].ItemIndex
		if itemIdx < 0 {
			// A click on a group label row selects nothing.
			return s, nil
		}

		return s, s.activateAt(itemIdx)
	}

	return s, nil
}

func (s Sidebar[T, K]) renderHeaderHeight() int {
	if s.header == "" || s.bounds.Width <= 0 {
		return 0
	}

	headerStr := s.itemStyle.
		Width(s.bounds.Width).
		Render(theme.Dim.Render(theme.Bold.Render(s.header)))

	return lipgloss.Height(headerStr)
}

func (s Sidebar[T, K]) padding() int {
	fw, _ := s.itemStyle.GetFrameSize()

	return fw
}
