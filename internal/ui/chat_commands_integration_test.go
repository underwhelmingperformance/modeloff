package ui_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	uipkg "github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/screens"
)

func TestApp_send_message_shows_pending_indicator(t *testing.T) {
	release := make(chan struct{})
	apiClient := &integrationAPI{
		generateNickFn: func(context.Context, domain.ModelID) (domain.Nick, error) {
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
	sess, _ := newIntegrationSession(t, apiClient)
	seedChannel(t, sess, "#general")

	_, err := sess.Invite(t.Context(), "#general", "test/model", "")
	require.NoError(t, err)

	tm := newTestApp(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	waitForOutput(t, tm, "#general")

	submitText(tm, "hello world")
	waitForOutput(t, tm, "responding")

	close(release)
	waitForOutput(t, tm, "hello world")

	view := finalView(t, tm)
	require.Contains(t, view, "hello world")
	require.NotContains(t, view, "responding")
}

func TestApp_nick_command_with_teatest(t *testing.T) {
	cfgStore := &integrationConfigStore{
		cfg: config.Config{
			UserNick:     "testuser",
			PokeInterval: 5 * time.Minute,
		},
	}
	sess, _ := newIntegrationSessionWithConfigStore(t, &integrationAPI{}, cfgStore)
	seedChannel(t, sess, "#general")

	tm := newTestApp(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	waitForOutput(t, tm, "#general")

	submitText(tm, "/nick newnick")
	waitForOutput(t, tm, "testuser is now known as newnick")

	require.Equal(t, "newnick", cfgStore.cfg.UserNick)

	view := finalView(t, tm)
	require.Contains(t, view, "testuser is now known as newnick")
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
	seedChannel(t, sess, "#general")

	tm := newTestApp(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	waitForOutput(t, tm, "#general")

	submitText(tm, "/nick newnick")
	waitForOutput(t, tm, "save config", "context deadline exceeded")

	view := finalView(t, tm)
	require.Contains(t, view, "save config")
	require.Contains(t, view, "context deadline exceeded")
}

func TestApp_title_list_and_help_commands_with_teatest(t *testing.T) {
	sess, _ := newIntegrationSession(t, &integrationAPI{})
	seedChannel(t, sess, "#general")
	seedChannel(t, sess, "#random")

	tm := newTestApp(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	waitForOutput(t, tm, "#random")

	submitText(tm, "/title cool topic")
	waitForOutput(t, tm, "topic for #random set to: cool topic")

	submitText(tm, "/list")
	waitForOutput(t, tm, "#general", "#random — cool topic")

	submitText(tm, "/title")
	waitForOutput(t, tm, "topic for #random cleared")

	submitText(tm, "/help")
	waitForOutput(t, tm, "/join <channel>", "/config api-key <key>", "/help")

	view := finalView(t, tm)
	require.Contains(t, view, "/join <channel>")
	require.Contains(t, view, "/help")
}

func TestApp_invite_whois_and_kick_commands_with_teatest(t *testing.T) {
	apiClient := &integrationAPI{
		generateNickFn: func(context.Context, domain.ModelID) (domain.Nick, error) {
			return "fakenick", nil
		},
	}
	sess, _ := newIntegrationSession(t, apiClient)
	seedChannel(t, sess, "#general")
	seedChannel(t, sess, "#random")

	tm := newTestApp(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	waitForOutput(t, tm, "#random")

	submitText(tm, "/invite")
	waitForOutput(t, tm, "usage: /invite <model-id> [--persona <text>]")

	submitText(tm, "/invite anthropic/claude-3-haiku --persona Helpful assistant")
	waitForOutput(t, tm, "fakenick (anthropic/claude-3-haiku) has joined #random")

	submitText(tm, "/whois fakenick")
	waitForOutput(t, tm, "fakenick is anthropic/claude-3-haiku", "persona: Helpful assistant")

	submitText(tm, "/kick fakenick")
	waitForOutput(t, tm, "fakenick has been kicked from #random")

	view := finalView(t, tm)
	require.Contains(t, view, "fakenick has been kicked from #random")
}

func TestApp_config_commands_with_teatest(t *testing.T) {
	cfgStore := &integrationConfigStore{
		cfg: config.Config{
			UserNick:     "testuser",
			PokeInterval: 5 * time.Minute,
		},
	}
	sess, _ := newIntegrationSessionWithConfigStore(t, &integrationAPI{}, cfgStore)
	seedChannel(t, sess, "#general")

	tm := newTestApp(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	waitForOutput(t, tm, "#general")

	submitText(tm, "/config")
	waitForOutput(t, tm, "usage: /config api-key")

	submitText(tm, "/config api-key test-key")
	waitForOutput(t, tm, "OpenRouter API key saved. Restart modeloff to use it.")

	submitText(tm, "/config poke-interval 10m")
	waitForOutput(t, tm, "Poke interval set to 10m0s.")

	submitText(tm, "/config nonsense")
	waitForOutput(t, tm, "unknown config key: nonsense")

	submitText(tm, "/config poke-interval nope")
	waitForOutput(t, tm, "invalid duration")

	require.Equal(t, "test-key", cfgStore.cfg.APIKey)
	require.Equal(t, 10*time.Minute, cfgStore.cfg.PokeInterval)

	view := finalView(t, tm)
	require.Contains(t, view, "invalid duration")
}

func TestApp_unknown_command_on_welcome_screen_with_teatest(t *testing.T) {
	sess, _ := newIntegrationSession(t, &integrationAPI{})

	tm := newTestApp(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	waitForOutput(t, tm, "Welcome to modeloff")

	submitText(tm, "/foo")
	waitForOutput(t, tm, "unknown command: /foo")

	view := finalView(t, tm)
	require.Contains(t, view, "unknown command: /foo")
	require.NotContains(t, view, "<testuser>")
}

func TestApp_message_on_welcome_screen_rejected_with_teatest(t *testing.T) {
	sess, _ := newIntegrationSession(t, &integrationAPI{})

	tm := newTestApp(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	waitForOutput(t, tm, "Welcome to modeloff")

	submitText(tm, "hello world")
	waitForOutput(t, tm, "join a channel first")

	view := finalView(t, tm)
	require.Contains(t, view, "join a channel first")
	require.NotContains(t, view, "<testuser>")
}

func TestApp_channel_command_on_welcome_screen_rejected_with_teatest(t *testing.T) {
	sess, _ := newIntegrationSession(t, &integrationAPI{})

	tm := newTestApp(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	waitForOutput(t, tm, "Welcome to modeloff")

	submitText(tm, "/leave")
	waitForOutput(t, tm, "join a channel first")

	view := finalView(t, tm)
	require.Contains(t, view, "join a channel first")
}

func TestApp_unknown_target_commands_with_teatest(t *testing.T) {
	sess, _ := newIntegrationSession(t, &integrationAPI{})
	seedChannel(t, sess, "#general")

	tm := newTestApp(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	waitForOutput(t, tm, "#general")

	submitText(tm, "/whois nobody")
	waitForOutput(t, tm, "no such nick: nobody")

	submitText(tm, "/msg ghost hello")
	waitForOutput(t, tm, "no such nick: ghost")

	view := finalView(t, tm)
	require.Contains(t, view, "no such nick: ghost")
}
