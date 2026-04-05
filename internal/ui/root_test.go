package ui_test

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui"
)

type fakeScreen struct {
	label string
}

func (f fakeScreen) Init() tea.Cmd { return nil }

func (f fakeScreen) Update(tea.Msg) (ui.Model, tea.Cmd) {
	return f, nil
}

func (f fakeScreen) View(width, height int) string {
	return fmt.Sprintf("%s:%dx%d", f.label, width, height)
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

	require.Equal(t, "test:80x24", root.View())
}

func TestRoot_View_nil_screen(t *testing.T) {
	root := ui.NewRoot(nil)

	require.Empty(t, root.View())
}

func TestRoot_ScreenMsg_switches_screen(t *testing.T) {
	first := fakeScreen{label: "first"}
	second := fakeScreen{label: "second"}

	root := ui.NewRoot(first)
	root = update(t, root, tea.WindowSizeMsg{Width: 40, Height: 10})
	root = update(t, root, ui.ScreenMsg{Screen: second})

	require.Equal(t, "second:40x10", root.View())
}
