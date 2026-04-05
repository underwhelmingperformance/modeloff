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
	sess, _ := newIntegrationSession(t, &integrationAPI{})
	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))

	tm.WaitFor("Welcome to modeloff")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})

	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))
}

func TestChatScreen_join_flow_with_teatest(t *testing.T) {
	sess, _ := newIntegrationSession(t, &integrationAPI{})
	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))

	tm.WaitFor("Welcome to modeloff")

	tm.Submit("/join #general")
	tm.WaitFor("Created channel #general")

	tm.Submit("/join #general")
	tm.WaitFor("testuser has joined #general")
}

func TestChatScreen_leave_flow_with_teatest(t *testing.T) {
	sess, _ := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedMessage(t, sess, "#general", "general msg")
	uitest.SeedChannel(t, sess, "#random")
	uitest.SeedMessage(t, sess, "#random", "random msg")

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	tm.WaitFor("random msg")

	tm.Submit("/part")
	tm.WaitFor("general msg")
}

func TestChatScreen_sidebar_navigation_with_teatest(t *testing.T) {
	sess, store := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedMessage(t, sess, "#general", "general msg")
	uitest.SeedChannel(t, sess, "#random")
	uitest.SeedMessage(t, sess, "#random", "random msg")
	require.NoError(t, store.SetLastChannel(t.Context(), "#general"))

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	tm.WaitFor("general msg")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlD})
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlO})
	tm.WaitFor("random msg")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlU})
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlO})
	tm.WaitFor("general msg")
}

func TestChatScreen_command_errors_with_teatest(t *testing.T) {
	sess, _ := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	tm.WaitFor("#general")

	tm.Submit("/nick")
	tm.WaitFor("missing required argument <new-nick>")

	tm.Submit("/unknown")
	tm.WaitFor("unknown command: /unknown")
}

func TestConnectionScreen_progression_with_teatest(t *testing.T) {
	sess, _ := newIntegrationSession(t, &integrationAPI{})
	root := uipkg.NewRoot(screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey:    true,
		ChannelCount: 0,
		Nick:         string(sess.UserNick()),
		Next:         screens.NewChatScreen(t.Context(), sess),
	}))
	tm := uitest.New(t, root)

	advanceConnection(tm, 4)
	tm.WaitFor("Welcome to modeloff")
}
