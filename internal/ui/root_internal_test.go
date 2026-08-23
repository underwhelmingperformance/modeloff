package ui

import (
	"fmt"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
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

// stubScreen is a minimal Component for exercising Root's own routing and
// rendering in isolation from any real screen.
type stubScreen struct {
	label    string
	bindings []KeyBinding
}

func (s stubScreen) Init() tea.Cmd { return nil }

func (s stubScreen) Update(tea.Msg) (Component, tea.Cmd) { return s, nil }

func (s stubScreen) render(width, height int) string {
	return fmt.Sprintf("%s:%dx%d", s.label, width, height)
}

func (s stubScreen) Draw(screen uv.Screen, area uv.Rectangle) {
	uv.NewStyledString(s.render(area.Dx(), area.Dy())).Draw(screen, area)
}

func (s stubScreen) KeyBindings() []KeyBinding { return s.bindings }

type focusScreen struct {
	focused bool
}

func (s focusScreen) Init() tea.Cmd { return nil }

func (s focusScreen) Update(msg tea.Msg) (Component, tea.Cmd) {
	switch msg.(type) {
	case tea.FocusMsg:
		s.focused = true
	case tea.BlurMsg:
		s.focused = false
	}

	return s, nil
}

func (s focusScreen) Draw(screen uv.Screen, area uv.Rectangle) {
	style := lipgloss.NewStyle().Reverse(s.focused)
	uv.NewStyledString(style.Render(" ")).Draw(screen, uv.Rect(area.Min.X, area.Max.Y-1, 1, 1))
}

// rootFrame is the whole rendered frame as lines, which for a
// stubScreen is fully determined: the banners Root chose, followed by
// the size it gave the screen.
func rootFrame(r Root) []string {
	return uitest.NonEmptyLines(r.View().Content)
}

func updateRoot(t *testing.T, r Root, msg tea.Msg) Root {
	t.Helper()

	m, _ := r.Update(msg)
	next, ok := m.(Root)
	require.True(t, ok, "expected Root, got %T", m)

	return next
}

func newRootWithClock(screen Component, clock *fakeClock) Root {
	r := NewRoot(screen)
	r.now = clock.Now

	return r
}

func TestRoot_ctrl_c_arms_quit_confirmation_without_quitting(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	r := newRootWithClock(stubScreen{label: "test"}, clock)
	r = updateRoot(t, r, tea.WindowSizeMsg{Width: 80, Height: 24})

	updated, cmd := r.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	r = updated.(Root)

	require.NotNil(t, cmd, "the first Ctrl-C must schedule expiry of the confirmation")
	require.Equal(t, []string{quitConfirmBanner, "test:80x23"}, rootFrame(r))
}

func TestRoot_second_ctrl_c_within_window_quits(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	r := newRootWithClock(stubScreen{label: "test"}, clock)
	r = updateRoot(t, r, tea.WindowSizeMsg{Width: 80, Height: 24})

	r = updateRoot(t, r, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	clock.Advance(time.Second)

	_, cmd := r.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})

	require.NotNil(t, cmd)
	require.Equal(t, QuitRequestedMsg{Message: "client exited"}, cmd())
}

func TestRoot_second_ctrl_c_after_window_rearms_instead_of_quitting(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	r := newRootWithClock(stubScreen{label: "test"}, clock)
	r = updateRoot(t, r, tea.WindowSizeMsg{Width: 80, Height: 24})

	r = updateRoot(t, r, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	clock.Advance(quitConfirmWindow + time.Second)

	updated, cmd := r.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	r = updated.(Root)

	require.NotNil(t, cmd, "a fresh confirmation must schedule its own expiry")
	require.Equal(t, []string{quitConfirmBanner, "test:80x23"}, rootFrame(r))
}

func TestRoot_quit_confirmation_expiry_restores_child_bounds(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	r := newRootWithClock(stubScreen{label: "test"}, clock)
	r = updateRoot(t, r, tea.WindowSizeMsg{Width: 80, Height: 24})
	r = updateRoot(t, r, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	armedAt := r.quitArmedAt

	clock.Advance(quitConfirmWindow)
	r = updateRoot(t, r, quitConfirmationExpiredMsg{armedAt: armedAt})

	require.Equal(t, []string{"test:80x24"}, rootFrame(r))
}

func TestRoot_keeps_quit_banner_bounds_until_expiry_message_arrives(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	r := newRootWithClock(stubScreen{label: "test"}, clock)
	r = updateRoot(t, r, tea.WindowSizeMsg{Width: 80, Height: 24})
	r = updateRoot(t, r, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})

	clock.Advance(quitConfirmWindow + time.Second)
	r = updateRoot(t, r, "unrelated message")

	require.Equal(t, []string{quitConfirmBanner, "test:80x23"}, rootFrame(r))
}

func TestRoot_ToggleMouse_changes_the_declared_mouse_mode(t *testing.T) {
	r := NewRoot(stubScreen{label: "test"})
	r = updateRoot(t, r, tea.WindowSizeMsg{Width: 80, Height: 24})

	require.Equal(t, []string{"test:80x24"}, rootFrame(r))

	updated, cmd := r.Update(tea.KeyPressMsg{Code: 'm', Mod: tea.ModAlt})
	r = updated.(Root)

	require.Nil(t, cmd)
	require.Equal(t, tea.MouseModeNone, r.View().MouseMode)
	require.Equal(t, []string{mouseOffBanner, "test:80x23"}, rootFrame(r),
		"the banner takes a row from the screen while it is shown")

	updated, cmd = r.Update(tea.KeyPressMsg{Code: 'm', Mod: tea.ModAlt})
	r = updated.(Root)

	require.Nil(t, cmd)
	require.Equal(t, tea.MouseModeCellMotion, r.View().MouseMode)
	require.Equal(t, []string{"test:80x24"}, rootFrame(r))
}

func TestRoot_KeyBindings_include_application_bindings(t *testing.T) {
	r := NewRoot(stubScreen{label: "test"})

	var helpKeys []string
	for _, b := range r.KeyBindings() {
		helpKeys = append(helpKeys, b.Help().Key)
	}

	require.Equal(t, []string{"^C", "M-m", "F1"}, helpKeys)
}

func TestRoot_F1_renders_keyboard_help_from_key_bindings(t *testing.T) {
	r := NewRoot(stubScreen{
		label: "test",
		bindings: []KeyBinding{
			Bind(key.NewBinding(
				key.WithKeys("ctrl+n"),
				key.WithHelp("^N", "next window"),
			)).WithHelpMetadata(KeyHelpNavigation, KeyHintHigh),
			Bind(key.NewBinding(
				key.WithKeys("ctrl+w"),
				key.WithHelp("^W", "delete word"),
			)).WithHelpMetadata(KeyHelpEditing, KeyHintNone),
		},
	})
	r = updateRoot(t, r, tea.WindowSizeMsg{Width: 60, Height: 20})
	r = updateRoot(t, r, tea.KeyPressMsg{Code: tea.KeyF1})

	view := r.View().Content
	require.Contains(t, view, "╭")
	require.Equal(t, []string{
		"test:60x20",
		"Keyboard shortcuts                    F1/Esc close",
		"Navigation",
		"^N    next window",
		"Editing",
		"^W    delete word",
		"Application",
		"^C    quit",
		"M-m   mouse",
		"F1    shortcuts",
	}, uitest.NonBorderSegments(view))

	r = updateRoot(t, r, tea.KeyPressMsg{Code: tea.KeyEsc})

	require.Equal(t, []string{"test:60x20"}, rootFrame(r))
}

func TestRoot_keyboard_help_blurs_and_refocuses_the_screen(t *testing.T) {
	r := NewRoot(focusScreen{focused: true})
	r = updateRoot(t, r, tea.WindowSizeMsg{Width: 80, Height: 24})

	cellAtScreenEdge := func() *uv.Cell {
		screen := uv.NewScreenBuffer(80, 24)
		r.draw(screen, screen.Bounds())

		return screen.CellAt(0, 23)
	}

	require.NotZero(t, cellAtScreenEdge().Style.Attrs&uv.AttrReverse)

	r = updateRoot(t, r, tea.KeyPressMsg{Code: tea.KeyF1})

	require.Zero(t, cellAtScreenEdge().Style.Attrs&uv.AttrReverse,
		"the screen outside the modal must not retain an active focus indicator")

	r = updateRoot(t, r, tea.FocusMsg{})

	require.Zero(t, cellAtScreenEdge().Style.Attrs&uv.AttrReverse,
		"terminal focus must not refocus the screen behind the modal")

	r = updateRoot(t, r, tea.KeyPressMsg{Code: tea.KeyEsc})

	require.NotZero(t, cellAtScreenEdge().Style.Attrs&uv.AttrReverse)
}

func TestRoot_keyboard_help_scrolls_to_the_last_binding(t *testing.T) {
	bindings := make([]KeyBinding, 50)
	for i := range bindings {
		bindings[i] = Bind(key.NewBinding(
			key.WithKeys("x"),
			key.WithHelp(fmt.Sprintf("K%d", i), fmt.Sprintf("action %d", i)),
		)).WithHelpMetadata(KeyHelpGeneral, KeyHintNone)
	}

	r := NewRoot(stubScreen{label: "test", bindings: bindings})
	r = updateRoot(t, r, tea.WindowSizeMsg{Width: 80, Height: 40})
	r = updateRoot(t, r, tea.KeyPressMsg{Code: tea.KeyF1})

	for range 10 {
		r = updateRoot(t, r, tea.KeyPressMsg{Code: tea.KeyPgDown})
	}

	require.Contains(t, r.View().Content, "action 49")
}

func TestRoot_keyboard_help_remains_dismissible_in_tiny_bounds(t *testing.T) {
	r := NewRoot(stubScreen{label: "test"})
	r = updateRoot(t, r, tea.WindowSizeMsg{Width: 8, Height: 4})
	r = updateRoot(t, r, tea.KeyPressMsg{Code: tea.KeyF1})

	require.Contains(t, r.View().Content, "╭")

	r = updateRoot(t, r, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, []string{"test:8x4"}, rootFrame(r))
}
