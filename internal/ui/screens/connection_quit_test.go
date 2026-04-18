package screens

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	uipkg "github.com/laney/modeloff/internal/ui"
)

func TestConnectionScreen_StatusItems_omits_disconnecting_by_default(t *testing.T) {
	s := NewConnectionScreen(ConnectionConfig{
		HasAPIKey: true,
		Nick:      "alice",
	})

	require.Empty(t, s.StatusItems(),
		"disconnecting status item should only appear once a quit is in flight")
}

func TestConnectionScreen_StatusItems_surfaces_disconnecting_while_quit_in_flight(t *testing.T) {
	s := NewConnectionScreen(ConnectionConfig{
		HasAPIKey: true,
		Nick:      "alice",
	})

	// Animation-only mode (no Session): the first QuitRequestedMsg
	// short-circuits to tea.Quit without flipping `quitting`. Drive
	// the state directly so we can verify the StatusItems contract.
	s.quitting = true

	require.Equal(t, []uipkg.StatusItem{disconnectingStatusItem},
		s.StatusItems(),
		"quit-in-flight must surface a Disconnecting… status item")
}

func TestConnectionScreen_View_renders_disconnecting_in_status_bar(t *testing.T) {
	s := NewConnectionScreen(ConnectionConfig{
		HasAPIKey: true,
		Nick:      "alice",
	})
	s.quitting = true

	view := s.View(80, 24)

	require.Contains(t, view, "Disconnecting…",
		"connection screen must render the status bar with the disconnecting indicator while quitting")
}

func TestConnectionScreen_View_omits_status_bar_when_idle(t *testing.T) {
	s := NewConnectionScreen(ConnectionConfig{
		HasAPIKey: true,
		Nick:      "alice",
	})

	view := s.View(80, 24)

	require.NotContains(t, view, "Disconnecting…",
		"the disconnecting indicator must not appear before a quit is requested")
}

func TestConnectionScreen_View_uses_full_height_when_idle(t *testing.T) {
	s := NewConnectionScreen(ConnectionConfig{
		HasAPIKey: true,
		Nick:      "alice",
	})

	idle := s.View(80, 24)
	s.quitting = true
	busy := s.View(80, 24)

	// Both renders span the full terminal height; idle absorbs the
	// would-be status-bar row into the animation's vertical padding,
	// busy surfaces it as the trailing visible row.
	require.Equal(t, 24, lipgloss.Height(idle))
	require.Equal(t, 24, lipgloss.Height(busy))

	require.Equal(t, []string{"… Connecting to modeloff"}, trimmedLines(idle),
		"idle view: only the centred animation is visible; no trailing bar row")
	require.Equal(t, []string{"… Connecting to modeloff", "Disconnecting…"}, trimmedLines(busy),
		"quitting view: animation plus the disconnecting bar row")
}

// trimmedLines returns the non-empty rows of a rendered view with
// ANSI escapes stripped and surrounding whitespace trimmed.
func trimmedLines(view string) []string {
	stripped := ansi.Strip(view)
	out := make([]string, 0)

	for line := range strings.SplitSeq(stripped, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		out = append(out, trimmed)
	}

	return out
}

func TestConnectionScreen_refreshPane_is_idempotent_across_ticks(t *testing.T) {
	sess := newTestSession(t)
	require.NoError(t, sess.Connect(t.Context()))
	// JoinAutojoinChannels appends a status notice for each autojoin
	// entry; pre-seed two entries via a pair of normal Join calls
	// (which both persist membership and register the channel in
	// the autojoin list) so the autojoin replay emits something the
	// pane can accumulate.
	require.NoError(t, sess.Join(t.Context(), "#general"))
	require.NoError(t, sess.Join(t.Context(), "#random"))
	require.NoError(t, sess.JoinAutojoinChannels(t.Context()))

	s := NewConnectionScreen(ConnectionConfig{
		HasAPIKey:    true,
		ChannelCount: 2,
		Nick:         string(sess.UserNick()),
		Session:      sess,
		Ctx:          t.Context(),
	})

	// First refresh drains every accumulated status notice; the
	// subsequent refreshes must be pure no-ops, leaving the pane's
	// visible content identical.
	s.refreshPane()
	afterFirst := trimmedLines(s.pane.View(80, 16))

	s.refreshPane()
	s.refreshPane()
	s.refreshPane()

	require.Equal(t, []string{
		"✓ Connected to modeloff",
		"✓ Joining #general",
		"✓ Joining #random",
	}, afterFirst,
		"pane must hold one entry per status notice in emission order")
	require.Equal(t, afterFirst, trimmedLines(s.pane.View(80, 16)),
		"repeated refreshPane calls must not append the same events again")
}

func TestConnectionScreen_second_quit_request_escalates_to_tea_quit(t *testing.T) {
	s := NewConnectionScreen(ConnectionConfig{
		HasAPIKey: true,
		Nick:      "alice",
	})
	// Pretend the first quit has already started (cfg.Session is nil
	// in this test, so handleQuitRequested would otherwise return
	// tea.Quit immediately on the first call). Setting quitting
	// directly mirrors the post-first-Ctrl-C state.
	s.quitting = true

	updated, cmd := s.Update(uipkg.QuitRequestedMsg{})
	require.NotNil(t, cmd)

	second, ok := updated.(ConnectionScreen)
	require.True(t, ok)
	require.True(t, second.quitting,
		"quitting flag should remain set after the escalation")

	require.Equal(t, tea.Quit(), cmd(),
		"second QuitRequestedMsg should escalate to tea.Quit")
}
