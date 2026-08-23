package components_test

import (
	"fmt"
	"testing"

	bkey "charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
)

// Layout dimensions used across the MainLayout tests. These track
// the current width-split heuristics; if the heuristics change
// (e.g. a wider sidebar, different nicklist width) update these
// constants in one place, and every test that uses them follows.
const (
	sidebarWidthAt80           = 15
	sidebarWidthAt100          = 19
	sidebarWidthAt120          = 23
	contentWidthAt80           = 64
	contentWidthAt80WithNicks  = 52
	contentWidthAt100          = 80
	contentWidthAt120TwoPane   = 96
	contentWidthAt120WithNicks = 78
	nickListWidthAt80          = 11
	nickListWidthAt120         = 17
	defaultTestHeight          = 24
)

// stubModel is a minimal ui.Component for testing layout behaviour.
type stubModel struct {
	label string
}

func (s stubModel) Init() tea.Cmd { return nil }

func (s stubModel) Update(tea.Msg) (ui.Component, tea.Cmd) {
	return s, nil
}

func (s stubModel) render(width, height int) string {
	return fmt.Sprintf("%s:%dx%d", s.label, width, height)
}

func (s stubModel) ContentWidth() int { return 1_000 }

func (s stubModel) Draw(screen uv.Screen, area uv.Rectangle) {
	uv.NewStyledString(s.render(area.Dx(), area.Dy())).Draw(screen, area)
}

// boundsRecordingStub is a stubModel that also records the last
// [ui.BoundsMsg] it received, so a test can pin the Rect MainLayout
// computed for it without exporting computeLayout.
type boundsRecordingStub struct {
	stubModel

	bounds *uv.Rectangle
}

func newBoundsRecordingStub(label string) boundsRecordingStub {
	return boundsRecordingStub{stubModel: stubModel{label: label}, bounds: new(uv.Rectangle)}
}

func (s boundsRecordingStub) Update(msg tea.Msg) (ui.Component, tea.Cmd) {
	if b, ok := msg.(ui.BoundsMsg); ok {
		*s.bounds = b.Rect
	}

	return s, nil
}

// dims formats a `label:WxH` dimension token matching stubModel.Draw,
// so tests can express expectations in terms of the layout constants.
func dims(label string, width, height int) string {
	return fmt.Sprintf("%s:%dx%d", label, width, height)
}

// columnContents reduces a [][]string from visibleColumns to its
// nonEmptyColumn projection, so full-slice assertions can compare the
// number of columns and their content in one structural check.
func columnContents(columns [][]string) [][]string {
	out := make([][]string, len(columns))
	for i, col := range columns {
		out[i] = nonEmptyColumn(col)
	}

	return out
}

type keybindingStubModel struct {
	stubModel

	bindings []ui.KeyBinding
}

func (s keybindingStubModel) KeyBindings() []ui.KeyBinding {
	return s.bindings
}

func TestMainLayout_Draw_responsive(t *testing.T) {
	tests := []struct {
		name        string
		width       int
		height      int
		wantSidebar []string
		wantContent []string
	}{
		{
			name:        "sidebar and content are both rendered",
			width:       80,
			height:      defaultTestHeight,
			wantSidebar: []string{dims("sidebar", sidebarWidthAt80, defaultTestHeight)},
			wantContent: []string{dims("content", contentWidthAt80, defaultTestHeight)},
		},
		{
			name:        "content width adjusts with terminal size",
			width:       100,
			height:      defaultTestHeight,
			wantSidebar: []string{dims("sidebar", sidebarWidthAt100, defaultTestHeight)},
			wantContent: []string{dims("content", contentWidthAt100, defaultTestHeight)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sidebar := stubModel{label: "sidebar"}
			content := stubModel{label: "content"}

			layout := components.NewMainLayout(sidebar, content)
			got := renderToBuffer(layout, tt.width, tt.height)
			columns := visibleColumns(got)

			require.Equal(t, tt.wantSidebar, nonEmptyColumn(columns[0]))
			require.Equal(t, tt.wantContent, nonEmptyColumn(columns[1]))
		})
	}
}

func TestMainLayout_Draw_narrow_terminal(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}
	layout := components.NewMainLayout(sidebar, content)

	t.Run("below threshold collapses the sidebar and gives content the full width", func(t *testing.T) {
		got := renderToBuffer(layout, 79, 24)

		// No sidebar border ("│") appears at all: Content is the only
		// thing rendered, at the full 79 columns.
		require.NotContains(t, got, "│")
		require.Equal(t, []string{dims("content", 79, 24)}, visibleLines(got))
	})

	t.Run("collapses at any width, not just above zero", func(t *testing.T) {
		got := renderToBuffer(layout, 20, 10)

		require.Equal(t, []string{dims("content", 20, 10)}, visibleLines(got))
	})

	t.Run("at threshold renders normally", func(t *testing.T) {
		got := renderToBuffer(layout, 80, defaultTestHeight)
		columns := visibleColumns(got)

		require.Equal(t, []string{dims("sidebar", sidebarWidthAt80, defaultTestHeight)}, nonEmptyColumn(columns[0]))
		require.Equal(t, []string{dims("content", contentWidthAt80, defaultTestHeight)}, nonEmptyColumn(columns[1]))
	})
}

// TestMainLayout_BoundsMsg_narrow_terminal pins the BoundsMsg
// Content receives below the compact threshold: the full terminal
// width, matching what Draw actually renders it at. A Rect that
// disagreed with the rendered width would misplace mouse
// hit-testing inside Content even though the collapsed columns
// looked right.
func TestMainLayout_BoundsMsg_narrow_terminal(t *testing.T) {
	sidebar := newBoundsRecordingStub("sidebar")
	content := newBoundsRecordingStub("content")
	layout := components.NewMainLayout(sidebar, content)

	updated, _ := layout.Update(ui.BoundsMsg{Rect: uv.Rect(0, 0, 60, 20)})
	layout = updated.(components.MainLayout)

	require.Equal(t, uv.Rect(0, 0, 60, 20), *content.bounds,
		"Content's bounds must span the full width the collapsed layout renders it at")
	require.Equal(t, uv.Rect(0, 0, 0, 20), *sidebar.bounds,
		"the collapsed sidebar gets a zero-width Rect, matching that Draw never renders it")

	got := renderToBuffer(layout, 60, 20)
	require.Equal(t, []string{dims("content", 60, 20)}, visibleLines(got))
}

func TestMainLayout_BoundsMsg_uses_absolute_draw_rectangles(t *testing.T) {
	sidebar := newBoundsRecordingStub("sidebar")
	content := newBoundsRecordingStub("content")
	nickList := newBoundsRecordingStub("nicks")
	layout := components.NewMainLayout(sidebar, content)
	layout.NickList = nickList

	updated, _ := layout.Update(ui.BoundsMsg{Rect: uv.Rect(5, 3, 120, 20)})
	_ = updated.(components.MainLayout)

	require.Equal(t, uv.Rect(5, 3, sidebarWidthAt120, 20), *sidebar.bounds)
	require.Equal(t, uv.Rect(29, 3, contentWidthAt120WithNicks, 20), *content.bounds)
	require.Equal(t, uv.Rect(108, 3, nickListWidthAt120, 20), *nickList.bounds)
}

func TestMainLayout_Draw_fills_width(t *testing.T) {
	sidebar := stubModel{label: "S"}
	content := stubModel{label: "C"}

	layout := components.NewMainLayout(sidebar, content)
	got := renderToBuffer(layout, 100, 24)

	renderedWidth := lipgloss.Width(got)
	require.LessOrEqual(t, renderedWidth, 100)
}

func TestMainLayout_Draw_preserves_height(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}
	layout := components.NewMainLayout(sidebar, content)

	got := renderToBuffer(layout, 80, 24)

	require.Equal(t, 24, lipgloss.Height(got))
}

func TestMainLayout_Draw_three_pane_at_wide_width(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}
	nicklist := stubModel{label: "nicks"}

	layout := components.NewMainLayout(sidebar, content)
	layout.NickList = nicklist

	got := renderToBuffer(layout, 120, defaultTestHeight)
	columns := visibleColumns(got)

	require.Equal(t, []string{dims("sidebar", sidebarWidthAt120, defaultTestHeight)}, nonEmptyColumn(columns[0]))
	require.Equal(t, []string{dims("content", contentWidthAt120WithNicks, defaultTestHeight)}, nonEmptyColumn(columns[1]))
	require.Equal(t, []string{dims("nicks", nickListWidthAt120, defaultTestHeight)}, nonEmptyColumn(columns[2]))
}

func TestMainLayout_Draw_hides_nicklist_when_main_too_narrow(t *testing.T) {
	// Use a wide sidebar stub and nicklist stub that, together with
	// the nick list, squeeze the main area below minMainWidth.
	// The layout should hide the nick list to reclaim space.
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}
	nicklist := stubModel{label: "nicks"}

	layout := components.NewMainLayout(sidebar, content)
	layout.NickList = nicklist

	// At 80 columns with small stubs everything fits.
	got := renderToBuffer(layout, 80, defaultTestHeight)
	require.Equal(t, [][]string{
		{dims("sidebar", sidebarWidthAt80, defaultTestHeight)},
		{dims("content", contentWidthAt80WithNicks, defaultTestHeight)},
		{dims("nicks", nickListWidthAt80, defaultTestHeight)},
	}, columnContents(visibleColumns(got)))

	// Toggle it off — the nick list column must disappear.
	toggled, _ := layout.Update(components.NickListToggleMsg{})
	got = renderToBuffer(toggled, 80, 24)
	require.Equal(t, [][]string{
		{dims("sidebar", sidebarWidthAt80, defaultTestHeight)},
		{dims("content", contentWidthAt80, defaultTestHeight)},
	}, columnContents(visibleColumns(got)))
}

func TestMainLayout_Draw_nicklist_toggle(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}
	nicklist := stubModel{label: "nicks"}

	layout := components.NewMainLayout(sidebar, content)
	layout.NickList = nicklist

	withNicks := [][]string{
		{dims("sidebar", sidebarWidthAt120, defaultTestHeight)},
		{dims("content", contentWidthAt120WithNicks, defaultTestHeight)},
		{dims("nicks", nickListWidthAt120, defaultTestHeight)},
	}
	withoutNicks := [][]string{
		{dims("sidebar", sidebarWidthAt120, defaultTestHeight)},
		{dims("content", contentWidthAt120TwoPane, defaultTestHeight)},
	}

	// Initially visible at wide width.
	require.Equal(t, withNicks, columnContents(visibleColumns(renderToBuffer(layout, 120, defaultTestHeight))))

	// Toggle off.
	updated, _ := layout.Update(components.NickListToggleMsg{})
	layout = updated.(components.MainLayout)

	require.Equal(t, withoutNicks, columnContents(visibleColumns(renderToBuffer(layout, 120, defaultTestHeight))))

	// Toggle back on.
	updated, _ = layout.Update(components.NickListToggleMsg{})
	layout = updated.(components.MainLayout)

	require.Equal(t, withNicks, columnContents(visibleColumns(renderToBuffer(layout, 120, defaultTestHeight))))
}

func TestMainLayout_Draw_no_nicklist_without_set(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}

	layout := components.NewMainLayout(sidebar, content)

	got := renderToBuffer(layout, 120, defaultTestHeight)

	columns := visibleColumns(got)
	require.Equal(t, []string{dims("sidebar", sidebarWidthAt120, defaultTestHeight)}, nonEmptyColumn(columns[0]))
	require.Equal(t, []string{dims("content", contentWidthAt120TwoPane, defaultTestHeight)}, nonEmptyColumn(columns[1]))
}

func TestMainLayout_Draw_three_pane_fills_width(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}
	nicklist := stubModel{label: "nicks"}

	layout := components.NewMainLayout(sidebar, content)
	layout.NickList = nicklist

	got := renderToBuffer(layout, 120, 24)

	renderedWidth := lipgloss.Width(got)
	require.LessOrEqual(t, renderedWidth, 120,
		"three-pane rendered width must not exceed total width")
}

// initModel is a stubModel that returns a command from Init.
type initModel struct {
	stubModel

	initMsg string
}

func (m initModel) Init() tea.Cmd {
	msg := m.initMsg
	return func() tea.Msg { return msg }
}

func TestMainLayout_Init_batches_children(t *testing.T) {
	sidebar := initModel{stubModel: stubModel{label: "sidebar"}, initMsg: "s"}
	content := initModel{stubModel: stubModel{label: "content"}, initMsg: "c"}

	layout := components.NewMainLayout(sidebar, content)
	cmd := layout.Init()

	require.NotNil(t, cmd)
}

func TestMainLayout_KeyBindings_collects_from_children(t *testing.T) {
	sidebar := keybindingStubModel{
		stubModel: stubModel{label: "sidebar"},
		bindings: []ui.KeyBinding{
			ui.Bind(bkey.NewBinding(bkey.WithKeys("ctrl+d"), bkey.WithHelp("^D", "channels"))),
		},
	}
	content := keybindingStubModel{
		stubModel: stubModel{label: "content"},
		bindings: []ui.KeyBinding{
			ui.Bind(bkey.NewBinding(bkey.WithKeys("pgup"), bkey.WithHelp("PgUp", "scroll"))),
		},
	}

	layout := components.NewMainLayout(sidebar, content)

	require.Equal(t, []ui.KeyBinding{
		ui.Bind(bkey.NewBinding(bkey.WithKeys("ctrl+d"), bkey.WithHelp("^D", "channels"))),
		ui.Bind(bkey.NewBinding(bkey.WithKeys("pgup"), bkey.WithHelp("PgUp", "scroll"))),
		components.DefaultWindowSwitchKeyMap.Direct,
		components.DefaultWindowSwitchKeyMap.NextActivity,
		components.DefaultWindowSwitchKeyMap.Next,
		components.DefaultWindowSwitchKeyMap.Previous,
	}, layout.KeyBindings())
}

// recordingModel is a ui.Component that records every message it
// receives, so a test can assert exactly which child a message
// reached.
type recordingModel struct {
	stubModel

	received *[]tea.Msg
}

func (s recordingModel) Update(msg tea.Msg) (ui.Component, tea.Cmd) {
	*s.received = append(*s.received, msg)
	return s, nil
}

func TestMainLayout_window_switch_keys_reach_only_the_sidebar(t *testing.T) {
	tests := []struct {
		name string
		key  tea.KeyPressMsg
		want tea.Msg
	}{
		{
			name: "alt+3 activates index 2",
			key:  tea.KeyPressMsg{Code: '3', Mod: tea.ModAlt},
			want: components.ActivateIndexMsg{Index: 2},
		},
		{
			name: "lock state does not block alt+3",
			key:  tea.KeyPressMsg{Code: '3', Mod: tea.ModAlt | tea.ModNumLock},
			want: components.ActivateIndexMsg{Index: 2},
		},
		{
			name: "alt+a activates next activity",
			key:  tea.KeyPressMsg{Code: 'a', Mod: tea.ModAlt},
			want: components.ActivateNextActivityMsg{},
		},
		{
			name: "ctrl+n steps forward",
			key:  tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl},
			want: components.ActivateOffsetMsg{Delta: 1},
		},
		{
			name: "ctrl+p steps backward",
			key:  tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl},
			want: components.ActivateOffsetMsg{Delta: -1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sidebarReceived := &[]tea.Msg{}
			contentReceived := &[]tea.Msg{}

			sidebar := recordingModel{stubModel: stubModel{label: "sidebar"}, received: sidebarReceived}
			content := recordingModel{stubModel: stubModel{label: "content"}, received: contentReceived}

			layout := components.NewMainLayout(sidebar, content)
			layout.Update(tt.key)

			require.Equal(t, []tea.Msg{tt.want}, *sidebarReceived)
			require.Empty(t, *contentReceived,
				"a window-switch key must not reach Content, where the input editor could mistake it for a literal character")
		})
	}
}

func TestMainLayout_non_window_switch_keys_reach_all_children(t *testing.T) {
	sidebarReceived := &[]tea.Msg{}
	contentReceived := &[]tea.Msg{}

	sidebar := recordingModel{stubModel: stubModel{label: "sidebar"}, received: sidebarReceived}
	content := recordingModel{stubModel: stubModel{label: "content"}, received: contentReceived}

	layout := components.NewMainLayout(sidebar, content)
	key := tea.KeyPressMsg{Code: 'x', Text: "x"}
	layout.Update(key)

	require.Equal(t, []tea.Msg{key}, *sidebarReceived)
	require.Equal(t, []tea.Msg{key}, *contentReceived)
}

func TestMainLayout_fullscreen_observability_keeps_sidebar_and_hides_chat(t *testing.T) {
	layout := components.NewMainLayout(
		stubModel{label: "sidebar"},
		stubModel{label: "content"},
	).WithObservability(components.NewMetricsPane(t.Context, nil))
	layout.NickList = stubModel{label: "nicks"}

	updated, _ := layout.Update(tea.KeyPressMsg{Code: 'l', Mod: tea.ModAlt})
	layout = updated.(components.MainLayout)
	updated, _ = layout.Update(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl})
	layout = updated.(components.MainLayout)

	view := renderToBuffer(layout, 120, defaultTestHeight)

	require.Contains(t, view, "sidebar")
	require.Contains(t, view, "Logs")
	require.Contains(t, view, "Metrics")
	require.NotContains(t, view, "content")
	require.NotContains(t, view, "nicks")
}

func TestMainLayout_fullscreen_observability_updates_hidden_state_without_hidden_input(t *testing.T) {
	sidebarReceived := &[]tea.Msg{}
	contentReceived := &[]tea.Msg{}
	nickListReceived := &[]tea.Msg{}

	layout := components.NewMainLayout(
		recordingModel{stubModel: stubModel{label: "sidebar"}, received: sidebarReceived},
		recordingModel{stubModel: stubModel{label: "content"}, received: contentReceived},
	).WithObservability(components.NewMetricsPane(t.Context, nil))
	layout.NickList = recordingModel{stubModel: stubModel{label: "nicks"}, received: nickListReceived}

	updated, _ := layout.Update(tea.KeyPressMsg{Code: 'l', Mod: tea.ModAlt})
	layout = updated.(components.MainLayout)
	updated, _ = layout.Update(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl})
	layout = updated.(components.MainLayout)

	*sidebarReceived = nil
	*contentReceived = nil
	*nickListReceived = nil

	updated, _ = layout.Update("state changed")
	layout = updated.(components.MainLayout)

	require.Equal(t, []tea.Msg{"state changed"}, *sidebarReceived)
	require.Equal(t, []tea.Msg{"state changed"}, *contentReceived)
	require.Equal(t, []tea.Msg{"state changed"}, *nickListReceived)

	*sidebarReceived = nil
	*contentReceived = nil
	*nickListReceived = nil

	layout.Update(tea.PasteMsg{Content: "hidden input"})

	require.Empty(t, *sidebarReceived)
	require.Empty(t, *contentReceived)
	require.Empty(t, *nickListReceived)

	key := tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}
	layout.Update(key)

	require.Equal(t, []tea.Msg{key}, *sidebarReceived)
	require.Empty(t, *contentReceived)
	require.Empty(t, *nickListReceived)

	*sidebarReceived = nil

	wheel := tea.MouseWheelMsg{X: 40, Y: 10, Button: tea.MouseWheelUp}
	layout.Update(wheel)

	require.Equal(t, []tea.Msg{wheel}, *sidebarReceived)
	require.Empty(t, *contentReceived)
	require.Empty(t, *nickListReceived)
}
