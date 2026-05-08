package components_test

import (
	"fmt"
	"testing"

	bkey "github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
)

// Layout dimensions used across the MainLayout tests. These track
// the current width-split heuristics; if the heuristics change
// (e.g. a wider sidebar, different nicklist width) update these
// constants in one place rather than touching every test.
const (
	sidebarWidthAt80           = 16
	sidebarWidthAt100          = 20
	sidebarWidthAt120          = 24
	contentWidthAt80           = 66
	contentWidthAt80WithNicks  = 54
	contentWidthAt100          = 86
	contentWidthAt120TwoPane   = 106
	contentWidthAt120WithNicks = 94
	nickListWidthAt80          = 12
	nickListWidthAt120         = 18
	obsDrawerHeight            = 8
	obsDrawerColumnHeight      = 16
	defaultTestHeight          = 24
)

// stubModel is a minimal ui.Model for testing layout behaviour.
type stubModel struct {
	label string
}

func (s stubModel) Init() tea.Cmd { return nil }

func (s stubModel) Update(tea.Msg) (ui.Model, tea.Cmd) {
	return s, nil
}

func (s stubModel) View(width, height int) string {
	return fmt.Sprintf("%s:%dx%d", s.label, width, height)
}

// dims formats a `label:WxH` dimension token matching stubModel.View,
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

func TestMainLayout_View_responsive(t *testing.T) {
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
			got := layout.View(tt.width, tt.height)
			columns := visibleColumns(got)

			require.Equal(t, tt.wantSidebar, nonEmptyColumn(columns[0]))
			require.Equal(t, tt.wantContent, nonEmptyColumn(columns[1]))
		})
	}
}

func TestMainLayout_View_narrow_terminal(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}
	layout := components.NewMainLayout(sidebar, content)

	t.Run("below threshold shows resize message", func(t *testing.T) {
		got := layout.View(79, 24)

		require.Equal(t, []string{"Resize terminal to 80+ columns"}, visibleLines(got))
	})

	t.Run("at threshold renders normally", func(t *testing.T) {
		got := layout.View(80, defaultTestHeight)
		columns := visibleColumns(got)

		require.Equal(t, []string{dims("sidebar", sidebarWidthAt80, defaultTestHeight)}, nonEmptyColumn(columns[0]))
		require.Equal(t, []string{dims("content", contentWidthAt80, defaultTestHeight)}, nonEmptyColumn(columns[1]))
	})
}

func TestMainLayout_View_fills_width(t *testing.T) {
	sidebar := stubModel{label: "S"}
	content := stubModel{label: "C"}

	layout := components.NewMainLayout(sidebar, content)
	got := layout.View(100, 24)

	renderedWidth := lipgloss.Width(got)
	require.LessOrEqual(t, renderedWidth, 100)
}

func TestMainLayout_View_preserves_height(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}
	layout := components.NewMainLayout(sidebar, content)

	got := layout.View(80, 24)

	require.Equal(t, 24, lipgloss.Height(got))
}

func TestMainLayout_View_three_pane_at_wide_width(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}
	nicklist := stubModel{label: "nicks"}

	layout := components.NewMainLayout(sidebar, content)
	layout.NickList = nicklist

	got := layout.View(120, defaultTestHeight)
	columns := visibleColumns(got)

	require.Equal(t, []string{dims("sidebar", sidebarWidthAt120, defaultTestHeight)}, nonEmptyColumn(columns[0]))
	require.Equal(t, []string{dims("content", contentWidthAt120WithNicks, defaultTestHeight)}, nonEmptyColumn(columns[1]))
	require.Equal(t, []string{dims("nicks", nickListWidthAt120, defaultTestHeight)}, nonEmptyColumn(columns[2]))
}

func TestMainLayout_View_hides_nicklist_when_main_too_narrow(t *testing.T) {
	// Use a wide sidebar stub and nicklist stub that, together with
	// the nick list, squeeze the main area below minMainWidth.
	// The layout should hide the nick list to reclaim space.
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}
	nicklist := stubModel{label: "nicks"}

	layout := components.NewMainLayout(sidebar, content)
	layout.NickList = nicklist

	// At 80 columns with small stubs everything fits.
	got := layout.View(80, defaultTestHeight)
	require.Equal(t, [][]string{
		{dims("sidebar", sidebarWidthAt80, defaultTestHeight)},
		{dims("content", contentWidthAt80WithNicks, defaultTestHeight)},
		{dims("nicks", nickListWidthAt80, defaultTestHeight)},
	}, columnContents(visibleColumns(got)))

	// Toggle it off — the nick list column must disappear.
	toggled, _ := layout.Update(components.NickListToggleMsg{})
	got = toggled.View(80, 24)
	require.Equal(t, [][]string{
		{dims("sidebar", sidebarWidthAt80, defaultTestHeight)},
		{dims("content", contentWidthAt80, defaultTestHeight)},
	}, columnContents(visibleColumns(got)))
}

func TestMainLayout_View_nicklist_toggle(t *testing.T) {
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
	require.Equal(t, withNicks, columnContents(visibleColumns(layout.View(120, defaultTestHeight))))

	// Toggle off.
	updated, _ := layout.Update(components.NickListToggleMsg{})
	layout = updated.(components.MainLayout)

	require.Equal(t, withoutNicks, columnContents(visibleColumns(layout.View(120, defaultTestHeight))))

	// Toggle back on.
	updated, _ = layout.Update(components.NickListToggleMsg{})
	layout = updated.(components.MainLayout)

	require.Equal(t, withNicks, columnContents(visibleColumns(layout.View(120, defaultTestHeight))))
}

func TestMainLayout_View_no_nicklist_without_set(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}

	layout := components.NewMainLayout(sidebar, content)

	got := layout.View(120, defaultTestHeight)

	columns := visibleColumns(got)
	require.Equal(t, []string{dims("sidebar", sidebarWidthAt120, defaultTestHeight)}, nonEmptyColumn(columns[0]))
	require.Equal(t, []string{dims("content", contentWidthAt120TwoPane, defaultTestHeight)}, nonEmptyColumn(columns[1]))
}

func TestMainLayout_View_three_pane_fills_width(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := stubModel{label: "content"}
	nicklist := stubModel{label: "nicks"}

	layout := components.NewMainLayout(sidebar, content)
	layout.NickList = nicklist

	got := layout.View(120, 24)

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

// obsStubModel is a stubModel that also acts as an ObsProvider,
// simulating the ChatWorkspace's observability drawer.
type obsStubModel struct {
	stubModel

	obsOpen   bool
	obsHeight int
}

func (o obsStubModel) ObsView(width, height int) string {
	if !o.obsOpen {
		return ""
	}

	return lipgloss.Place(width, height, lipgloss.Left, lipgloss.Top,
		fmt.Sprintf("obs:%dx%d", width, height))
}

func (o obsStubModel) ObsHeight(_ int) int {
	if !o.obsOpen {
		return 0
	}

	return o.obsHeight
}

func TestMainLayout_View_obs_closed_height_matches(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := obsStubModel{
		stubModel: stubModel{label: "content"},
		obsOpen:   false,
	}

	layout := components.NewMainLayout(sidebar, content)
	got := layout.View(120, defaultTestHeight)

	require.Equal(t, defaultTestHeight, lipgloss.Height(got))
	require.Equal(t, []string{dims("sidebar", sidebarWidthAt120, defaultTestHeight)}, nonEmptyColumn(visibleColumns(got)[0]))
	require.Equal(t, []string{dims("content", contentWidthAt120TwoPane, defaultTestHeight)}, nonEmptyColumn(visibleColumns(got)[1]))
}

func TestMainLayout_View_obs_open_spans_full_width(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := obsStubModel{
		stubModel: stubModel{label: "content"},
		obsOpen:   true,
		obsHeight: obsDrawerHeight,
	}

	layout := components.NewMainLayout(sidebar, content)
	got := layout.View(120, defaultTestHeight)

	require.Equal(t, defaultTestHeight, lipgloss.Height(got))
	require.Equal(t, []string{
		dims("sidebar", sidebarWidthAt120, obsDrawerColumnHeight),
		dims("obs", 120, obsDrawerHeight),
	}, nonEmptyColumn(visibleColumns(got)[0]))
	require.Equal(t, []string{
		dims("content", contentWidthAt120TwoPane, obsDrawerColumnHeight),
	}, nonEmptyColumn(visibleColumns(got)[1]))
}

func TestMainLayout_View_obs_open_reduces_column_height(t *testing.T) {
	sidebar := stubModel{label: "sidebar"}
	content := obsStubModel{
		stubModel: stubModel{label: "content"},
		obsOpen:   true,
		obsHeight: obsDrawerHeight,
	}

	layout := components.NewMainLayout(sidebar, content)
	got := layout.View(120, defaultTestHeight)

	columns := visibleColumns(got)
	require.Equal(t, []string{
		dims("sidebar", sidebarWidthAt120, obsDrawerColumnHeight),
		dims("obs", 120, obsDrawerHeight),
	}, nonEmptyColumn(columns[0]))
	require.Equal(t, []string{
		dims("content", contentWidthAt120TwoPane, obsDrawerColumnHeight),
	}, nonEmptyColumn(columns[1]))
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
	}, layout.KeyBindings())
}
