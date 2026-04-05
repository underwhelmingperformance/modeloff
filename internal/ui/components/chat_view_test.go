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

	"github.com/laney/modeloff/internal/command"
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
	require.Equal(t, "/join #random", sub.Raw)
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

func TestChatView_append_line(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "", testMessages)
	var m ui.Model = cv

	m, _ = m.Update(components.MessageLine{
		Message: domain.Message{ID: "10", Channel: "#general", From: "dave", Body: "appended message"},
	})

	v := m.View(80, 24)
	require.Contains(t, v, "appended message")
	require.Contains(t, v, "dave")
	// Original messages should still be there.
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

func TestChatView_ctrl_arrow_scroll(t *testing.T) {
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

	m.View(80, 24)

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlUp})

	v := m.View(80, 24)
	require.NotContains(t, v, "message 29")

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlDown})

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

func TestChatView_arrow_keys_stay_with_input(t *testing.T) {
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

	m.View(80, 24)

	m = typeText(t, m, "first")
	m, _ = enter(t, m)
	m = typeText(t, m, "second")
	m, _ = enter(t, m)
	m = typeText(t, m, "draft")

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})

	v := m.View(80, 24)
	require.Contains(t, v, "second")
	require.Contains(t, v, "message 29")

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = typeText(t, m, "X")

	v = m.View(80, 24)
	require.Contains(t, v, "draXft")
	require.Contains(t, v, "message 29")
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

func TestChatView_TopicUpdatedMsg_updates_topic_bar(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "", testMessages)

	// No topic initially.
	v := cv.View(80, 24)
	stripped := ansi.Strip(v)
	require.NotContains(t, stripped, "new topic")

	// Send TopicUpdatedMsg.
	cv.Update(components.TopicUpdatedMsg{Topic: "new topic"})

	v = cv.View(80, 24)
	stripped = ansi.Strip(v)
	require.Contains(t, stripped, "new topic")

	// Clear topic.
	cv.Update(components.TopicUpdatedMsg{Topic: ""})

	v = cv.View(80, 24)
	stripped = ansi.Strip(v)
	require.NotContains(t, stripped, "new topic")
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

func renderSingleLine(line tea.Msg) string {
	return renderSingleLineWithHighlight(line, nil, "testuser")
}

func renderSingleLineWithHighlight(line tea.Msg, words []string, nick domain.Nick) string {
	cv := components.NewChatView("#test", nick, "", []tea.Msg{line})
	cv.Update(components.CommandStateMsg{
		Commands: command.Set{
			Commands: []*command.Node{
				{Name: "join", Help: "Join or create a channel", Positionals: []command.Positional{{Name: "channel"}}},
				{Name: "help", Help: "Show available commands."},
			},
		},
	})

	if len(words) > 0 {
		cv.Update(components.HighlightWordsMsg{Words: words, UserNick: nick})
	}

	v := cv.View(200, 24)

	return ansi.Strip(v)
}

func TestRenderLine_IRC_events(t *testing.T) {
	tests := []struct {
		name string
		line tea.Msg
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
			"topic_set_with_author",
			components.TopicChange{TopicChangeEvent: domain.TopicChangeEvent{Channel: "#general", Topic: "cool topic", By: "alice"}},
			"*** topic for #general set by alice: cool topic",
		},
		{
			"topic_set_no_author",
			components.TopicChange{TopicChangeEvent: domain.TopicChangeEvent{Channel: "#general", Topic: "cool topic"}},
			"*** topic for #general set to: cool topic",
		},
		{
			"topic_cleared",
			components.TopicChange{TopicChangeEvent: domain.TopicChangeEvent{Channel: "#general", Topic: "", By: "alice"}},
			"*** topic for #general cleared by alice",
		},
		{
			"topic_info_with_metadata",
			components.TopicInfo{Channel: domain.Channel{
				Name: "#general", Topic: "cool topic",
				TopicSetBy: "alice", TopicSetAt: time.Date(2026, 4, 4, 23, 30, 0, 0, time.UTC),
			}},
			"*** topic for #general: cool topic (set by alice on 2026-04-04 23:30)",
		},
		{
			"topic_info_no_topic",
			components.TopicInfo{Channel: domain.Channel{Name: "#general"}},
			"*** No topic set for #general",
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
		{
			"action_message",
			components.MessageLine{Message: domain.Message{From: "alice", Body: "waves", Action: true, SentAt: time.Now()}},
			"* alice waves",
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
		line tea.Msg
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
				{Name: "#random", Topic: "cool"},
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
			components.UsageHint{
				Command: "config",
				Usage:   "/config api-key <value> | /config nick-model <model-id> | /config poke-interval <duration>",
			},
			"⚠ usage: /config api-key",
		},
		{
			"usage_hint_invite",
			components.UsageHint{
				Command: "invite",
				Usage:   "/invite <model-id> [--persona <text>]",
			},
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
			"config_changed",
			components.ConfigChanged{Operation: "API key saved"},
			"✓ API key saved",
		},
		{
			"backend_error",
			components.BackendError{
				Operation: "model invocation",
				Err:       fmt.Errorf("connection refused"),
			},
			"✗ model invocation: connection refused",
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
	cv := components.NewChatView("#test", "testuser", "", []tea.Msg{
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

func TestChatView_command_popover_renders_and_completes(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "", nil)
	cv.Update(components.CommandStateMsg{
		Commands: command.Set{
			Commands: []*command.Node{
				{
					Name: "join",
					Help: "Join a channel",
					Positionals: []command.Positional{
						{Name: "channel", Source: command.LiteralSource(
							command.Suggestion{Value: "#general", Label: "#general"},
							command.Suggestion{Value: "#random", Label: "#random"},
						)},
					},
				},
			},
		},
	})
	var m ui.Model = cv

	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{X: 20, Y: 0, Width: 60, Height: 24}})
	m = typeText(t, m, "/jo")

	v := m.View(60, 24)
	require.Contains(t, v, "/join")
	require.Contains(t, v, "Join a channel")

	var cmd tea.Cmd
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyTab})

	require.NotNil(t, cmd, "Tab should produce a cmd")
	m, _ = m.Update(cmd())

	m = typeText(t, m, "#random")
	_, cmd = enter(t, m)

	require.NotNil(t, cmd)
	sub := cmd().(components.CommandSubmitMsg)
	require.Equal(t, "/join #random", sub.Raw)
}

func TestChatView_popover_arrow_keys_do_not_fall_through(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "", nil)
	cv.Update(components.CommandStateMsg{
		Commands: command.Set{
			Commands: []*command.Node{
				{Name: "join", Help: "Join a channel"},
				{Name: "part", Help: "Part from the current channel"},
				{Name: "quit", Help: "Exit modeloff"},
			},
		},
	})
	var m ui.Model = cv

	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{X: 0, Y: 0, Width: 60, Height: 24}})

	// Seed input history so Up would recall it if it fell through.
	m = typeText(t, m, "previous input")
	m, _ = enter(t, m)

	m = typeText(t, m, "/")

	// The popover is now visible with suggestions. Down should
	// navigate the popover, not recall input history.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})

	// Type Tab to accept whatever is selected, then complete and submit.
	var cmd tea.Cmd
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyTab})

	require.NotNil(t, cmd, "Tab should produce a cmd")
	m, _ = m.Update(cmd())

	_, cmd = enter(t, m)

	require.NotNil(t, cmd)
	sub := cmd().(components.CommandSubmitMsg)

	// If Down fell through to input history, the input would contain
	// "previous input" instead of a command. The second suggestion
	// (/part) should be selected after one Down press.
	require.Equal(t, "/part", sub.Raw)
}

func TestChatView_popover_renders_usage_in_suggestions(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "", nil)
	cv.Update(components.CommandStateMsg{
		Commands: command.Set{
			Commands: []*command.Node{
				{Name: "join", Help: "Join a channel", Positionals: []command.Positional{{Name: "channel"}}},
				{Name: "part", Help: "Part from the current channel"},
				{Name: "quit", Help: "Exit modeloff"},
			},
		},
	})
	var m ui.Model = cv

	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{X: 0, Y: 0, Width: 60, Height: 24}})
	m = typeText(t, m, "/")

	v := m.View(60, 24)
	stripped := ansi.Strip(v)

	require.Contains(t, stripped, "/join <channel>")
	require.Contains(t, stripped, "Join a channel")
	require.Contains(t, stripped, "/part")
	require.Contains(t, stripped, "/quit")
}

func TestChatView_mouse_click_positions_input_cursor(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "", nil)
	var m ui.Model = cv

	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{X: 20, Y: 0, Width: 60, Height: 24}})
	m = typeText(t, m, "hello")
	m, _ = m.Update(tea.MouseMsg{
		X:      32,
		Y:      23,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	m = typeText(t, m, "X")

	msg := m.(*components.ChatView)
	_, cmd := msg.Update(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd)

	sub := cmd().(components.MessageSubmitMsg)
	require.Equal(t, "hXello", sub.Text)
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

	cv.Update(components.SetLinesMsg{Lines: components.MessagesToLines(newMsgs)})

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

	cv.Update(components.SetLinesMsg{Lines: components.MessagesToLines(newMsgs)})

	v := cv.View(80, 24)
	stripped := ansi.Strip(v)

	require.NotContains(t, stripped, "new messages",
		"no divider should appear when viewport is at bottom")
}

func TestChatView_mouse_wheel_scrolls_messages(t *testing.T) {
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
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{X: 20, Y: 0, Width: 60, Height: 24}})

	m, _ = m.Update(tea.MouseMsg{
		X:      25,
		Y:      10,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonWheelUp,
	})

	v := m.View(60, 24)
	require.NotContains(t, v, "message 29")
}

func TestChatView_mouse_click_accepts_popover_suggestion(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "", nil)
	cv.Update(components.CommandStateMsg{
		Commands: command.Set{
			Commands: []*command.Node{
				{
					Name: "join",
					Help: "Join a channel",
					Positionals: []command.Positional{
						{Name: "channel", Source: command.LiteralSource(
							command.Suggestion{Value: "#general", Label: "#general"},
							command.Suggestion{Value: "#random", Label: "#random"},
						)},
					},
				},
			},
		},
	})
	var m ui.Model = cv

	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{X: 20, Y: 0, Width: 60, Height: 24}})
	m = typeText(t, m, "/jo")

	var cmd tea.Cmd
	m, cmd = m.Update(tea.MouseMsg{
		X:      24,
		Y:      22,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})

	require.NotNil(t, cmd, "mouse click should produce a cmd")
	m, _ = m.Update(cmd())

	m = typeText(t, m, "#general")
	_, cmd = enter(t, m)

	require.NotNil(t, cmd)
	sub := cmd().(components.CommandSubmitMsg)
	require.Equal(t, "/join #general", sub.Raw)
}

func TestContainsHighlightWord(t *testing.T) {
	tests := []struct {
		name   string
		words  []string
		nick   domain.Nick
		body   string
		expect bool
	}{
		{
			name:   "dollar_nick_matches_user_nick",
			words:  []string{"$nick"},
			nick:   "testuser",
			body:   "hey testuser check this out",
			expect: true,
		},
		{
			name:   "literal_word_matches",
			words:  []string{"check"},
			nick:   "testuser",
			body:   "hey testuser check this out",
			expect: true,
		},
		{
			name:   "case_insensitive",
			words:  []string{"CHECK"},
			nick:   "testuser",
			body:   "hey testuser check this out",
			expect: true,
		},
		{
			name:   "no_match",
			words:  []string{"foobar"},
			nick:   "testuser",
			body:   "hey testuser check this out",
			expect: false,
		},
		{
			name:   "no_highlight_words_configured",
			words:  nil,
			nick:   "testuser",
			body:   "hey testuser check this out",
			expect: false,
		},
		{
			name:   "empty_nick_placeholder_ignored",
			words:  []string{"$nick"},
			nick:   "",
			body:   "hey testuser check this out",
			expect: false,
		},
		{
			name:   "multiple_words_any_matches",
			words:  []string{"nope", "check"},
			nick:   "testuser",
			body:   "hey testuser check this out",
			expect: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := components.ContainsHighlightWord(tt.body, tt.words, tt.nick)
			require.Equal(t, tt.expect, got)
		})
	}
}
