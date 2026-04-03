package ui_test

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

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
	r, ok := m.(ui.Root)
	if !ok {
		t.Fatalf("Update returned %T, want ui.Root", m)
	}

	return r
}

func TestRoot_View_delegates_to_screen(t *testing.T) {
	screen := fakeScreen{label: "test"}
	root := ui.NewRoot(screen)
	root = update(t, root, tea.WindowSizeMsg{Width: 80, Height: 24})

	got := root.View()
	want := "test:80x24"

	if got != want {
		t.Errorf("View() = %q, want %q", got, want)
	}
}

func TestRoot_View_nil_screen(t *testing.T) {
	root := ui.NewRoot(nil)

	got := root.View()
	if got != "" {
		t.Errorf("View() with nil screen = %q, want empty", got)
	}
}

func TestRoot_ScreenMsg_switches_screen(t *testing.T) {
	first := fakeScreen{label: "first"}
	second := fakeScreen{label: "second"}

	root := ui.NewRoot(first)
	root = update(t, root, tea.WindowSizeMsg{Width: 40, Height: 10})
	root = update(t, root, ui.ScreenMsg{Screen: second})

	got := root.View()
	want := "second:40x10"

	if got != want {
		t.Errorf("View() after ScreenMsg = %q, want %q", got, want)
	}
}

func TestRoot_ctrl_c_quits(t *testing.T) {
	root := ui.NewRoot(nil)
	_, cmd := root.Update(tea.KeyMsg{Type: tea.KeyCtrlC})

	if cmd == nil {
		t.Fatal("expected quit command, got nil")
	}

	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
}
