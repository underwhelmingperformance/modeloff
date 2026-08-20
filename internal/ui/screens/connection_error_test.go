package screens

import (
	"context"
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui"
)

// fakeConnector is a minimal SessionConnector whose Connect result is
// fixed at construction, so a test can drive the connect gate without
// a real session or store.
type fakeConnector struct{ err error }

func (f fakeConnector) Connect(context.Context) error { return f.err }

// stubChatScreen is a placeholder ui.Model standing in for the real
// chat screen, distinct from nil so a test can tell "no chat screen
// configured" apart from "a chat screen exists but the sequence
// never transitioned to it".
type stubChatScreen struct{}

func (stubChatScreen) Init() tea.Cmd                      { return nil }
func (stubChatScreen) Update(tea.Msg) (ui.Model, tea.Cmd) { return stubChatScreen{}, nil }
func (stubChatScreen) View(int, int) string               { return "stub" }

// TestConnectionScreen_routes_each_gates_error_to_its_own_step pins
// that a gate's failure lands on the step waiting on that gate, not
// on whichever step the animation cursor happens to be showing. The
// three async calls behind gateConnect, gateLoadModels and
// gateAutojoin race each other and the animation only advances a
// gated step once its own signal has arrived, so the cursor can
// still be sitting on an earlier step when a later gate's result
// comes back.
func TestConnectionScreen_routes_each_gates_error_to_its_own_step(t *testing.T) {
	errLoadModels := errors.New("catalogue unreachable")
	errAutojoin := errors.New("list autojoin channels: db closed")

	tests := []struct {
		name string
		gate stepGate
		msg  tea.Msg
	}{
		{name: "load models", gate: gateLoadModels, msg: loadModelsDoneMsg{err: errLoadModels}},
		{name: "autojoin", gate: gateAutojoin, msg: joinAutojoinDoneMsg{err: errAutojoin}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewConnectionScreen(ConnectionConfig{
				HasAPIKey:    true,
				ChannelCount: 1,
				Nick:         "alice",
				Session:      fakeConnector{},
			}, nil)

			// No tick has run, so the animation cursor is still on
			// step 0 ("Connecting to modeloff") when this gate's
			// result arrives — exactly the ordering a slow connect
			// alongside a fast-failing catalogue load or autojoin
			// produces.
			var m ui.Model = s
			m, _ = m.Update(tt.msg)
			cs := m.(ConnectionScreen)

			require.Equal(t, stepPending, cs.steps[0].status,
				"the connect step must not carry an unrelated gate's error")

			idx, ok := cs.stepIndex[tt.gate]
			require.True(t, ok)
			require.Equal(t, stepError, cs.steps[idx].status)
		})
	}
}

// TestConnectionScreen_fatal_connect_failure_freezes_the_sequence
// pins that a failed connect (a store that cannot service the
// handshake) stops the sequence outright: the failed step stays
// visible and no further tick advances the cursor or transitions to
// the chat screen. A failed connect skips runAutojoin (see the
// connectionReadyMsg case in Update), so without this the autojoin
// gate would never be satisfied and the sequence would stick on the
// unrelated "Joining channels" step forever, nowhere near where the
// failure actually happened.
func TestConnectionScreen_fatal_connect_failure_freezes_the_sequence(t *testing.T) {
	errConnect := errors.New("open store: disk full")
	next := stubChatScreen{}

	s := NewConnectionScreen(ConnectionConfig{
		HasAPIKey:    true,
		ChannelCount: 1,
		Nick:         "alice",
		Session:      fakeConnector{err: errConnect},
	}, next)

	var m ui.Model = s
	m, cmd := m.Update(connectionReadyMsg{err: errConnect})
	cs := m.(ConnectionScreen)

	require.True(t, cs.fatal)
	require.Equal(t, stepError, cs.steps[0].status)
	require.Equal(t, 0, cs.cur, "the cursor stays on the failed step")
	require.Nil(t, cmd, "no chat screen forwarded and no autojoin armed")

	// Further ticks must not move the cursor, mark the sequence
	// done, or transition — the sequence is frozen on the fatal
	// failure.
	for range 5 {
		m, cmd = m.Update(ConnectionTickMsg{})
		require.Nil(t, cmd)
	}

	cs = m.(ConnectionScreen)
	require.Equal(t, 0, cs.cur)
	require.False(t, cs.done)
}

// TestConnectionScreen_non_fatal_failure_completes_the_sequence pins
// that a failed load-models or autojoin gate — neither is fatal —
// still lets the sequence finish and hand off to the chat screen,
// with the failure left visible on its own step.
func TestConnectionScreen_non_fatal_failure_completes_the_sequence(t *testing.T) {
	next := stubChatScreen{}
	errLoadModels := errors.New("catalogue unreachable")

	s := NewConnectionScreen(ConnectionConfig{
		HasAPIKey:    true,
		ChannelCount: 1,
		Nick:         "alice",
		Session:      fakeConnector{},
	}, next)

	var m ui.Model = s
	m, _ = m.Update(connectionReadyMsg{})
	m, _ = m.Update(loadModelsDoneMsg{err: errLoadModels})
	m, _ = m.Update(joinAutojoinDoneMsg{})

	cs := m.(ConnectionScreen)
	require.False(t, cs.fatal)

	var transition tea.Cmd

	for range len(cs.steps) + 1 {
		var cmd tea.Cmd
		m, cmd = m.Update(ConnectionTickMsg{})
		cs = m.(ConnectionScreen)

		if cs.done {
			transition = cmd
			break
		}
	}

	require.True(t, cs.done, "the sequence must complete despite the non-fatal failure")
	require.NotNil(t, transition)

	idx, ok := cs.stepIndex[gateLoadModels]
	require.True(t, ok)
	require.Equal(t, stepError, cs.steps[idx].status,
		"the failure stays visible on its own step")
}
