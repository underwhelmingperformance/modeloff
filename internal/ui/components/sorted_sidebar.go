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
}

// Sidebar renders a scrollable, sorted list of items with
// cursor and active tracking by identity key. It is backed by a
// *set.Sorted[T] and never copies or rebuilds the item list.
type Sidebar[T any, K comparable] struct {
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
func NewSidebar[T any, K comparable](
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
	}

	return s, nil
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

	// Render each item, tracking the widest.
	rendered := make([]string, 0, s.items.Len())
	naturalW := 0

	if s.header != "" {
		if hw := lipgloss.Width(s.header); hw > naturalW {
			naturalW = hw
		}
	}

	idx := 0

	for item := range s.items.All() {
		k := s.cfg.Key(item)

		state := StateNone

		switch {
		case k == s.active && k == s.cursor:
			state = StateActiveSelected
		case k == s.active:
			state = StateActive
		case k == s.cursor:
			state = StateSelected
		}

		text := s.cfg.View(item, state, width-pad)
		rendered = append(rendered, text)

		if rw := lipgloss.Width(text); rw > naturalW {
			naturalW = rw
		}

		idx++
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

	for i, text := range rendered {
		if lipgloss.Width(text) > contentW {
			text = ansi.Truncate(text, contentW, "…")
		}

		b.WriteString(lineStyle.Render(text))

		if i < len(rendered)-1 {
			b.WriteByte('\n')
		}
	}

	s.viewport.Width = panelW
	s.viewport.Height = listHeight
	s.viewport.SetContent(b.String())

	if s.cursorIdx < s.viewport.YOffset {
		s.viewport.SetYOffset(s.cursorIdx)
	} else if s.cursorIdx >= s.viewport.YOffset+listHeight {
		s.viewport.SetYOffset(s.cursorIdx - listHeight + 1)
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
		s.moveCursor(-1)

		return s, nil

	case msg.Button == tea.MouseButtonWheelDown:
		s.moveCursor(1)

		return s, nil

	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft:
		if !s.bounds.Contains(msg.X, msg.Y) {
			return s, nil
		}

		_, localY := s.bounds.Local(msg.X, msg.Y)
		headerHeight := s.renderHeaderHeight()
		itemIdx := localY - headerHeight + s.viewport.YOffset

		if itemIdx < 0 || itemIdx >= s.items.Len() {
			return s, nil
		}

		s.cursorIdx = itemIdx

		if item, ok := s.items.GetAt(itemIdx); ok {
			s.cursor = s.cfg.Key(item)
		}

		return s, s.activateIndex(s.cursorIdx)
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
