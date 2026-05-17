package ui_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	uipkg "github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/screens"
	"github.com/laney/modeloff/internal/ui/uitest"
)

func TestApp_send_message_shows_pending_indicator(t *testing.T) {
	// The test exercises the user-`/msg`-triggered pending
	// indicator. Block the dispatch turn that handles the user's
	// PRIVMSG so the indicator stays on long enough for the
	// assertion; let unrelated turns (the INVITE-driven turn
	// AddModel triggers, the model-clients' first-attach turn,
	// etc.) return silence immediately. Without this guard the
	// model's dispatch goroutine is still parked in the INVITE
	// turn when the user submits, queues the new Message behind
	// it, and never emits the `ModelDispatchStarted` the pending
	// indicator depends on.
	release := make(chan struct{})

	apiClient := &integrationAPI{
		generateNickFn: func(context.Context, domain.ModelID, string, []domain.Nick) (domain.Nick, error) {
			return "fakenick", nil
		},
		sendEventsFn: func(
			_ context.Context,
			_ domain.ModelID,
			_ string,
			_ []protocol.IRCMessage,
			events []protocol.IRCMessage,
		) (protocol.ModelResponse, error) {
			if len(events) > 0 && events[0].Kind == protocol.KindPrivMsg {
				<-release
			}

			return protocol.ModelResponse{Kind: protocol.ResponseSilence}, nil
		},
	}
	sess, _, cfgStore := newIntegrationSession(t, apiClient)
	uitest.SeedChannel(t, sess, "#general")

	require.NoError(t, sess.AddModel(t.Context(), "#general", "test/model", ""))
	uitest.DrainEvents(sess)

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	tm.WaitFor("#general")

	tm.Submit("hello world")

	// The user's message should appear immediately alongside the
	// pending indicator, before the model has responded.
	tm.WaitFor("hello world", "responding")

	// Let the model respond. Wait for the pending indicator to clear.
	close(release)
	tm.WaitForCondition(func(out []byte) bool {
		return !bytes.Contains(out, []byte("responding"))
	})
}

func TestApp_nick_command_with_teatest(t *testing.T) {
	cfgStore := &integrationConfigStore{
		cfg: config.Config{
			UserNick:     "testuser",
			PokeInterval: 5 * time.Minute,
		},
	}
	sess, _ := newIntegrationSessionWithConfigStore(t, &integrationAPI{}, cfgStore)
	uitest.SeedChannel(t, sess, "#general")

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	tm.WaitFor("#general")

	tm.Submit("/nick newnick")
	tm.WaitFor("testuser is now known as newnick")

	require.Equal(t, "newnick", cfgStore.cfg.UserNick)
}

func TestApp_nick_command_reports_persist_error_with_teatest(t *testing.T) {
	cfgStore := &integrationConfigStore{
		cfg: config.Config{
			UserNick:     "testuser",
			PokeInterval: 5 * time.Minute,
		},
		saveErr: context.DeadlineExceeded,
	}
	sess, _ := newIntegrationSessionWithConfigStore(t, &integrationAPI{}, cfgStore)
	uitest.SeedChannel(t, sess, "#general")

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	tm.WaitFor("#general")

	tm.Submit("/nick newnick")
	tm.WaitFor("save config", "context deadline exceeded")
}

func TestApp_title_list_and_help_commands_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	root := uipkg.NewRoot(chatScreen)
	tm := uitest.New(t, root, teatest.WithInitialTermSize(256, 256))

	tm.WaitFor("#random")

	tm.Submit("/topic cool topic")
	tm.WaitFor("topic for #random set by testuser: cool topic")

	tm.Submit("/list")
	tm.WaitFor("#general (0)", "#random (0) — cool topic", "End of /list")

	tm.Submit("/topic")
	tm.WaitFor("topic for #random: cool topic", "set by testuser")

	tm.Submit("/help")
	tm.WaitFor("/join", "/help")
}

func TestApp_invite_whois_and_kick_commands_with_teatest(t *testing.T) {
	apiClient := &integrationAPI{
		generateNickFn: func(context.Context, domain.ModelID, string, []domain.Nick) (domain.Nick, error) {
			return "fakenick", nil
		},
	}
	sess, _, cfgStore := newIntegrationSession(t, apiClient)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen),
		teatest.WithInitialTermSize(120, 24))
	tm.WaitFor("#random")

	tm.Submit("/add-model")
	tm.WaitFor("usage: /add-model <model-id> [--persona <text>]")

	tm.Submit("/add-model anthropic/claude-3-haiku --persona Helpful assistant")
	tm.WaitFor("fakenick has joined #random")

	tm.Submit("/whois fakenick")
	tm.WaitFor("fakenick is anthropic/claude-3-haiku", "persona: Helpful assistant")

	tm.Submit("/kick fakenick")
	tm.WaitFor("fakenick was kicked from #random by testuser")
}

func TestApp_config_commands_with_teatest(t *testing.T) {
	cfgStore := &integrationConfigStore{
		cfg: config.Config{
			UserNick:     "testuser",
			PokeInterval: 5 * time.Minute,
		},
	}
	sess, _ := newIntegrationSessionWithConfigStore(t, &integrationAPI{}, cfgStore)
	uitest.SeedChannel(t, sess, "#general")

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	tm.WaitFor("#general")

	tm.Submit("/config")
	tm.WaitFor("/config requires a subcommand")

	tm.Submit("/config api-key test-key")
	tm.WaitFor("OpenRouter API key saved and activated.")

	tm.Submit("/config poke-interval 10m")
	tm.WaitFor("Poke interval set to 10m0s.")

	tm.Submit("/config timestamp-format %c")
	tm.WaitFor("timestamp format set to %c")

	tm.Submit(`/config timestamp-format ""`)
	tm.WaitFor("timestamps disabled")

	tm.Submit("/config nonsense")
	tm.WaitFor("unknown subcommand")

	tm.Submit("/config poke-interval nope")
	tm.WaitFor("invalid duration")

	require.Equal(t, "test-key", cfgStore.cfg.APIKey)
	require.Equal(t, 10*time.Minute, cfgStore.cfg.PokeInterval)
	require.NotNil(t, cfgStore.cfg.TimestampFormat)
	require.Equal(t, "", *cfgStore.cfg.TimestampFormat)
}

func TestApp_unknown_command_on_welcome_screen_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	tm.WaitFor("Welcome to modeloff")

	tm.Submit("/foo")
	tm.WaitFor("unknown command: /foo")

	require.Equal(t, []string{
		"Channels",
		"▸&modeloff",
		"No members",
		"✗ command: unknown command: /foo",
	}, visibleBodySegments(tm.CurrentView()))
}

func TestApp_welcome_join_command_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	tm.WaitFor("Welcome to modeloff")

	tm.Submit("/join #general")
	tm.WaitFor("Created channel #general")

	view := tm.CurrentView()
	require.Equal(t, []string{"Channels", "&modeloff", "▸#general"}, sidebarColumn(view))
	require.Equal(t, []string{
		"*** Created channel #general",
	}, normaliseContent(contentColumn(view)))
}

func TestApp_message_on_welcome_screen_rejected_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	tm.WaitFor("Welcome to modeloff")

	tm.Submit("hello world")
	tm.WaitFor("join a channel first")

	require.Equal(t, []string{
		"Channels",
		"▸&modeloff",
		"No members",
		"⚠ join a channel first",
	}, visibleBodySegments(tm.CurrentView()))
}

func TestApp_channel_command_on_welcome_screen_rejected_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	tm.WaitFor("Welcome to modeloff")

	tm.Submit("/part")
	tm.WaitFor("no channel to part from")
}

func TestApp_quit_command_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	tm.WaitFor("#general")

	tm.Submit("/quit")

	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))
}

func TestApp_unknown_target_commands_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	tm.WaitFor("#general")

	tm.Submit("/whois nobody")
	tm.WaitFor("no such nick: nobody")

	tm.Submit("/msg ghost hello")
	tm.WaitFor("no such nick: ghost")
}

func TestApp_unread_counts_clear_when_visiting_channel_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	uitest.SeedMessage(t, sess, "#general", "general unread")

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	// Wait for the sidebar to settle (both channels rendered and the
	// initial focus marker on #random) before issuing the Ctrl+U +
	// Ctrl+O navigation. WaitFor on cumulative output can match a
	// transient frame from before the focus marker is positioned,
	// causing the navigation keys to be processed against a stale
	// sidebar selection.
	tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "#general") &&
			strings.Contains(view, "▸#random")
	})

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlU})
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlO})

	// Anchor the assertion to the snapshot returned by WaitForView
	// (the exact view that satisfied the predicate). Polling the
	// rendered view rather than the cumulative output stream avoids
	// matching a transient frame that briefly contained "general
	// unread" before the focus actually landed.
	view := tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "general unread") &&
			strings.Contains(view, "▸#general")
	})

	require.Equal(t, []string{"Channels", "&modeloff", "▸#general", "#random"}, sidebarColumn(view))
}

func TestApp_input_history_and_sidebar_shortcuts_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedMessage(t, sess, "#general", "general msg")
	uitest.SeedChannel(t, sess, "#random")

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	tm.WaitFor("#random")

	tm.Submit("first history entry")
	tm.WaitFor("first", "history", "entry")

	tm.Submit("second history entry")
	tm.WaitFor("second", "history", "entry")

	tm.Type("draft-only")
	tm.Send(tea.KeyMsg{Type: tea.KeyUp})
	tm.Send(tea.KeyMsg{Type: tea.KeyUp})
	tm.Send(tea.KeyMsg{Type: tea.KeyDown})
	tm.Send(tea.KeyMsg{Type: tea.KeyDown})
	tm.WaitFor("draft-only")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlU})
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlO})
	tm.WaitFor("general msg")

	view := tm.CurrentView()
	require.Equal(t, []string{"Channels", "&modeloff", "▸#general", "#random"}, sidebarColumn(view))
	// The typed-but-unsent draft must survive a sidebar-driven channel
	// switch.
	require.Equal(t, "testuser > draft-only", inputLine(view),
		"draft text should remain in the input bar after switching channel")
}

func TestApp_ctrl_arrow_scroll_preserves_draft_with_teatest(t *testing.T) {
	t.Skip("Pending MessageList redesign: paced model-message delivery emits" +
		" a live `StoredEvent` long after `bufferEvent` has already added" +
		" the message to scrollback. When focus-change's `scrollbackCmd`" +
		" snapshot includes those messages, they render twice in the" +
		" message list (once via snapshot, once via the delayed live" +
		" append). The fix is to drop the dual-render: have MessageList" +
		" read from a getter pointed at the chat-screen's scrollback," +
		" removing `HistoryLoadedMsg`/`loadHistory` entirely.")

	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")

	for i := range 30 {
		uitest.SeedMessage(t, sess, "#general", fmt.Sprintf("message %d", i))
	}

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	// Wait for history to finish loading so Ctrl+Up has content to
	// scroll. A Ctrl+Up issued while the viewport still shows
	// "No messages yet" is a no-op, leaving message 29 to appear in
	// view once history finally lands and tripping the assertion.
	tm.WaitForViewContains("#general", "message 29")

	tm.Type("draft-only")
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlUp})

	// Both the typed draft and the Ctrl+Up scroll are async. Anchor
	// the assertion to the snapshot returned by WaitForView (the
	// exact view that satisfied the predicate); a re-sample via
	// CurrentView/RenderedView could race against subsequent state
	// churn before the assertion runs.
	view := tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "draft-only") &&
			!strings.Contains(view, "message 29")
	})

	require.Equal(t, []string{"Channels", "&modeloff", "▸#general"}, sidebarColumn(view))
	// The typed draft must survive a Ctrl+Up viewport scroll.
	require.Equal(t, "testuser > draft-only", inputLine(view),
		"draft text should remain in the input bar after scrolling")
	// Ctrl+Up scrolls the viewport up by one line, pushing the most
	// recent message off the bottom of the chat region.
	require.NotContains(t, normaliseContent(contentColumn(view)),
		"<testuser> message 29",
		"viewport should have scrolled the latest message out of view")
}

func TestApp_new_messages_divider_with_teatest(t *testing.T) {
	t.Skip("Pending MessageList redesign: paced model-message delivery emits" +
		" a live `StoredEvent` long after `bufferEvent` has already added" +
		" the message to scrollback. When focus-change's `scrollbackCmd`" +
		" snapshot includes those messages, they render twice in the" +
		" message list (once via snapshot, once via the delayed live" +
		" append). The fix is to drop the dual-render: have MessageList" +
		" read from a getter pointed at the chat-screen's scrollback," +
		" removing `HistoryLoadedMsg`/`loadHistory` entirely.")

	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")

	for i := range 30 {
		uitest.SeedMessage(t, sess, "#general", fmt.Sprintf("message %d", i))
	}

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	// Wait for history to finish loading before scrolling: a PgUp
	// issued while the viewport is still showing "No messages yet"
	// is a no-op and leaves us at the bottom.
	tm.WaitForViewContains("#general", "message 29")

	tm.Send(tea.KeyMsg{Type: tea.KeyPgUp})
	tm.WaitForViewContains("message 0")

	tm.Submit("fresh divider trigger 1")
	tm.Submit("fresh divider trigger 2")
	tm.Submit("fresh divider trigger 3")

	// Events are processed asynchronously by the UI events
	// goroutine. Scroll down one line at a time so the divider
	// (rendered into the off-screen content buffer the moment a
	// `StoredEvent` lands while the viewport is scrolled up)
	// eventually scrolls into view. `WaitForView` reconstructs the
	// current screen state from the rendered diff frames, so the
	// predicate sees the divider exactly when it becomes visible to
	// the user — unlike a substring match on the cumulative output
	// stream, which can be satisfied by a transient earlier frame.
	tm.WaitForView(func(view string) bool {
		if strings.Contains(view, "new messages") {
			return true
		}
		tm.Send(tea.KeyMsg{Type: tea.KeyCtrlDown})
		return false
	})

	require.Equal(t, expectedContentDivider(contentColumn(tm.FinalView())),
		findDividerLine(contentColumn(tm.FinalView())),
		"the new-messages divider must span the chat content column")
}

// expectedContentDivider returns the canonical "new messages" divider
// for the content-column width inferred from a rendered chat column.
// The divider is a centred " new messages " label flanked by `─`
// runes that fill the column.
func expectedContentDivider(lines []string) string {
	const label = " new messages "

	width := 0
	for _, line := range lines {
		if w := len([]rune(line)); w > width {
			width = w
		}
	}

	if width <= len([]rune(label)) {
		return label
	}

	pad := width - len([]rune(label))
	left := pad / 2
	right := pad - left

	return strings.Repeat("─", left) + label + strings.Repeat("─", right)
}

// findDividerLine returns the first "new messages" divider row from a
// rendered content column, or an empty string when none is present.
func findDividerLine(lines []string) string {
	for _, line := range lines {
		if bytes.Contains([]byte(line), []byte(" new messages ")) {
			return line
		}
	}

	return ""
}
