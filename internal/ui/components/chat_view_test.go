package components_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
	"github.com/laney/modeloff/internal/ui/theme"
)

var testMessages = components.MessagesToLines([]domain.Message{
	{ID: "1", Channel: "#general", From: "alice", Body: "hello", SentAt: time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)},
	{ID: "2", Channel: "#general", From: "bob", Body: "hi there", SentAt: time.Date(2025, 1, 1, 10, 1, 0, 0, time.UTC)},
	{ID: "3", Channel: "#general", From: "alice", Body: "how are you?", SentAt: time.Date(2025, 1, 1, 10, 2, 0, 0, time.UTC)},
})

func TestChatView_View_shows_messages(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "", testMessages)
	v := cv.View(80, 24)

	require.Contains(t, v, "hello")
	require.Contains(t, v, "hi there")
	require.Contains(t, v, "how are you?")
	require.Contains(t, v, "alice")
	require.Contains(t, v, "bob")
}

func TestChatView_View_shows_timestamps(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "", testMessages)
	v := cv.View(80, 24)

	require.Contains(t, v, "[10:00:00]")
	require.Contains(t, v, "[10:01:00]")
	require.Contains(t, v, "[10:02:00]")
}

func TestChatView_View_wraps_long_messages(t *testing.T) {
	longBody := strings.Repeat("word ", 30)
	lines := components.MessagesToLines([]domain.Message{
		{ID: "1", Channel: "#general", From: "alice", Body: longBody, SentAt: time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)},
	})

	cv := components.NewChatView("#general", "testuser", "", lines)
	v := cv.View(40, 24)

	// The message should wrap, producing more rendered lines than one.
	require.Greater(t, lipgloss.Height(v), 3,
		"long message should wrap to multiple lines at narrow width")

	// All the content should still be present.
	require.Contains(t, v, "word")
	require.Contains(t, v, "alice")
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
		Channel: "#general",
		Lines:   components.MessagesToLines(newMsgs),
	})

	v := m.View(80, 24)
	require.Contains(t, v, "new message")
	require.Contains(t, v, "charlie")
}

func TestChatView_messages_updated_wrong_channel(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "", testMessages)
	var m ui.Model = cv

	m, _ = m.Update(components.MessagesUpdatedMsg{
		Channel: "#other",
		Lines:   nil,
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

	cv := components.NewChatView("#general", "testuser", "", components.MessagesToLines(msgs))
	var m ui.Model = cv

	// Render once so the viewport learns its dimensions.
	m.View(80, 24)

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

func TestChatView_scroll_indicator(t *testing.T) {
	msgs := make([]domain.Message, 30)
	for i := range msgs {
		msgs[i] = domain.Message{
			ID:      fmt.Sprintf("%d", i),
			Channel: "#general",
			From:    "user",
			Body:    fmt.Sprintf("message %d", i),
		}
	}

	cv := components.NewChatView("#general", "testuser", "", components.MessagesToLines(msgs))
	var m ui.Model = cv

	// At the bottom — no indicator.
	v := m.View(80, 24)
	require.NotContains(t, v, "%)")

	// Scroll up — indicator appears.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})

	v = m.View(80, 24)
	require.Contains(t, v, "%)")

	// Total height stays the same.
	require.Equal(t, 24, lipgloss.Height(v))

	// Scroll back to bottom — indicator disappears.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})

	v = m.View(80, 24)
	require.NotContains(t, v, "%)")
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

func TestChatView_nicks_use_hashed_colours(t *testing.T) {
	msgs := []domain.Message{
		{ID: "1", Channel: "#general", From: "alice", Body: "from user"},
		{ID: "2", Channel: "#general", From: "bot", Body: "from model"},
	}

	cv := components.NewChatView("#general", "alice", "", components.MessagesToLines(msgs))
	v := cv.View(80, 24)

	// Each nick is rendered with a colour derived from its name.
	aliceStyled := theme.NickStyle("alice").Render("<alice>")
	botStyled := theme.NickStyle("bot").Render("<bot>")

	require.Contains(t, v, aliceStyled)
	require.Contains(t, v, botStyled)
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

	lines := components.MessagesToLines(msgs)

	withTitle := components.NewChatView("#general", "testuser", "A topic", lines)
	without := components.NewChatView("#general", "testuser", "", lines)

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

	cv := components.NewChatView("#general", "testuser", "", components.MessagesToLines(msgs))
	var m ui.Model = cv

	m, _ = m.Update(components.PendingResponseMsg{Pending: true})

	v := m.View(80, 24)
	withLines := lipgloss.Height(v)

	// Total height should stay the same.
	require.Equal(t, 24, withLines)
	require.Contains(t, v, "responding")
}

func renderSingleLine(line components.ChatLine) string {
	cv := components.NewChatView("#test", "testuser", "", []components.ChatLine{line})
	v := cv.View(200, 24)

	return ansi.Strip(v)
}

func TestRenderLine_IRC_events(t *testing.T) {
	tests := []struct {
		name string
		line components.ChatLine
		want string
	}{
		{
			"join",
			components.Join{JoinEvent: domain.JoinEvent{Channel: "#general", Nick: "alice"}},
			"*** alice has joined #general",
		},
		{
			"join_created",
			components.Join{JoinEvent: domain.JoinEvent{Channel: "#general", Nick: "alice", Created: true}},
			"*** Created channel #general",
		},
		{
			"part",
			components.Part{PartEvent: domain.PartEvent{Channel: "#general", Nick: "alice"}},
			"*** alice has left #general",
		},
		{
			"nick_change",
			components.NickChange{NickChangeEvent: domain.NickChangeEvent{OldNick: "alice", NewNick: "bob"}},
			"*** alice is now known as bob",
		},
		{
			"topic_set",
			components.TopicChange{TopicChangeEvent: domain.TopicChangeEvent{Channel: "#general", Title: "cool topic"}},
			"*** topic for #general set to: cool topic",
		},
		{
			"topic_cleared",
			components.TopicChange{TopicChangeEvent: domain.TopicChangeEvent{Channel: "#general", Title: ""}},
			"*** topic for #general cleared",
		},
		{
			"model_invited",
			components.ModelInvited{ModelInvitedEvent: domain.ModelInvitedEvent{
				Channel:  "#general",
				Instance: domain.ModelInstance{Nick: "botty", ModelID: "anthropic/haiku"},
			}},
			"*** botty (anthropic/haiku) has joined #general",
		},
		{
			"model_invited_with_persona",
			components.ModelInvited{ModelInvitedEvent: domain.ModelInvitedEvent{
				Channel:  "#general",
				Instance: domain.ModelInstance{Nick: "botty", ModelID: "anthropic/haiku", Persona: "helpful"},
			}},
			`*** botty (anthropic/haiku) has joined #general with persona "helpful"`,
		},
		{
			"model_kicked",
			components.ModelKicked{ModelKickedEvent: domain.ModelKickedEvent{Channel: "#general", Nick: "botty"}},
			"*** botty has been kicked from #general",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderSingleLine(tt.line)

			require.Contains(t, got, tt.want)
		})
	}
}

func TestRenderLine_application_feedback(t *testing.T) {
	tests := []struct {
		name string
		line components.ChatLine
		want string
	}{
		{
			"help",
			components.Help{},
			"*** /join <channel>",
		},
		{
			"channel_list",
			components.ChannelList{Channels: []domain.Channel{
				{Name: "#general"},
				{Name: "#random", Title: "cool"},
			}},
			"*** #general",
		},
		{
			"channel_list_empty",
			components.ChannelList{},
			"*** no channels",
		},
		{
			"api_key_saved",
			components.APIKeySaved{},
			"✓ OpenRouter API key saved",
		},
		{
			"poke_interval_set",
			components.PokeIntervalSet{Interval: 10 * time.Minute},
			"✓ Poke interval set to 10m0s.",
		},
		{
			"dm_opened",
			components.DMOpened{Nick: "botty"},
			"✓ Opened direct message with botty",
		},
		{
			"usage_hint_config",
			components.UsageHint{Command: "config"},
			"⚠ usage: /config api-key",
		},
		{
			"usage_hint_invite",
			components.UsageHint{Command: "invite"},
			"⚠ usage: /invite",
		},
		{
			"no_channel",
			components.NoChannel{},
			"⚠ join a channel first",
		},
		{
			"command_error",
			components.CommandError{Err: domain.UnknownCommandError{Name: "foo"}},
			"✗ unknown command: /foo",
		},
		{
			"unknown_nick_error",
			components.CommandError{Err: domain.UnknownNickError{Nick: "ghost"}},
			"✗ no such nick: ghost",
		},
		{
			"new_messages_divider",
			components.NewMessagesDivider{},
			"new messages",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderSingleLine(tt.line)

			require.Contains(t, got, tt.want)
		})
	}
}

func TestNewMessagesDivider_fills_width(t *testing.T) {
	cv := components.NewChatView("#test", "testuser", "", []components.ChatLine{
		components.NewMessagesDivider{},
	})

	v := cv.View(80, 24)
	stripped := ansi.Strip(v)

	require.Contains(t, stripped, "new messages")
	require.Contains(t, stripped, "──")

	// The divider dashes should span most of the width.
	for _, line := range strings.Split(stripped, "\n") {
		if strings.Contains(line, "new messages") {
			require.Contains(t, line, "─")
			require.GreaterOrEqual(t, len([]rune(line)), 40,
				"divider should span a significant portion of the width")
			break
		}
	}
}

func TestChatView_divider_inserted_when_scrolled_up(t *testing.T) {
	// Create enough messages to fill the viewport.
	msgs := make([]domain.Message, 30)
	for i := range msgs {
		msgs[i] = domain.Message{
			ID:      fmt.Sprintf("%d", i),
			Channel: "#general",
			From:    "user",
			Body:    fmt.Sprintf("message %d", i),
		}
	}

	cv := components.NewChatView("#general", "testuser", "",
		components.MessagesToLines(msgs))
	var m ui.Model = cv

	// Render to initialise viewport dimensions.
	m.View(80, 24)

	// Scroll up so we're no longer at the bottom.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})

	// Verify we're scrolled up.
	v := m.View(80, 24)
	require.NotContains(t, v, "message 29")

	// Add new messages via SetLines.
	newMsgs := make([]domain.Message, 33)
	copy(newMsgs, msgs)

	for i := 30; i < 33; i++ {
		newMsgs[i] = domain.Message{
			ID:      fmt.Sprintf("%d", i),
			Channel: "#general",
			From:    "other",
			Body:    fmt.Sprintf("new message %d", i),
		}
	}

	cv.SetLines(components.MessagesToLines(newMsgs))

	// Scroll to bottom to see the divider.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})

	v = m.View(80, 24)
	stripped := ansi.Strip(v)

	require.Contains(t, stripped, "new messages",
		"divider should be visible after scrolling down")
}

func TestChatView_no_divider_when_at_bottom(t *testing.T) {
	msgs := make([]domain.Message, 5)
	for i := range msgs {
		msgs[i] = domain.Message{
			ID:      fmt.Sprintf("%d", i),
			Channel: "#general",
			From:    "user",
			Body:    fmt.Sprintf("message %d", i),
		}
	}

	cv := components.NewChatView("#general", "testuser", "",
		components.MessagesToLines(msgs))

	// Render — viewport is at bottom.
	cv.View(80, 24)

	// Add more messages while at bottom.
	newMsgs := make([]domain.Message, 8)
	copy(newMsgs, msgs)

	for i := 5; i < 8; i++ {
		newMsgs[i] = domain.Message{
			ID:      fmt.Sprintf("%d", i),
			Channel: "#general",
			From:    "other",
			Body:    fmt.Sprintf("new message %d", i),
		}
	}

	cv.SetLines(components.MessagesToLines(newMsgs))

	v := cv.View(80, 24)
	stripped := ansi.Strip(v)

	require.NotContains(t, stripped, "new messages",
		"no divider should appear when viewport is at bottom")
}
