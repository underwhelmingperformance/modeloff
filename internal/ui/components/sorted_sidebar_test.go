package components_test

import (
	"strings"
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

// sectionedItem is a set.Lesser element carrying a group label, used
// to exercise SidebarConfig.Section.
type sectionedItem struct {
	name    string
	section string
}

func (i sectionedItem) Less(other sectionedItem) bool { return i.name < other.name }

// newSectionedTestSidebar builds a Sidebar whose items are already
// sorted (by name) into contiguous runs per section, the way
// [domain.Window]'s ordering groups DMs after channels.
func newSectionedTestSidebar(items ...sectionedItem) (components.Sidebar[sectionedItem, string], *[]string) {
	set := set.NewSorted[sectionedItem]()
	for _, it := range items {
		set.Insert(it)
	}

	activated := &[]string{}

	sb := components.NewSidebar(set, components.SidebarConfig[sectionedItem, string]{
		Key:  func(i sectionedItem) string { return i.name },
		View: func(i sectionedItem, _ components.ViewState, _ int) string { return i.name },
		OnActivate: func(i sectionedItem) tea.Cmd {
			*activated = append(*activated, i.name)
			return func() tea.Msg { return nil }
		},
		Section: func(i sectionedItem) string { return i.section },
	})

	return sb, activated
}

func TestSidebar_renders_a_group_label_once_per_section_transition(t *testing.T) {
	sb, _ := newSectionedTestSidebar(
		sectionedItem{name: "alpha", section: ""},
		sectionedItem{name: "beta", section: ""},
		sectionedItem{name: "carol", section: "Queries"},
		sectionedItem{name: "dave", section: "Queries"},
	)

	view := sb.View(20, 10)
	lines := trimmedLines(view)

	// "alpha" and "beta" carry no section label (empty string means
	// no label row); "Queries" is drawn once, immediately above the
	// first item whose section differs from the previous item's.
	require.Contains(t, lines, "alpha")
	require.Contains(t, lines, "beta")
	require.Contains(t, lines, "Queries")
	require.Contains(t, lines, "carol")
	require.Contains(t, lines, "dave")

	queriesIdx := indexOfLine(lines, "Queries")
	carolIdx := indexOfLine(lines, "carol")
	require.Equal(t, queriesIdx+1, carolIdx, "the label sits immediately above the first item in its section")

	// Exactly one "Queries" label, not one per item in the section.
	count := 0
	for _, l := range lines {
		if l == "Queries" {
			count++
		}
	}
	require.Equal(t, 1, count)
}

func trimmedLines(view string) []string {
	lines := strings.Split(view, "\n")
	out := make([]string, len(lines))

	for i, l := range lines {
		out[i] = strings.TrimSpace(l)
	}

	return out
}

func indexOfLine(lines []string, want string) int {
	for i, l := range lines {
		if l == want {
			return i
		}
	}
	return -1
}

func TestSidebar_click_on_group_label_is_a_no_op(t *testing.T) {
	sb, activated := newSectionedTestSidebar(
		sectionedItem{name: "alpha", section: ""},
		sectionedItem{name: "carol", section: "Queries"},
	)
	sb, _ = sidebarUpdateSectioned(t, sb, ui.BoundsMsg{Rect: ui.Rect{X: 0, Y: 0, Width: 20, Height: 10}})

	// Row 0 is "alpha", row 1 is the "Queries" label, row 2 is "carol".
	sb, cmd := sidebarUpdateSectioned(t, sb, tea.MouseMsg{
		X: 2, Y: 1,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})

	require.Nil(t, cmd, "a click on a group label activates nothing")
	require.Empty(t, *activated)
	require.Equal(t, "", sb.ActiveKey())
}

func TestSidebar_click_on_item_after_group_label_activates_it(t *testing.T) {
	sb, activated := newSectionedTestSidebar(
		sectionedItem{name: "alpha", section: ""},
		sectionedItem{name: "carol", section: "Queries"},
	)
	sb, _ = sidebarUpdateSectioned(t, sb, ui.BoundsMsg{Rect: ui.Rect{X: 0, Y: 0, Width: 20, Height: 10}})

	// Row 2 is "carol", past the "Queries" label at row 1.
	sb, cmd := sidebarUpdateSectioned(t, sb, tea.MouseMsg{
		X: 2, Y: 2,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})

	require.NotNil(t, cmd)
	require.Equal(t, []string{"carol"}, *activated)
	require.Equal(t, "carol", sb.ActiveKey())
}

func sidebarUpdateSectioned(t *testing.T, sb components.Sidebar[sectionedItem, string], msg tea.Msg) (components.Sidebar[sectionedItem, string], tea.Cmd) {
	t.Helper()

	updated, cmd := sb.Update(msg)
	next, ok := updated.(components.Sidebar[sectionedItem, string])
	require.True(t, ok, "expected components.Sidebar[sectionedItem, string], got %T", updated)

	if cmd != nil {
		cmd()
	}

	return next, cmd
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
