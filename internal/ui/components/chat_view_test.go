package components_test

import (
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
	"github.com/laney/modeloff/internal/ui/theme"
)

var testMessages = []domain.Message{
	{ID: "1", Channel: "#general", From: "alice", Body: "hello", SentAt: time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)},
	{ID: "2", Channel: "#general", From: "bob", Body: "hi there", SentAt: time.Date(2025, 1, 1, 10, 1, 0, 0, time.UTC)},
	{ID: "3", Channel: "#general", From: "alice", Body: "how are you?", SentAt: time.Date(2025, 1, 1, 10, 2, 0, 0, time.UTC)},
}

func TestChatView_View_shows_messages(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "", testMessages)
	v := cv.View(80, 24)

	require.Contains(t, v, "hello")
	require.Contains(t, v, "hi there")
	require.Contains(t, v, "how are you?")
	require.Contains(t, v, "alice")
	require.Contains(t, v, "bob")
}

func TestChatView_View_empty_messages(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "", nil)
	v := cv.View(80, 24)

	require.Contains(t, v, "No messages yet")
}

func TestChatView_View_has_input_prompt(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "", testMessages)
	v := cv.View(80, 24)

	require.Contains(t, v, ">")
}

func TestChatView_typing_goes_to_input(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "", nil)
	var m ui.Model = cv

	m = typeText(t, m, "test message")
	m, cmd := enter(t, m)

	require.NotNil(t, cmd)

	msg := cmd()
	sub, ok := msg.(components.MessageSubmitMsg)
	require.True(t, ok, "expected MessageSubmitMsg, got %T", msg)
	require.Equal(t, "test message", sub.Text)

	_ = m
}

func TestChatView_command_from_input(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "", nil)
	var m ui.Model = cv

	m = typeText(t, m, "/join #random")
	_, cmd := enter(t, m)

	require.NotNil(t, cmd)

	msg := cmd()
	sub, ok := msg.(components.CommandSubmitMsg)
	require.True(t, ok, "expected CommandSubmitMsg, got %T", msg)
	require.Equal(t, "join", sub.Name)
	require.Equal(t, "#random", sub.Args)
}

func TestChatView_messages_updated(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "", nil)
	var m ui.Model = cv

	newMsgs := []domain.Message{
		{ID: "10", Channel: "#general", From: "charlie", Body: "new message"},
	}

	m, _ = m.Update(components.MessagesUpdatedMsg{
		Channel:  "#general",
		Messages: newMsgs,
	})

	v := m.View(80, 24)
	require.Contains(t, v, "new message")
	require.Contains(t, v, "charlie")
}

func TestChatView_messages_updated_wrong_channel(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "", testMessages)
	var m ui.Model = cv

	m, _ = m.Update(components.MessagesUpdatedMsg{
		Channel:  "#other",
		Messages: nil,
	})

	// Should still show the original messages.
	v := m.View(80, 24)
	require.Contains(t, v, "hello")
}

func TestChatView_scroll(t *testing.T) {
	// Create many messages to fill the view.
	msgs := make([]domain.Message, 30)
	for i := range msgs {
		msgs[i] = domain.Message{
			ID:      fmt.Sprintf("%d", i),
			Channel: "#general",
			From:    "user",
			Body:    fmt.Sprintf("message %d", i),
		}
	}

	cv := components.NewChatView("#general", "testuser", "", msgs)
	var m ui.Model = cv

	// Scroll up.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})

	v := m.View(80, 24)
	// After scrolling up, the last message should no longer be visible
	// (it scrolled off the bottom).
	require.NotContains(t, v, "message 29")

	// Scroll back down.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})

	v = m.View(80, 24)
	require.Contains(t, v, "message 29")
}

func TestChatView_scroll_does_not_go_negative(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "", testMessages)
	var m ui.Model = cv

	// Try to scroll down past zero.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})

	v := m.View(80, 24)
	require.Contains(t, v, "how are you?")
}

func TestChatView_user_nick_styled_differently(t *testing.T) {
	msgs := []domain.Message{
		{ID: "1", Channel: "#general", From: "alice", Body: "from user"},
		{ID: "2", Channel: "#general", From: "bot", Body: "from model"},
	}

	cv := components.NewChatView("#general", "alice", "", msgs)
	v := cv.View(80, 24)

	// Both nicks should appear in the output.
	require.Contains(t, v, "alice")
	require.Contains(t, v, "bot")

	// The user nick "alice" and the model nick "bot" are styled with
	// different theme colours (green vs magenta). Render both with their
	// respective styles and check they appear in the view.
	userStyled := theme.UserNick.Render("<alice>")
	modelStyled := theme.ModelNick.Render("<bot>")

	require.Contains(t, v, userStyled)
	require.Contains(t, v, modelStyled)
}

func TestChatView_shows_nick_in_input_area(t *testing.T) {
	cv := components.NewChatView("#general", "alice", "", testMessages)
	v := cv.View(80, 24)

	require.Contains(t, v, "alice")
	require.Contains(t, v, ">")
}

func TestChatView_nick_updates_after_change(t *testing.T) {
	cv1 := components.NewChatView("#general", "oldnick", "", testMessages)
	cv2 := components.NewChatView("#general", "newnick", "", testMessages)

	v1 := cv1.View(80, 24)
	v2 := cv2.View(80, 24)

	require.Contains(t, v1, "oldnick")
	require.NotContains(t, v1, "newnick")
	require.Contains(t, v2, "newnick")
	require.NotContains(t, v2, "oldnick")
}

func TestChatView_topic_bar_shown(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "Welcome to general", testMessages)
	v := cv.View(80, 24)

	require.Contains(t, v, "Welcome to general")
}

func TestChatView_no_topic_bar_when_empty(t *testing.T) {
	withTitle := components.NewChatView("#general", "testuser", "some topic", testMessages)
	without := components.NewChatView("#general", "testuser", "", testMessages)

	vWith := withTitle.View(80, 24)
	vWithout := without.View(80, 24)

	require.Contains(t, vWith, "some topic")
	require.NotContains(t, vWithout, "some topic")
}

func TestChatView_topic_bar_reduces_message_area(t *testing.T) {
	msgs := make([]domain.Message, 30)
	for i := range msgs {
		msgs[i] = domain.Message{
			ID:      fmt.Sprintf("%d", i),
			Channel: "#general",
			From:    "user",
			Body:    fmt.Sprintf("msg %d", i),
		}
	}

	withTitle := components.NewChatView("#general", "testuser", "A topic", msgs)
	without := components.NewChatView("#general", "testuser", "", msgs)

	vWith := withTitle.View(80, 24)
	vWithout := without.View(80, 24)

	withLines := lipgloss.Height(vWith)
	withoutLines := lipgloss.Height(vWithout)

	// Both should fill the same total height.
	require.Equal(t, withoutLines, withLines)
}

func TestChatView_pending_indicator(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "", testMessages)
	var m ui.Model = cv

	// Initially no pending indicator.
	v := m.View(80, 24)
	require.NotContains(t, v, "responding")

	// Set pending.
	m, _ = m.Update(components.PendingResponseMsg{Pending: true})

	v = m.View(80, 24)
	require.Contains(t, v, "responding")

	// Clear pending.
	m, _ = m.Update(components.PendingResponseMsg{Pending: false})

	v = m.View(80, 24)
	require.NotContains(t, v, "responding")
}

func TestChatView_pending_indicator_reduces_message_area(t *testing.T) {
	msgs := make([]domain.Message, 30)
	for i := range msgs {
		msgs[i] = domain.Message{
			ID:      fmt.Sprintf("%d", i),
			Channel: "#general",
			From:    "user",
			Body:    fmt.Sprintf("msg %d", i),
		}
	}

	cv := components.NewChatView("#general", "testuser", "", msgs)
	var m ui.Model = cv

	m, _ = m.Update(components.PendingResponseMsg{Pending: true})

	v := m.View(80, 24)
	withLines := lipgloss.Height(v)

	// Total height should stay the same.
	require.Equal(t, 24, withLines)
	require.Contains(t, v, "responding")
}

func TestRenderSystemEvent(t *testing.T) {
	got := components.RenderSystemEvent("alice has joined")

	require.Contains(t, got, "***")
	require.Contains(t, got, "alice has joined")
}
