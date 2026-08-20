package ui

import (
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui/uitest"
)

// fakeClock is a manually-advanced clock for deterministic tests of
// Root's quit-confirm window, which is a real wall-clock duration and
// so cannot be driven by a fixed number of Update calls.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

// stubScreen is a minimal Model for exercising Root's own routing and
// rendering in isolation from any real screen.
type stubScreen struct {
	label string
}

func (s stubScreen) Init() tea.Cmd { return nil }

func (s stubScreen) Update(tea.Msg) (Model, tea.Cmd) { return s, nil }

func (s stubScreen) View(width, height int) string {
	return fmt.Sprintf("%s:%dx%d", s.label, width, height)
}

// rootFrame is the whole rendered frame as lines, which for a
// stubScreen is fully determined: the banners Root chose, followed by
// the size it gave the screen.
func rootFrame(r Root) []string {
	return uitest.RenderedLines(r.View())
}

func updateRoot(t *testing.T, r Root, msg tea.Msg) Root {
	t.Helper()

	m, _ := r.Update(msg)
	next, ok := m.(Root)
	require.True(t, ok, "expected Root, got %T", m)

	return next
}

func newRootWithClock(screen Model, clock *fakeClock) Root {
	r := NewRoot(screen)
	r.now = clock.Now

	return r
}

func TestRoot_ctrl_c_arms_quit_confirmation_without_quitting(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	r := newRootWithClock(stubScreen{label: "test"}, clock)
	r = updateRoot(t, r, tea.WindowSizeMsg{Width: 80, Height: 24})

	updated, cmd := r.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	r = updated.(Root)

	require.Nil(t, cmd, "the first Ctrl-C must arm the confirmation, not quit")
	require.Equal(t, []string{quitConfirmBanner, "test:80x23"}, rootFrame(r))
}

func TestRoot_second_ctrl_c_within_window_quits(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	r := newRootWithClock(stubScreen{label: "test"}, clock)
	r = updateRoot(t, r, tea.WindowSizeMsg{Width: 80, Height: 24})

	r = updateRoot(t, r, tea.KeyMsg{Type: tea.KeyCtrlC})
	clock.Advance(time.Second)

	_, cmd := r.Update(tea.KeyMsg{Type: tea.KeyCtrlC})

	require.NotNil(t, cmd)
	require.Equal(t, QuitRequestedMsg{Message: "client exited"}, cmd())
}

func TestRoot_second_ctrl_c_after_window_rearms_instead_of_quitting(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	r := newRootWithClock(stubScreen{label: "test"}, clock)
	r = updateRoot(t, r, tea.WindowSizeMsg{Width: 80, Height: 24})

	r = updateRoot(t, r, tea.KeyMsg{Type: tea.KeyCtrlC})
	clock.Advance(quitConfirmWindow + time.Second)

	updated, cmd := r.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	r = updated.(Root)

	require.Nil(t, cmd, "a Ctrl-C after the confirmation window elapsed must arm a fresh confirmation, not quit")
	require.Equal(t, []string{quitConfirmBanner, "test:80x23"}, rootFrame(r))
}

func TestRoot_ToggleMouse_flips_state_and_returns_the_matching_cmd(t *testing.T) {
	r := NewRoot(stubScreen{label: "test"})
	r = updateRoot(t, r, tea.WindowSizeMsg{Width: 80, Height: 24})

	require.Equal(t, []string{"test:80x24"}, rootFrame(r))

	updated, cmd := r.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}, Alt: true})
	r = updated.(Root)

	require.NotNil(t, cmd)
	require.NotNil(t, cmd(), "toggling off must send tea.DisableMouse")
	require.Equal(t, []string{mouseOffBanner, "test:80x23"}, rootFrame(r),
		"the banner takes a row from the screen while it is shown")

	updated, cmd = r.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}, Alt: true})
	r = updated.(Root)

	require.NotNil(t, cmd)
	require.NotNil(t, cmd(), "toggling back on must send tea.EnableMouseCellMotion")
	require.Equal(t, []string{"test:80x24"}, rootFrame(r))
}

func TestRoot_KeyBindings_include_quit_and_toggle_mouse(t *testing.T) {
	r := NewRoot(stubScreen{label: "test"})

	var helpKeys []string
	for _, b := range r.KeyBindings() {
		helpKeys = append(helpKeys, b.Help().Key)
	}

	require.Equal(t, []string{"^C", "M-m"}, helpKeys)
}
