package components_test

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/set"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
)

// sidebarItem is a minimal set.Lesser element used to exercise
// components.Sidebar on its own terms, independent of any real
// domain type.
type sidebarItem struct {
	name     string
	activity bool
}

func (i sidebarItem) Less(other sidebarItem) bool { return i.name < other.name }

// newTestSidebar builds a Sidebar over the given names (in the order
// given; Sidebar sorts them by name) and returns it alongside the
// slice of names OnActivate was called with, in call order.
func newTestSidebar(names ...string) (components.Sidebar[sidebarItem, string], *[]string) {
	items := set.NewSorted[sidebarItem]()
	for _, n := range names {
		items.Insert(sidebarItem{name: n})
	}

	activated := &[]string{}

	sb := components.NewSidebar(items, components.SidebarConfig[sidebarItem, string]{
		Key:  func(i sidebarItem) string { return i.name },
		View: func(i sidebarItem, _ components.ViewState, _ int) string { return i.name },
		OnActivate: func(i sidebarItem) tea.Cmd {
			*activated = append(*activated, i.name)
			return func() tea.Msg { return nil }
		},
		HasActivity: func(i sidebarItem) bool { return i.activity },
	})

	return sb, activated
}

func sidebarUpdate(t *testing.T, sb components.Sidebar[sidebarItem, string], msg tea.Msg) (components.Sidebar[sidebarItem, string], tea.Cmd) {
	t.Helper()

	updated, cmd := sb.Update(msg)
	next, ok := updated.(components.Sidebar[sidebarItem, string])
	require.True(t, ok, "expected components.Sidebar[sidebarItem, string], got %T", updated)

	if cmd != nil {
		cmd()
	}

	return next, cmd
}

func TestSidebar_mouse_wheel_outside_bounds_is_ignored(t *testing.T) {
	sb, _ := newTestSidebar("alpha", "beta", "gamma")
	sb = sb.SetActiveKey("alpha")
	sb, _ = sidebarUpdate(t, sb, ui.BoundsMsg{Rect: ui.Rect{X: 0, Y: 0, Width: 20, Height: 10}})

	require.Equal(t, "alpha", sb.CursorKey())

	sb, _ = sidebarUpdate(t, sb, tea.MouseMsg{
		X: 50, Y: 50, // well outside the 20x10 rect at (0, 0)
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonWheelDown,
	})

	require.Equal(t, "alpha", sb.CursorKey(), "a wheel event outside the sidebar's bounds must not move the cursor")
}

func TestSidebar_mouse_wheel_inside_bounds_moves_cursor(t *testing.T) {
	sb, _ := newTestSidebar("alpha", "beta", "gamma")
	sb, _ = sidebarUpdate(t, sb, ui.BoundsMsg{Rect: ui.Rect{X: 0, Y: 0, Width: 20, Height: 10}})

	sb, cmd := sidebarUpdate(t, sb, tea.MouseMsg{
		X: 2, Y: 1,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonWheelDown,
	})

	require.Equal(t, "beta", sb.CursorKey())
	require.Nil(t, cmd, "wheel scroll moves the cursor but does not activate")
}

func TestSidebar_ActivateIndexMsg(t *testing.T) {
	sb, activated := newTestSidebar("alpha", "beta", "gamma")

	sb, cmd := sidebarUpdate(t, sb, components.ActivateIndexMsg{Index: 2})

	require.NotNil(t, cmd)
	require.Equal(t, []string{"gamma"}, *activated)
	require.Equal(t, "gamma", sb.ActiveKey())
	require.Equal(t, "gamma", sb.CursorKey(), "activating by index also moves the cursor to it")
}

func TestSidebar_ActivateIndexMsg_out_of_range_is_a_no_op(t *testing.T) {
	sb, activated := newTestSidebar("alpha", "beta")

	sb, cmd := sidebarUpdate(t, sb, components.ActivateIndexMsg{Index: 5})

	require.Nil(t, cmd)
	require.Empty(t, *activated)
	require.Equal(t, "", sb.ActiveKey())
}

func TestSidebar_ActivateOffsetMsg_steps_and_wraps(t *testing.T) {
	sb, activated := newTestSidebar("alpha", "beta", "gamma")
	sb, _ = sidebarUpdate(t, sb, components.ActivateIndexMsg{Index: 0})

	sb, _ = sidebarUpdate(t, sb, components.ActivateOffsetMsg{Delta: 1})
	require.Equal(t, "beta", sb.ActiveKey())

	sb, _ = sidebarUpdate(t, sb, components.ActivateOffsetMsg{Delta: 1})
	require.Equal(t, "gamma", sb.ActiveKey())

	// Wraps from the last item back to the first.
	sb, _ = sidebarUpdate(t, sb, components.ActivateOffsetMsg{Delta: 1})
	require.Equal(t, "alpha", sb.ActiveKey())

	// And backward from the first wraps to the last.
	sb, _ = sidebarUpdate(t, sb, components.ActivateOffsetMsg{Delta: -1})
	require.Equal(t, "gamma", sb.ActiveKey())

	require.Equal(t, []string{"alpha", "beta", "gamma", "alpha", "gamma"}, *activated)
}

func TestSidebar_ActivateNextActivityMsg_finds_and_wraps(t *testing.T) {
	items := set.NewSorted[sidebarItem]()
	items.Insert(sidebarItem{name: "alpha"})
	items.Insert(sidebarItem{name: "beta", activity: true})
	items.Insert(sidebarItem{name: "gamma"})

	activated := &[]string{}
	sb := components.NewSidebar(items, components.SidebarConfig[sidebarItem, string]{
		Key:  func(i sidebarItem) string { return i.name },
		View: func(i sidebarItem, _ components.ViewState, _ int) string { return i.name },
		OnActivate: func(i sidebarItem) tea.Cmd {
			*activated = append(*activated, i.name)
			return func() tea.Msg { return nil }
		},
		HasActivity: func(i sidebarItem) bool { return i.activity },
	})

	sb, _ = sidebarUpdate(t, sb, components.ActivateIndexMsg{Index: 2}) // gamma

	// Scanning forward from gamma wraps past alpha to beta, the only
	// item with activity.
	sb, cmd := sidebarUpdate(t, sb, components.ActivateNextActivityMsg{})

	require.NotNil(t, cmd)
	require.Equal(t, "beta", sb.ActiveKey())
	require.Equal(t, []string{"gamma", "beta"}, *activated)
}

func TestSidebar_ActivateNextActivityMsg_no_activity_is_a_no_op(t *testing.T) {
	sb, activated := newTestSidebar("alpha", "beta")
	sb, _ = sidebarUpdate(t, sb, components.ActivateIndexMsg{Index: 0})

	sb, cmd := sidebarUpdate(t, sb, components.ActivateNextActivityMsg{})

	require.Nil(t, cmd)
	require.Equal(t, "alpha", sb.ActiveKey(), "no item has activity, so the active item does not change")
	require.Equal(t, []string{"alpha"}, *activated)
}

func TestSidebar_window_switch_messages_are_no_ops_without_OnActivate(t *testing.T) {
	items := set.NewSorted[sidebarItem]()
	items.Insert(sidebarItem{name: "alpha"})
	items.Insert(sidebarItem{name: "beta", activity: true})

	// No OnActivate set, matching the nick list's configuration: it
	// has no activation semantics, so these messages must not move
	// its cursor or mutate its active state.
	sb := components.NewSidebar(items, components.SidebarConfig[sidebarItem, string]{
		Key:         func(i sidebarItem) string { return i.name },
		View:        func(i sidebarItem, _ components.ViewState, _ int) string { return i.name },
		HasActivity: func(i sidebarItem) bool { return i.activity },
	})

	sb, cmd := sidebarUpdate(t, sb, components.ActivateIndexMsg{Index: 1})
	require.Nil(t, cmd)
	require.Equal(t, "", sb.ActiveKey())

	sb, cmd = sidebarUpdate(t, sb, components.ActivateOffsetMsg{Delta: 1})
	require.Nil(t, cmd)
	require.Equal(t, "", sb.ActiveKey())

	_, cmd = sidebarUpdate(t, sb, components.ActivateNextActivityMsg{})
	require.Nil(t, cmd)
}

func TestSidebar_click_activates_and_moves_cursor(t *testing.T) {
	sb, activated := newTestSidebar("alpha", "beta", "gamma")
	sb, _ = sidebarUpdate(t, sb, ui.BoundsMsg{Rect: ui.Rect{X: 0, Y: 0, Width: 20, Height: 10}})

	sb, cmd := sidebarUpdate(t, sb, tea.MouseMsg{
		X: 2, Y: 1,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})

	require.NotNil(t, cmd)
	require.Equal(t, "beta", sb.CursorKey())
	require.Equal(t, "beta", sb.ActiveKey())
	require.Equal(t, []string{"beta"}, *activated)
}
