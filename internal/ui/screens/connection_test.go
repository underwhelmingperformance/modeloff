package screens_test

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/screens"
	"github.com/laney/modeloff/internal/ui/uitest"
)

type stubScreen struct{}

func (s *stubScreen) Init() tea.Cmd                      { return nil }
func (s *stubScreen) Update(tea.Msg) (ui.Model, tea.Cmd) { return s, nil }
func (s *stubScreen) View(int, int) string               { return "stub" }

func tick(t *testing.T, m ui.Model) (ui.Model, tea.Cmd) {
	t.Helper()

	return m.Update(screens.ConnectionTickMsg{})
}

func view(m ui.Model) string {
	return m.View(80, 24)
}

func TestConnectionScreen_with_api_key(t *testing.T) {
	next := &stubScreen{}
	s := screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey:    true,
		ChannelCount: 3,
		Nick:         "alice",
	}, next)

	// Initial view: first step is shown as pending.
	require.Equal(t, []string{"… Connecting to modeloff"}, uitest.TrimmedVisibleLines(view(s)))

	// Tick 1: "Connecting" completes.
	var m ui.Model = s
	m, cmd := tick(t, m)

	v := view(m)
	require.Equal(t, []string{
		"✓ Connecting to modeloff",
		"… Checking configuration",
	}, uitest.TrimmedVisibleLines(v))
	require.NotNil(t, cmd)

	// Tick 2: "Checking configuration" completes.
	m, cmd = tick(t, m)

	v = view(m)
	require.Equal(t, []string{
		"✓ Connecting to modeloff",
		"✓ Checking configuration",
		"… Loading channels (3 found)",
	}, uitest.TrimmedVisibleLines(v))
	require.NotNil(t, cmd)

	// Tick 3: "Loading channels" completes.
	m, cmd = tick(t, m)

	v = view(m)
	require.Equal(t, []string{
		"✓ Connecting to modeloff",
		"✓ Checking configuration",
		"✓ Loading channels (3 found)",
		"… Loading models",
	}, uitest.TrimmedVisibleLines(v))
	require.NotNil(t, cmd)

	// Tick 4: "Loading models" completes (animation-only mode
	// short-circuits the load gate).
	m, cmd = tick(t, m)

	v = view(m)
	require.Equal(t, []string{
		"✓ Connecting to modeloff",
		"✓ Checking configuration",
		"✓ Loading channels (3 found)",
		"✓ Loading models",
		"… Joining channels",
	}, uitest.TrimmedVisibleLines(v))
	require.NotNil(t, cmd)

	// Tick 5: "Joining channels" completes (animation-only mode
	// short-circuits the autojoin gate).
	m, cmd = tick(t, m)

	v = view(m)
	require.Equal(t, []string{
		"✓ Connecting to modeloff",
		"✓ Checking configuration",
		"✓ Loading channels (3 found)",
		"✓ Loading models",
		"✓ Joining channels",
		"… Welcome, alice",
	}, uitest.TrimmedVisibleLines(v))
	require.NotNil(t, cmd)

	// Tick 6: "Welcome" completes — transitions to the next screen.
	_, cmd = tick(t, m)
	require.NotNil(t, cmd)

	// Animation-only mode emits a bare ScreenMsg (no live-models
	// payload to deliver since no session was attached). Root
	// only swaps the active pointer — the wrapped child has been
	// running throughout the animation, so no Init follows.
	msg := cmd()
	require.Equal(t, ui.ScreenMsg{Screen: next}, msg)
}

func TestConnectionScreen_no_api_key(t *testing.T) {
	s := screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey: false,
		Nick:      "bob",
	}, nil)

	// Tick 1: "Connecting" completes.
	var m ui.Model = s
	m, cmd := tick(t, m)
	require.NotNil(t, cmd)

	// Tick 2: "Checking configuration" completes, error step appears.
	m, cmd = tick(t, m)

	v := view(m)
	require.Equal(t, []string{
		"✓ Connecting to modeloff",
		"✓ Checking configuration",
		"✗ No API key configured — use /config to set one",
	}, uitest.TrimmedVisibleLines(v))
	require.NotNil(t, cmd)

	// Tick 3: error step is reached — no further ticks, no done msg.
	_, cmd = tick(t, m)
	require.Nil(t, cmd)
}

func TestConnectionScreen_Init_returns_tick_cmd(t *testing.T) {
	s := screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey: true,
		Nick:      "user",
	}, nil)

	require.NotNil(t, s.Init())
}

func TestConnectionScreen_View_narrow_terminal(t *testing.T) {
	s := screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey: true,
		Nick:      "user",
	}, nil)

	t.Run("below threshold shows resize message", func(t *testing.T) {
		got := s.View(79, 24)

		require.Equal(t, []string{"Resize terminal to 80+ columns"}, uitest.TrimmedVisibleLines(got))
	})

	t.Run("at threshold renders normally", func(t *testing.T) {
		got := s.View(80, 24)

		require.Equal(t, []string{"… Connecting to modeloff"}, uitest.TrimmedVisibleLines(got))
	})
}

func TestConnectionScreen_ignores_other_messages(t *testing.T) {
	s := screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey: true,
		Nick:      "user",
	}, nil)

	var m ui.Model = s
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})

	require.Nil(t, cmd)
	require.Equal(t, []string{"… Connecting to modeloff"}, uitest.TrimmedVisibleLines(view(m)))
}
