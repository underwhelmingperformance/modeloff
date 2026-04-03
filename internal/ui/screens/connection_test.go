package screens_test

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/screens"
)

func tick(t *testing.T, m ui.Model) (ui.Model, tea.Cmd) {
	t.Helper()

	return m.Update(screens.ConnectionTickMsg{})
}

func view(m ui.Model) string {
	return m.View(80, 24)
}

func TestConnectionScreen_with_api_key(t *testing.T) {
	s := screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey:    true,
		ChannelCount: 3,
		Nick:         "alice",
	})

	// Initial view: first step is shown as pending.
	require.Contains(t, view(s), "Connecting to modeloff")

	// Tick 1: "Connecting" completes.
	var m ui.Model = s
	m, cmd := tick(t, m)

	v := view(m)
	require.Contains(t, v, "✓")
	require.Contains(t, v, "Connecting to modeloff")
	require.Contains(t, v, "Checking configuration")
	require.NotNil(t, cmd)

	// Tick 2: "Checking configuration" completes.
	m, cmd = tick(t, m)

	v = view(m)
	require.Contains(t, v, "Loading channels (3 found)")
	require.NotNil(t, cmd)

	// Tick 3: "Loading channels" completes.
	m, cmd = tick(t, m)

	v = view(m)
	require.Contains(t, v, "Welcome, alice")
	require.NotNil(t, cmd)

	// Tick 4: "Welcome" completes — final cmd should be ConnectionDoneMsg.
	_, cmd = tick(t, m)
	require.NotNil(t, cmd)

	msg := cmd()
	_, ok := msg.(screens.ConnectionDoneMsg)
	require.True(t, ok, "expected ConnectionDoneMsg, got %T", msg)
}

func TestConnectionScreen_no_api_key(t *testing.T) {
	s := screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey: false,
		Nick:      "bob",
	})

	// Tick 1: "Connecting" completes.
	var m ui.Model = s
	m, cmd := tick(t, m)
	require.NotNil(t, cmd)

	// Tick 2: "Checking configuration" completes, error step appears.
	m, cmd = tick(t, m)

	v := view(m)
	require.Contains(t, v, "✗")
	require.Contains(t, v, "/config")
	require.NotNil(t, cmd)

	// Tick 3: error step is reached — no further ticks, no done msg.
	_, cmd = tick(t, m)
	require.Nil(t, cmd)
}

func TestConnectionScreen_Init_returns_tick_cmd(t *testing.T) {
	s := screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey: true,
		Nick:      "user",
	})

	require.NotNil(t, s.Init())
}

func TestConnectionScreen_ignores_other_messages(t *testing.T) {
	s := screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey: true,
		Nick:      "user",
	})

	var m ui.Model = s
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})

	require.Nil(t, cmd)
	require.Contains(t, view(m), "Connecting to modeloff")
}
