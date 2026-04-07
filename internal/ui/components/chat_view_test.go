package components_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
	"github.com/laney/modeloff/internal/ui/theme"
)

var testEvents = []domain.StoredEvent{
	{Event: domain.ChannelMessage{Channel: "#general", From: "alice", Body: "hello", At: time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)}},
	{Event: domain.ChannelMessage{Channel: "#general", From: "bob", Body: "hi there", At: time.Date(2025, 1, 1, 10, 1, 0, 0, time.UTC)}},
	{Event: domain.ChannelMessage{Channel: "#general", From: "alice", Body: "how are you?", At: time.Date(2025, 1, 1, 10, 2, 0, 0, time.UTC)}},
}

// messagesToEvents converts domain messages into stored events.
func messagesToEvents(msgs []domain.Message) []domain.StoredEvent {
	events := make([]domain.StoredEvent, len(msgs))

	for i, m := range msgs {
		events[i] = domain.StoredEvent{
			Event: domain.ChannelMessage{
				Channel: m.Channel,
				From:    m.From,
				Body:    m.Body,
				Action:  m.Action,
				At:      m.SentAt,
			},
		}
	}

	return events
}

// newChatViewWithEvents creates a ChatView and loads events via HistoryLoadedMsg.
func newChatViewWithEvents(ch domain.ChannelName, userNick domain.Nick, topic string, events []domain.StoredEvent) components.ChatView {
	cv := components.NewChatView(ch, userNick, topic)

	if len(events) == 0 {
		return cv
	}

	m, _ := cv.Update(components.HistoryLoadedMsg{Events: events})

	return m.(components.ChatView)
}

func TestChatView_View_shows_messages(t *testing.T) {
	cv := newChatViewWithEvents("#general", "testuser", "", testEvents)
	v := cv.View(80, 24)

	require.Contains(t, v, "hello")
	require.Contains(t, v, "hi there")
	require.Contains(t, v, "how are you?")
	require.Contains(t, v, "alice")
	require.Contains(t, v, "bob")
}

func TestChatView_View_shows_timestamps(t *testing.T) {
	cv := newChatViewWithEvents("#general", "testuser", "", testEvents)
	v := cv.View(80, 24)

	require.Contains(t, v, "[10:00:00]")
	require.Contains(t, v, "[10:01:00]")
	require.Contains(t, v, "[10:02:00]")
}

func TestChatView_View_wraps_long_messages(t *testing.T) {
	longBody := strings.Repeat("word ", 30)
	events := []domain.StoredEvent{
		{Event: domain.ChannelMessage{Channel: "#general", From: "alice", Body: longBody, At: time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)}},
	}

	cv := newChatViewWithEvents("#general", "testuser", "", events)
	v := cv.View(40, 24)

	// The message should wrap, producing more rendered lines than one.
	require.Greater(t, lipgloss.Height(v), 3,
		"long message should wrap to multiple lines at narrow width")

	// All the content should still be present.
	require.Contains(t, v, "word")
	require.Contains(t, v, "alice")
}

func TestChatView_View_empty_messages(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "")
	v := cv.View(80, 24)

	require.Contains(t, v, "No messages yet")
}

func TestChatView_View_has_input_prompt(t *testing.T) {
	cv := newChatViewWithEvents("#general", "testuser", "", testEvents)
	v := cv.View(80, 24)

	require.Contains(t, v, ">")
}

func TestChatView_typing_goes_to_input(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "")
	var m ui.Model = cv

	m = typeText(t, m, "test message")
	m, cmd := enter(t, m)

	require.NotNil(t, cmd)

	msg := cmd()
	require.Equal(t, components.MessageSubmitMsg{Text: "test message"}, msg)

	_ = m
}

func TestChatView_command_from_input(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "")
	var m ui.Model = cv

	m = typeText(t, m, "/join #random")
	_, cmd := enter(t, m)

	require.NotNil(t, cmd)

	msg := cmd()
	require.Equal(t, components.CommandSubmitMsg{Raw: "/join #random"}, msg)
}

func TestChatView_messages_updated(t *testing.T) {
	cv := components.NewChatView("#general", "testuser", "")
	var m ui.Model = cv

	m, _ = m.Update(components.HistoryLoadedMsg{Events: []domain.StoredEvent{
		{Event: domain.ChannelMessage{Channel: "#general", From: "charlie", Body: "new message", At: time.Now()}},
	}})

	v := m.View(80, 24)
	require.Contains(t, v, "new message")
	require.Contains(t, v, "charlie")
}

func TestChatView_original_messages_persist(t *testing.T) {
	cv := newChatViewWithEvents("#general", "testuser", "", testEvents)
	var m ui.Model = cv

	// Append a new event — original messages should still be present.
	m, _ = m.Update(domain.StoredEvent{
		Event: domain.ChannelMessage{Channel: "#general", From: "charlie", Body: "extra", At: time.Now()},
	})

	v := m.View(80, 24)
	require.Contains(t, v, "hello")
	require.Contains(t, v, "extra")
}

func TestChatView_append_event(t *testing.T) {
	cv := newChatViewWithEvents("#general", "testuser", "", testEvents)
	var m ui.Model = cv

	m, _ = m.Update(domain.StoredEvent{
		Event: domain.ChannelMessage{Channel: "#general", From: "dave", Body: "appended message", At: time.Now()},
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

	cv := newChatViewWithEvents("#general", "testuser", "", messagesToEvents(msgs))
	var m ui.Model = cv

	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: 80, Height: 24}})

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

	cv := newChatViewWithEvents("#general", "testuser", "", messagesToEvents(msgs))
	var m ui.Model = cv
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: 80, Height: 24}})

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

	cv := newChatViewWithEvents("#general", "testuser", "", messagesToEvents(msgs))
	var m ui.Model = cv
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: 80, Height: 24}})

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlUp})

	v := m.View(80, 24)
	require.NotContains(t, v, "message 29")

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlDown})

	v = m.View(80, 24)
	require.Contains(t, v, "message 29")
}

func TestChatView_scroll_does_not_go_negative(t *testing.T) {
	cv := newChatViewWithEvents("#general", "testuser", "", testEvents)
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

	cv := newChatViewWithEvents("#general", "testuser", "", messagesToEvents(msgs))
	var m ui.Model = cv
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: 80, Height: 24}})

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

	cv := newChatViewWithEvents("#general", "alice", "", messagesToEvents(msgs))
	v := cv.View(80, 24)

	// Each nick is rendered with a colour derived from its name.
	aliceStyled := theme.NickStyle("alice").Render("<alice>")
	botStyled := theme.NickStyle("bot").Render("<bot>")

	require.Contains(t, v, aliceStyled)
	require.Contains(t, v, botStyled)
}

func TestChatView_shows_nick_in_input_area(t *testing.T) {
	cv := newChatViewWithEvents("#general", "alice", "", testEvents)
	v := cv.View(80, 24)

	require.Contains(t, v, "alice")
	require.Contains(t, v, ">")
}

func TestChatView_nick_updates_after_change(t *testing.T) {
	cv1 := newChatViewWithEvents("#general", "oldnick", "", testEvents)
	cv2 := newChatViewWithEvents("#general", "newnick", "", testEvents)

	v1 := cv1.View(80, 24)
	v2 := cv2.View(80, 24)

	require.Contains(t, v1, "oldnick")
	require.NotContains(t, v1, "newnick")
	require.Contains(t, v2, "newnick")
	require.NotContains(t, v2, "oldnick")
}

func TestChatView_topic_bar_shown(t *testing.T) {
	cv := newChatViewWithEvents("#general", "testuser", "Welcome to general", testEvents)
	v := cv.View(80, 24)

	require.Contains(t, v, "Welcome to general")
}

func TestChatView_no_topic_bar_when_empty(t *testing.T) {
	withTitle := newChatViewWithEvents("#general", "testuser", "some topic", testEvents)
	without := newChatViewWithEvents("#general", "testuser", "", testEvents)

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

	events := messagesToEvents(msgs)

	withTitle := newChatViewWithEvents("#general", "testuser", "A topic", events)
	without := newChatViewWithEvents("#general", "testuser", "", events)

	vWith := withTitle.View(80, 24)
	vWithout := without.View(80, 24)

	withLines := lipgloss.Height(vWith)
	withoutLines := lipgloss.Height(vWithout)

	// Both should fill the same total height.
	require.Equal(t, withoutLines, withLines)
}

func TestChatView_TopicUpdatedMsg_updates_topic_bar(t *testing.T) {
	var m ui.Model = newChatViewWithEvents("#general", "testuser", "", testEvents)

	// No topic initially.
	v := m.View(80, 24)
	stripped := ansi.Strip(v)
	require.NotContains(t, stripped, "new topic")

	// Send TopicUpdatedMsg.
	m, _ = m.Update(components.TopicUpdatedMsg{Topic: "new topic"})

	v = m.View(80, 24)
	stripped = ansi.Strip(v)
	require.Contains(t, stripped, "new topic")

	// Clear topic.
	m, _ = m.Update(components.TopicUpdatedMsg{Topic: ""})

	v = m.View(80, 24)
	stripped = ansi.Strip(v)
	require.NotContains(t, stripped, "new topic")
}

func TestChatView_pending_indicator(t *testing.T) {
	cv := newChatViewWithEvents("#general", "testuser", "", testEvents)
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

	cv := newChatViewWithEvents("#general", "testuser", "", messagesToEvents(msgs))
	var m ui.Model = cv

	m, _ = m.Update(components.PendingResponseMsg{Pending: true})

	v := m.View(80, 24)
	withLines := lipgloss.Height(v)

	// Total height should stay the same.
	require.Equal(t, 24, withLines)
	require.Contains(t, v, "responding")
}

func renderSingleEvent(event domain.StoredEvent) string {
	return renderSingleEventWithHighlight(event, nil, "testuser")
}

func renderSingleEventWithHighlight(event domain.StoredEvent, words []string, nick domain.Nick) string {
	cv := components.NewChatView("#test", nick, "")
	var m ui.Model = cv

	m, _ = m.Update(components.HistoryLoadedMsg{Events: []domain.StoredEvent{event}})
	m, _ = m.Update(components.CommandStateMsg{
		Commands: command.Set{
			Commands: []*command.Node{
				{Name: "join", Help: "Join or create a channel", Positionals: []command.Positional{{Name: "channel"}}},
				{Name: "help", Help: "Show available commands."},
			},
		},
	})

	if len(words) > 0 {
		m, _ = m.Update(components.HighlightWordsMsg{Words: words, UserNick: nick})
	}

	v := m.View(200, 24)

	return ansi.Strip(v)
}

func TestRenderLine_IRC_events(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name  string
		event domain.StoredEvent
		want  string
	}{
		{
			"join",
			domain.StoredEvent{Event: domain.ChannelJoin{Channel: "#general", Nick: "alice", At: now}},
			"*** alice has joined #general",
		},
		{
			"join_created",
			domain.StoredEvent{Event: domain.ChannelJoin{Channel: "#general", Nick: "alice", Created: true, At: now}},
			"*** Created channel #general",
		},
		{
			"part",
			domain.StoredEvent{Event: domain.ChannelPart{Channel: "#general", Nick: "alice", At: now}},
			"*** alice has left #general",
		},
		{
			"nick_change",
			domain.StoredEvent{Event: domain.ChannelNickChange{Channel: "#test", OldNick: "alice", NewNick: "bob", At: now}},
			"*** alice is now known as bob",
		},
		{
			"topic_set_with_author",
			domain.StoredEvent{Event: domain.ChannelTopicChange{Channel: "#general", Topic: "cool topic", By: "alice", At: now}},
			"*** topic for #general set by alice: cool topic",
		},
		{
			"topic_set_no_author",
			domain.StoredEvent{Event: domain.ChannelTopicChange{Channel: "#general", Topic: "cool topic", At: now}},
			"*** topic for #general set to: cool topic",
		},
		{
			"topic_cleared",
			domain.StoredEvent{Event: domain.ChannelTopicChange{Channel: "#general", Topic: "", By: "alice", At: now}},
			"*** topic for #general cleared by alice",
		},
		{
			"topic_info_with_metadata",
			domain.StoredEvent{Event: domain.ChannelTopicInfo{
				Channel: "#general", Topic: "cool topic",
				TopicSetBy: "alice", TopicSetAt: time.Date(2026, 4, 4, 23, 30, 0, 0, time.UTC),
				At: now,
			}},
			"*** topic for #general: cool topic (set by alice on 2026-04-04 23:30)",
		},
		{
			"topic_info_no_topic",
			domain.StoredEvent{Event: domain.ChannelTopicInfo{Channel: "#general", At: now}},
			"*** No topic set for #general",
		},
		{
			"model_invited",
			domain.StoredEvent{Event: domain.ChannelModelInvited{
				Channel: "#general", Nick: "botty", ModelID: "anthropic/haiku", At: now,
			}},
			"*** botty (anthropic/haiku) has joined #general",
		},
		{
			"model_invited_with_persona",
			domain.StoredEvent{Event: domain.ChannelModelInvited{
				Channel: "#general", Nick: "botty", ModelID: "anthropic/haiku", Persona: "helpful", At: now,
			}},
			`*** botty (anthropic/haiku) has joined #general with persona "helpful"`,
		},
		{
			"model_kicked",
			domain.StoredEvent{Event: domain.ChannelModelKicked{Channel: "#general", Nick: "botty", At: now}},
			"*** botty has been kicked from #general",
		},
		{
			"action_message",
			domain.StoredEvent{Event: domain.ChannelMessage{Channel: "#test", From: "alice", Body: "waves", Action: true, At: now}},
			"* alice waves",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderSingleEvent(tt.event)

			require.Contains(t, got, tt.want)
		})
	}
}

func TestRenderLine_application_feedback(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name  string
		event domain.StoredEvent
		want  string
	}{
		{
			"help",
			domain.StoredEvent{Event: domain.ChannelHelp{Channel: "#test", At: now}},
			"*** /join <channel>",
		},
		{
			"channel_list",
			domain.StoredEvent{Event: domain.ChannelListOutput{Channels: []domain.Channel{
				{Name: "#general"},
				{Name: "#random", Topic: "cool"},
			}, At: now}},
			"*** #general",
		},
		{
			"channel_list_empty",
			domain.StoredEvent{Event: domain.ChannelListOutput{At: now}},
			"*** no channels",
		},
		{
			"api_key_saved",
			domain.StoredEvent{Event: domain.ChannelSystemNotice{Channel: "#test", Text: "OpenRouter API key saved and activated.", At: now}},
			"OpenRouter API key saved",
		},
		{
			"poke_interval_set",
			domain.StoredEvent{Event: domain.ChannelSystemNotice{Channel: "#test", Text: "Poke interval set to 10m0s.", At: now}},
			"Poke interval set to 10m0s.",
		},
		{
			"usage_hint_config",
			domain.StoredEvent{Event: domain.ChannelUsageHint{
				Channel: "#test",
				Command: "config",
				Usage:   "/config api-key <value> | /config nick-model <model-id> | /config poke-interval <duration>",
				At:      now,
			}},
			"usage: /config api-key",
		},
		{
			"usage_hint_invite",
			domain.StoredEvent{Event: domain.ChannelUsageHint{
				Channel: "#test",
				Command: "invite",
				Usage:   "/invite <model-id> [--persona <text>]",
				At:      now,
			}},
			"usage: /invite",
		},
		{
			"no_channel",
			domain.StoredEvent{Event: domain.ChannelUsageHint{Usage: "join a channel first", At: now}},
			"join a channel first",
		},
		{
			"command_error",
			domain.StoredEvent{Event: domain.ChannelCommandError{Channel: "#test", Err: "unknown command: /foo", At: now}},
			"unknown command: /foo",
		},
		{
			"unknown_nick_error",
			domain.StoredEvent{Event: domain.ChannelCommandError{Channel: "#test", Err: "no such nick: ghost", At: now}},
			"no such nick: ghost",
		},
		{
			"config_changed",
			domain.StoredEvent{Event: domain.ChannelSystemNotice{Channel: "#test", Text: "API key saved", At: now}},
			"API key saved",
		},
		{
			"backend_error",
			domain.StoredEvent{Event: domain.ChannelCommandError{Channel: "#test", Err: "model invocation: connection refused", At: now}},
			"model invocation: connection refused",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderSingleEvent(tt.event)

			require.Contains(t, got, tt.want)
		})
	}
}

func TestNewMessagesDivider_fills_width(t *testing.T) {
	// Create enough events to overflow the viewport so we can scroll.
	events := make([]domain.StoredEvent, 30)
	for i := range events {
		events[i] = domain.StoredEvent{
			Event: domain.ChannelMessage{
				Channel: "#test",
				From:    "user",
				Body:    fmt.Sprintf("message %d", i),
				At:      time.Now(),
			},
		}
	}

	cv := newChatViewWithEvents("#test", "testuser", "", events)
	var m ui.Model = cv
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: 80, Height: 24}})

	// Scroll up, then add a new event to trigger the divider.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	m, _ = m.Update(domain.StoredEvent{
		Event: domain.ChannelMessage{Channel: "#test", From: "other", Body: "new arrival", At: time.Now()},
	})

	// Scroll back to bottom to see the divider.
	for range 5 {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	}

	v := m.View(80, 24)
	stripped := ansi.Strip(v)

	require.Contains(t, stripped, "new messages")
	require.Contains(t, stripped, "──")

	for line := range strings.SplitSeq(stripped, "\n") {
		if strings.Contains(line, "new messages") {
			require.Contains(t, line, "─")
			require.GreaterOrEqual(t, len([]rune(line)), 40,
				"divider should span a significant portion of the width")

			break
		}
	}
}

func TestChatView_command_popover_renders_and_completes(t *testing.T) {
	var m ui.Model = components.NewChatView("#general", "testuser", "")
	m, _ = m.Update(components.CommandStateMsg{
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
	var m ui.Model = components.NewChatView("#general", "testuser", "")
	m, _ = m.Update(components.CommandStateMsg{
		Commands: command.Set{
			Commands: []*command.Node{
				{Name: "join", Help: "Join a channel"},
				{Name: "part", Help: "Part from the current channel"},
				{Name: "quit", Help: "Exit modeloff"},
			},
		},
	})

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
	var m ui.Model = components.NewChatView("#general", "testuser", "")
	m, _ = m.Update(components.CommandStateMsg{
		Commands: command.Set{
			Commands: []*command.Node{
				{Name: "join", Help: "Join a channel", Positionals: []command.Positional{{Name: "channel"}}},
				{Name: "part", Help: "Part from the current channel"},
				{Name: "quit", Help: "Exit modeloff"},
			},
		},
	})

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
	cv := components.NewChatView("#general", "testuser", "")
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

	var cmd tea.Cmd
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd)

	sub := cmd().(components.MessageSubmitMsg)
	require.Equal(t, "hXello", sub.Text)
}

func TestChatView_divider_inserted_when_scrolled_up(t *testing.T) {
	events := make([]domain.StoredEvent, 30)
	for i := range events {
		events[i] = domain.StoredEvent{
			Event: domain.ChannelMessage{
				Channel: "#general",
				From:    "user",
				Body:    fmt.Sprintf("message %d", i),
				At:      time.Now(),
			},
		}
	}

	cv := newChatViewWithEvents("#general", "testuser", "", events)
	var m ui.Model = cv
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: 80, Height: 24}})

	// Scroll up so we're no longer at the bottom.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})

	v := m.View(80, 24)
	require.NotContains(t, v, "message 29")

	// Append new events while scrolled up.
	for i := 30; i < 33; i++ {
		m, _ = m.Update(domain.StoredEvent{
			Event: domain.ChannelMessage{
				Channel: "#general",
				From:    "other",
				Body:    fmt.Sprintf("new message %d", i),
				At:      time.Now(),
			},
		})
	}

	vScrolledUp := ansi.Strip(m.View(80, 24))
	t.Logf("while scrolled up:\n%s", vScrolledUp)

	// Scroll to bottom to see the divider.
	for range 5 {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	}

	v = m.View(80, 24)
	stripped := ansi.Strip(v)

	require.Contains(t, stripped, "new messages",
		"divider should be visible after scrolling down")
}

func TestChatView_no_divider_when_at_bottom(t *testing.T) {
	events := make([]domain.StoredEvent, 5)
	for i := range events {
		events[i] = domain.StoredEvent{
			Event: domain.ChannelMessage{
				Channel: "#general",
				From:    "user",
				Body:    fmt.Sprintf("message %d", i),
				At:      time.Now(),
			},
		}
	}

	cv := newChatViewWithEvents("#general", "testuser", "", events)
	var m ui.Model = cv
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: 80, Height: 24}})

	// Add more events while at bottom.
	for i := 5; i < 8; i++ {
		m, _ = m.Update(domain.StoredEvent{
			Event: domain.ChannelMessage{
				Channel: "#general",
				From:    "other",
				Body:    fmt.Sprintf("new message %d", i),
				At:      time.Now(),
			},
		})
	}

	v := m.View(80, 24)
	stripped := ansi.Strip(v)

	require.NotContains(t, stripped, "new messages",
		"no divider should appear when viewport is at bottom")
}

func TestChatView_stored_events_insert_divider_when_scrolled_up(t *testing.T) {
	events := make([]domain.StoredEvent, 30)
	for i := range 30 {
		events[i] = domain.StoredEvent{
			Event: domain.ChannelMessage{
				Channel: "#general",
				From:    "user",
				Body:    fmt.Sprintf("message %d", i),
			},
		}
	}

	cv := newChatViewWithEvents("#general", "testuser", "", events)
	var m ui.Model = cv
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: 80, Height: 24}})

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})

	for i := 30; i < 33; i++ {
		m, _ = m.Update(domain.StoredEvent{
			Event: domain.ChannelMessage{
				Channel: "#general",
				From:    "user",
				Body:    fmt.Sprintf("new message %d", i),
			},
		})
	}

	for range 5 {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	}

	v := ansi.Strip(m.View(80, 24))

	require.Contains(t, v, "new messages")
	require.Contains(t, v, "new message 30")
	require.Contains(t, v, "new message 32")
}

func TestChatView_stored_events_keep_divider_when_more_arrive_during_catch_up(t *testing.T) {
	events := make([]domain.StoredEvent, 30)
	for i := range 30 {
		events[i] = domain.StoredEvent{
			Event: domain.ChannelMessage{
				Channel: "#general",
				From:    "user",
				Body:    fmt.Sprintf("message %d", i),
			},
		}
	}

	cv := newChatViewWithEvents("#general", "testuser", "", events)
	var m ui.Model = cv
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: 80, Height: 24}})

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})

	// First new event while scrolled up — divider is armed.
	m, _ = m.Update(domain.StoredEvent{
		Event: domain.ChannelMessage{
			Channel: "#general",
			From:    "user",
			Body:    "new message 30",
		},
	})

	// Second new event while still scrolled up — divider stays.
	m, _ = m.Update(domain.StoredEvent{
		Event: domain.ChannelMessage{
			Channel: "#general",
			From:    "user",
			Body:    "new message 31",
		},
	})

	// Scroll to bottom to see both new events and the divider.
	for range 5 {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	}

	v := ansi.Strip(m.View(80, 24))

	require.Contains(t, v, "new messages")
	require.Contains(t, v, "new message 30")
	require.Contains(t, v, "new message 31")
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

	cv := newChatViewWithEvents("#general", "testuser", "", messagesToEvents(msgs))
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
	var m ui.Model = components.NewChatView("#general", "testuser", "")
	m, _ = m.Update(components.CommandStateMsg{
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
