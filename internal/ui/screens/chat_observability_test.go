package screens_test

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/exp/teatest"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	uipkg "github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/screens"
	"github.com/laney/modeloff/internal/ui/uitest"
)

// TestChatScreen_obs_drawer_open_view_fits_terminal pins the contract
// between MainLayout's height reservation for the observability drawer
// and ChatWorkspace's actual rendered drawer size. The drawer is
// composed of two bordered panes stacked vertically; if either pane
// overshoots its allotted rows, the joined view exceeds the terminal
// and bubbletea's renderer scrolls the chat header off the top.
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

	tm := uitest.New(t, uipkg.NewRoot(chatScreen), teatest.WithInitialTermSize(width, height))

	tm.WaitFor("Created channel #general")
	tm.Submit("/topic anchor topic")
	tm.Submit("hello from #general")
	tm.WaitFor("hello from #general")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlL})

	view := tm.WaitForViewContains("Logs", "Metrics", "hello from #general", "testuser >")

	// The chat column's topic bar sits at the very top of the chat
	// area; the sidebar's "Channels" header sits at the very top
	// of the sidebar. Both anchor the top edge of the view. An
	// obs drawer that overshoots its reserved height pushes the
	// joined view past the terminal bottom, and the terminal
	// scrolls the top rows off — taking the topic bar and sidebar
	// header with them.
	lines := strings.Split(view, "\n")
	require.Contains(t, lines[0], "Channels",
		"sidebar 'Channels' header must remain at the top row; an over-tall obs drawer scrolls it off")
	require.Contains(t, lines[0], "anchor topic",
		"topic bar must remain at the top row; an over-tall obs drawer scrolls it off")
	require.Equal(t, height, lipgloss.Height(view),
		"the chat-screen view must fit the terminal exactly")
}
