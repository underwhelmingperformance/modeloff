package ui_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
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
	release := make(chan struct{})
	apiClient := &integrationAPI{
		generateNickFn: func(context.Context, domain.ModelID, domain.ModelID) (domain.Nick, error) {
			return "fakenick", nil
		},
		sendEventsFn: func(
			context.Context,
			domain.ModelID,
			string,
			[]protocol.IRCMessage,
			[]protocol.IRCMessage,
		) (protocol.ModelResponse, error) {
			<-release

			return protocol.ModelResponse{Kind: protocol.ResponseSilence}, nil
		},
	}
	sess, _, cfgStore := newIntegrationSession(t, apiClient)
	uitest.SeedChannel(t, sess, "#general")

	require.NoError(t, sess.AddModel(t.Context(), "#general", "test/model", ""))
	uitest.DrainEvents(sess)

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess, cfgStore)))
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

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess, cfgStore)))
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

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess, cfgStore)))
	tm.WaitFor("#general")

	tm.Submit("/nick newnick")
	tm.WaitFor("save config", "context deadline exceeded")
}

func TestApp_title_list_and_help_commands_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	root := uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess, cfgStore))
	tm := uitest.New(t, root, teatest.WithInitialTermSize(256, 256))

	tm.WaitFor("#random")

	tm.Submit("/topic cool topic")
	tm.WaitFor("topic for #random set by testuser: cool topic")

	tm.Submit("/list")
	tm.WaitFor("#general", "#random — cool topic")

	tm.Submit("/topic")
	tm.WaitFor("topic for #random: cool topic", "set by testuser")

	tm.Submit("/help")
	tm.WaitFor("/join", "/help")
}

func TestApp_invite_whois_and_kick_commands_with_teatest(t *testing.T) {
	apiClient := &integrationAPI{
		generateNickFn: func(context.Context, domain.ModelID, domain.ModelID) (domain.Nick, error) {
			return "fakenick", nil
		},
	}
	sess, _, cfgStore := newIntegrationSession(t, apiClient)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess, cfgStore)),
		teatest.WithInitialTermSize(120, 24))
	tm.WaitFor("#random")

	tm.Submit("/add-model")
	tm.WaitFor("usage: /add-model <model-id> [--persona <text>]")

	tm.Submit("/add-model anthropic/claude-3-haiku --persona Helpful assistant")
	tm.WaitFor("fakenick (anthropic/claude-3-haiku) has joined #random")

	tm.Submit("/whois fakenick")
	tm.WaitFor("fakenick is anthropic/claude-3-haiku", "persona: Helpful assistant")

	tm.Submit("/kick fakenick")
	tm.WaitFor("fakenick has been kicked from #random")
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

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess, cfgStore)))
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

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess, cfgStore)))
	tm.WaitFor("Welcome to modeloff")

	tm.Submit("/foo")
	tm.WaitFor("unknown command: /foo")

	view := tm.CurrentView()
	require.Contains(t, view, "unknown command: /foo")
	require.NotContains(t, view, "<testuser>")
}

func TestApp_welcome_join_command_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess, cfgStore)))
	tm.WaitFor("Welcome to modeloff")

	tm.Submit("/join #general")
	tm.WaitFor("Created channel #general")

	view := tm.CurrentView()
	require.Contains(t, view, "#general")
	require.NotContains(t, view, "Welcome to modeloff")
}

func TestApp_message_on_welcome_screen_rejected_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess, cfgStore)))
	tm.WaitFor("Welcome to modeloff")

	tm.Submit("hello world")
	tm.WaitFor("join a channel first")

	view := tm.CurrentView()
	require.Contains(t, view, "join a channel first")
	require.NotContains(t, view, "<testuser>")
}

func TestApp_channel_command_on_welcome_screen_rejected_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess, cfgStore)))
	tm.WaitFor("Welcome to modeloff")

	tm.Submit("/part")
	tm.WaitFor("join a channel first")
}

func TestApp_quit_command_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess, cfgStore)))
	tm.WaitFor("#general")

	tm.Submit("/quit")

	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))
}

func TestApp_unknown_target_commands_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess, cfgStore)))
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

	require.NoError(t, sess.SendMessage(t.Context(), "#general", "general unread"))
	uitest.DrainEvents(sess)

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess, cfgStore)))
	// Unread count includes all events: JoinEvent + ModeChangeEvent + ChannelMessage.
	tm.WaitFor("#general (3)", "#random")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlU})
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlO})
	tm.WaitFor("general", "unread")

	view := tm.CurrentView()
	require.Contains(t, view, "general")
	require.Contains(t, view, "unread")
	require.NotContains(t, view, "#general (3)")
}

func TestApp_input_history_and_sidebar_shortcuts_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess, cfgStore)))
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
	tm.WaitFor("testuser", "has joined #general")

	view := ansi.Strip(tm.CurrentView())
	require.Contains(t, view, "#general")
	require.Contains(t, view, "draft-only")
}

func TestApp_ctrl_arrow_scroll_preserves_draft_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")

	for i := range 30 {
		require.NoError(t, sess.SendMessage(t.Context(), "#general", fmt.Sprintf("message %d", i)))
	}

	uitest.DrainEvents(sess)

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess, cfgStore)))
	tm.WaitFor("#general")

	tm.Type("draft-only")
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlUp})

	view := tm.CurrentView()
	require.Contains(t, view, "draft-only")
	require.NotContains(t, view, "message 29")
}

func TestApp_new_messages_divider_with_teatest(t *testing.T) {
	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")

	for i := range 30 {
		require.NoError(t, sess.SendMessage(t.Context(), "#general", fmt.Sprintf("message %d", i)))
	}

	uitest.DrainEvents(sess)

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess, cfgStore)))
	tm.WaitFor("#general")

	tm.Send(tea.KeyMsg{Type: tea.KeyPgUp})
	tm.WaitFor("message 0")

	tm.Submit("fresh divider trigger 1")
	tm.Submit("fresh divider trigger 2")
	tm.Submit("fresh divider trigger 3")

	// The messages are processed asynchronously — the session emits
	// events that the UI picks up on a later render cycle. Scroll
	// down one line at a time so events arrive while the viewport
	// is still scrolled up (setting showDivider). Once the divider
	// scrolls into view the condition matches.
	tm.WaitForCondition(func(out []byte) bool {
		tm.Send(tea.KeyMsg{Type: tea.KeyCtrlDown})
		return bytes.Contains(out, []byte("new messages"))
	})

	view := ansi.Strip(tm.FinalView())
	require.Contains(t, view, "new messages")
}
