package screens_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	uipkg "github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/screens"
	"github.com/laney/modeloff/internal/ui/uitest"
)

// TestChatScreen_obs_drawer_open_view_fits_terminal pins the contract
// between the workspace's height reservation for the observability
// drawer and the drawer's rendered size. The drawer contains two
// bordered panes stacked vertically; if either pane overshoots its
// allotted rows, Bubble Tea scrolls the chat header off the top.
func TestChatScreen_obs_drawer_open_view_fits_terminal(t *testing.T) {
	const (
		width  = 200
		height = 60
	)

	h := newTestSession(t)
	uitest.SeedChannel(t, h.user, "#general")

	obs, err := observability.NewRuntime()
	require.NoError(t, err)
	t.Cleanup(func() { _ = obs.Shutdown(t.Context()) })

	chatScreen, err := screens.NewChatScreen(t.Context, h.sess, h.mgr, h.user, newFakeConfigStore(), nil, domain.KindStatus)
	require.NoError(t, err)
	chatScreen = chatScreen.WithObservability(obs)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen), uitest.WithInitialTermSize(width, height))

	tm.WaitFor("Created channel #general")
	tm.Submit("/topic anchor topic")
	tm.Submit("hello from #general")
	tm.WaitFor("hello from #general")

	tm.Send(tea.KeyPressMsg{Code: 'l', Mod: tea.ModAlt})

	view := tm.WaitForViewContains("Logs", "Metrics", "hello from #general", "testuser >")

	lines := strings.Split(view, "\n")
	require.Contains(t, lines[0], "Channels",
		"sidebar 'Channels' header must remain at the top row; an over-tall drawer scrolls it off")
	require.Contains(t, lines[0], "anchor topic",
		"topic bar must remain at the top row; an over-tall drawer scrolls it off")
	require.Equal(t, height, lipgloss.Height(view),
		"the chat-screen view must fit the terminal exactly")
}
