package ui_test

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui"
)

type fakeScreen struct {
	label string
}

type literalScreen string

func (s literalScreen) Init() tea.Cmd { return nil }

func (s literalScreen) Update(tea.Msg) (ui.Component, tea.Cmd) {
	return s, nil
}

func (s literalScreen) Draw(screen uv.Screen, area uv.Rectangle) {
	uv.NewStyledString(string(s)).Draw(screen, area)
}

type transitioningScreen struct {
	fakeScreen

	next ui.Component
}

func (s transitioningScreen) NextScreen() ui.Component {
	return s.next
}

func (s transitioningScreen) Update(msg tea.Msg) (ui.Component, tea.Cmd) {
	if label, ok := msg.(string); ok {
		s.next = fakeScreen{label: label}
	}

	return s, nil
}

func (f fakeScreen) Init() tea.Cmd { return nil }

func (f fakeScreen) Update(tea.Msg) (ui.Component, tea.Cmd) {
	return f, nil
}

func (f fakeScreen) render(width, height int) string {
	return fmt.Sprintf("%s:%dx%d", f.label, width, height)
}

func (f fakeScreen) Draw(screen uv.Screen, area uv.Rectangle) {
	uv.NewStyledString(f.render(area.Dx(), area.Dy())).Draw(screen, area)
}

func update(t *testing.T, root ui.Root, msg tea.Msg) ui.Root {
	t.Helper()

	m, _ := root.Update(msg)
	require.IsType(t, ui.Root{}, m)

	return m.(ui.Root)
}

func TestRoot_View_delegates_to_screen(t *testing.T) {
	screen := fakeScreen{label: "test"}
	root := ui.NewRoot(screen)
	root = update(t, root, tea.WindowSizeMsg{Width: 80, Height: 24})

	require.Equal(t, tea.View{
		Content:   "test:80x24" + strings.Repeat("\n", 23),
		AltScreen: true,
		MouseMode: tea.MouseModeCellMotion,
	}, root.View())
}

func TestRoot_View_nil_screen(t *testing.T) {
	root := ui.NewRoot(nil)

	require.Equal(t, tea.View{
		AltScreen: true,
		MouseMode: tea.MouseModeCellMotion,
	}, root.View())
}

func TestRoot_View_uses_grapheme_width_for_composition(t *testing.T) {
	root := ui.NewRoot(literalScreen("👨‍👩‍👧‍👦X"))
	root = update(t, root, tea.WindowSizeMsg{Width: 3, Height: 1})

	require.Equal(t, "👨‍👩‍👧‍👦X", root.View().Content)
}

func TestRoot_ScreenMsg_switches_screen(t *testing.T) {
	second := fakeScreen{label: "second"}
	first := transitioningScreen{fakeScreen: fakeScreen{label: "first"}, next: second}

	root := ui.NewRoot(first)
	root = update(t, root, tea.WindowSizeMsg{Width: 40, Height: 10})
	root = update(t, root, ui.ScreenMsg{})

	require.Equal(t, tea.View{
		Content:   "second:40x10" + strings.Repeat("\n", 9),
		AltScreen: true,
		MouseMode: tea.MouseModeCellMotion,
	}, root.View())
}

func TestRoot_ScreenMsg_takes_the_transition_component_current_child(t *testing.T) {
	first := transitioningScreen{
		fakeScreen: fakeScreen{label: "first"},
		next:       fakeScreen{label: "stale"},
	}
	root := ui.NewRoot(first)
	root = update(t, root, tea.WindowSizeMsg{Width: 40, Height: 10})
	root = update(t, root, "current")

	root = update(t, root, ui.ScreenMsg{})

	require.Equal(t, "current:40x10"+strings.Repeat("\n", 9), root.View().Content)
}
