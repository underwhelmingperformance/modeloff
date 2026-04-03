package ui_test

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
	"github.com/stretchr/testify/require"

	uipkg "github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/screens"
)

func TestRoot_quits_on_ctrl_c_with_teatest(t *testing.T) {
	tm := newTestApp(t, uipkg.NewRoot(screens.NewPlaceholderScreen("alice")))

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})

	model := tm.FinalModel(t, teatest.WithFinalTimeout(2*time.Second))
	_, ok := model.(uipkg.Root)
	require.True(t, ok, "expected Root, got %T", model)
}

func TestChatScreen_join_flow_with_teatest(t *testing.T) {
	sess, _ := newIntegrationSession(t, &integrationAPI{})
	tm := newTestApp(t, uipkg.NewRoot(screens.NewChatScreen(sess)))

	waitForOutput(t, tm, "Welcome to modeloff")

	submitText(tm, "/join #general")
	waitForOutput(t, tm, "Created channel #general")

	submitText(tm, "/join #general")
	waitForOutput(t, tm, "Switched to #general")

	view := finalView(t, tm)
	require.Contains(t, view, "#general")
}

func TestChatScreen_leave_flow_with_teatest(t *testing.T) {
	sess, _ := newIntegrationSession(t, &integrationAPI{})
	seedChannel(t, sess, "#general")
	seedMessage(t, sess, "#general", "general msg")
	seedChannel(t, sess, "#random")
	seedMessage(t, sess, "#random", "random msg")

	tm := newTestApp(t, uipkg.NewRoot(screens.NewChatScreen(sess)))
	waitForOutput(t, tm, "random msg")

	submitText(tm, "/leave")
	waitForOutput(t, tm, "general msg")

	view := finalView(t, tm)
	require.Contains(t, view, "general msg")
}

func TestChatScreen_sidebar_navigation_with_teatest(t *testing.T) {
	sess, store := newIntegrationSession(t, &integrationAPI{})
	seedChannel(t, sess, "#general")
	seedMessage(t, sess, "#general", "general msg")
	seedChannel(t, sess, "#random")
	seedMessage(t, sess, "#random", "random msg")
	require.NoError(t, store.SetLastChannel(t.Context(), "#general"))

	tm := newTestApp(t, uipkg.NewRoot(screens.NewChatScreen(sess)))
	waitForOutput(t, tm, "general msg")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlD})
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlO})
	waitForOutput(t, tm, "random msg")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlU})
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlO})
	waitForOutput(t, tm, "general msg")

	view := finalView(t, tm)
	require.Contains(t, view, "general msg")
}

func TestChatScreen_command_errors_with_teatest(t *testing.T) {
	sess, _ := newIntegrationSession(t, &integrationAPI{})
	seedChannel(t, sess, "#general")

	tm := newTestApp(t, uipkg.NewRoot(screens.NewChatScreen(sess)))
	waitForOutput(t, tm, "#general")

	submitText(tm, "/nick")
	waitForOutput(t, tm, "/nick requires a new nickname")

	submitText(tm, "/unknown")
	waitForOutput(t, tm, "unknown command: /unknown")

	view := finalView(t, tm)
	require.Contains(t, view, "unknown command: /unknown")
}

func TestConnectionScreen_progression_with_teatest(t *testing.T) {
	root := uipkg.NewRoot(screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey:    true,
		ChannelCount: 1,
		Nick:         "alice",
		Next:         screens.NewPlaceholderScreen("alice"),
	}))
	tm := newTestApp(t, root)

	advanceConnection(tm, 4)
	waitForOutput(t, tm, "connected as", "Use /join to enter a channel")

	view := finalView(t, tm)
	require.Contains(t, view, "connected as")
	require.Contains(t, view, "Use /join to enter a channel")
}
