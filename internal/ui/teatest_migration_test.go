package ui_test

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
	"github.com/stretchr/testify/require"

	uipkg "github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/screens"
	"github.com/laney/modeloff/internal/ui/uitest"
)

func TestRoot_quits_on_ctrl_c_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})
	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))

	tm.WaitFor("Welcome to modeloff")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})

	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))
}

func TestChatScreen_join_flow_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})
	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))

	tm.WaitFor("Welcome to modeloff")

	tm.Submit("/join #general")
	tm.WaitFor("Created channel #general")

	// Second join is idempotent: no join event is emitted, but the
	// channel stays active. Send a message to confirm we're still in it.
	tm.Submit("/join #general")
	tm.Submit("still here")
	tm.WaitFor("still here")
}

func TestChatScreen_leave_flow_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedMessage(t, sess, "#general", "general msg")
	uitest.SeedChannel(t, sess, "#random")
	uitest.SeedMessage(t, sess, "#random", "random msg")

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	tm.WaitFor("random msg")

	tm.Submit("/part")
	tm.WaitFor("general msg")
}

func TestChatScreen_sidebar_navigation_with_teatest(t *testing.T) {
	sess, store, cfgStore := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedMessage(t, sess, "#general", "general msg")
	uitest.SeedChannel(t, sess, "#random")
	uitest.SeedMessage(t, sess, "#random", "random msg")
	require.NoError(t, store.SetLastChannel(t.Context(), "#general"))

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	tm.WaitFor("general msg")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlD})
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlO})
	tm.WaitFor("random msg")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlU})
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlO})
	tm.WaitFor("general msg")
}

func TestChatScreen_command_errors_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	tm.WaitFor("#general")

	tm.Submit("/nick")
	tm.WaitFor("missing required argument <new-nick>")

	tm.Submit("/unknown")
	tm.WaitFor("unknown command: /unknown")
}

func TestConnectionScreen_progression_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore)
	require.NoError(t, err)

	root := uipkg.NewRoot(screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey:    true,
		ChannelCount: 0,
		Nick:         string(sess.UserNick()),
		Next:         chatScreen,
	}))
	tm := uitest.New(t, root)

	advanceConnection(tm, 4)
	tm.WaitFor("Welcome to modeloff")
}
