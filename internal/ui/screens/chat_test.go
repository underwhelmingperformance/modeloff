package screens_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	storemod "github.com/laney/modeloff/internal/store"
	uipkg "github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/screens"
)

type fakeAPI struct {
	listModelsFn   func(context.Context) ([]api.ModelInfo, error)
	generateNickFn func(context.Context, domain.ModelID, domain.ModelID) (domain.Nick, error)
}

func (f *fakeAPI) ListModels(ctx context.Context) ([]api.ModelInfo, error) {
	if f.listModelsFn != nil {
		return f.listModelsFn(ctx)
	}

	return nil, nil
}

func (f *fakeAPI) SendEvents(
	context.Context, domain.ModelID, string,
	[]protocol.IRCMessage, []protocol.IRCMessage,
) (protocol.ModelResponse, error) {
	return protocol.ModelResponse{Kind: protocol.ResponseSilence}, nil
}

func (f *fakeAPI) GenerateNick(ctx context.Context, nickModel domain.ModelID, modelID domain.ModelID) (domain.Nick, error) {
	if f.generateNickFn != nil {
		return f.generateNickFn(ctx, nickModel, modelID)
	}

	return "fakenick", nil
}

func newTestSession(t *testing.T) *session.Session {
	t.Helper()

	s := storemod.NewFileStore(t.TempDir())
	return session.New(s, nil, &fakeAPI{}, newFakeConfigStore(), "testuser")
}

func newTestSessionWithConfigStore(t *testing.T, cfgStore config.Store) *session.Session {
	t.Helper()

	s := storemod.NewFileStore(t.TempDir())
	return session.New(s, nil, &fakeAPI{}, cfgStore, "testuser")
}

func newChatApp(t *testing.T, sess *session.Session) *teatest.TestModel {
	t.Helper()

	root := uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess))
	tm := teatest.NewTestModel(t, root, teatest.WithInitialTermSize(256, 256))
	t.Cleanup(func() { _ = tm.Quit() })

	return tm
}

func seedChannel(t *testing.T, sess *session.Session, name string) {
	t.Helper()

	_, err := sess.Join(t.Context(), name)
	require.NoError(t, err)
}

func seedMessage(t *testing.T, sess *session.Session, channel, body string) {
	t.Helper()

	_, err := sess.SendMessage(t.Context(), domain.ChannelName(channel), body)
	require.NoError(t, err)
}

func submitText(tm *teatest.TestModel, text string) {
	tm.Type(text)
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
}

func waitForOutput(t *testing.T, tm *teatest.TestModel, parts ...string) {
	t.Helper()

	teatest.WaitFor(t, tm.Output(), func(out []byte) bool {
		for _, part := range parts {
			if !bytes.Contains(out, []byte(part)) {
				return false
			}
		}

		return true
	}, teatest.WithDuration(2*time.Second), teatest.WithCheckInterval(10*time.Millisecond))
}

func finalView(t *testing.T, tm *teatest.TestModel) string {
	t.Helper()

	require.NoError(t, tm.Quit())

	model := tm.FinalModel(t, teatest.WithFinalTimeout(2*time.Second))
	root, ok := model.(uipkg.Root)
	require.True(t, ok, "expected Root, got %T", model)

	return root.View()
}

func TestChatScreen_Init_loads_channels(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	seedMessage(t, sess, "#general", "hello")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general", "hello")
}

func TestChatScreen_Init_empty(t *testing.T) {
	sess := newTestSession(t)

	tm := newChatApp(t, sess)
	waitForOutput(t, tm,
		"Welcome to modeloff",
		"Connected as",
		"testuser",
		"/join #general",
		"/config api-key <value>",
		"ctrl+d, ctrl+u, ctrl+o",
		"No channels",
		">",
	)
}

func TestChatScreen_send_message(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "hello world")
	waitForOutput(t, tm, "hello world")
}

func TestChatScreen_join_new_channel(t *testing.T) {
	sess := newTestSession(t)

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "Welcome to modeloff")

	submitText(tm, "/join #newchan")
	waitForOutput(t, tm, "Created channel #newchan")
}

func TestChatScreen_join_existing_channel(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	seedChannel(t, sess, "#existing")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#existing")

	submitText(tm, "/join #general")
	waitForOutput(t, tm, "#general", "testuser has joined #general")
}

func TestChatScreen_part_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	seedChannel(t, sess, "#random")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#random")

	submitText(tm, "/part")

	view := finalView(t, tm)
	require.NotEmpty(t, view)
}

func TestChatScreen_nick_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/nick newnick")
	waitForOutput(t, tm, "testuser is now known as newnick")
}

func TestChatScreen_nick_command_reports_persist_error(t *testing.T) {
	cfgStore := newFakeConfigStore()
	cfgStore.saveErr = context.DeadlineExceeded
	sess := newTestSessionWithConfigStore(t, cfgStore)
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/nick newnick")
	waitForOutput(t, tm, "save config", "context deadline exceeded")
}

func TestChatScreen_topic_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/topic cool topic")
	waitForOutput(t, tm, "topic for #general set to: cool topic")
}

func TestChatScreen_topic_clear(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/topic")
	waitForOutput(t, tm, "topic for #general cleared")
}

func TestChatScreen_whois_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	_, err := sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", "")
	require.NoError(t, err)

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/whois fakenick")
	waitForOutput(t, tm, "fakenick is anthropic/claude-3-haiku")
}

func TestChatScreen_whois_unknown_nick(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/whois nobody")
	waitForOutput(t, tm, "no such nick: nobody")
}

func TestChatScreen_list_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	seedChannel(t, sess, "#random")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#random")

	submitText(tm, "/list")
	waitForOutput(t, tm, "#general", "#random")
}

func TestChatScreen_list_empty(t *testing.T) {
	sess := newTestSession(t)

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "Welcome to modeloff")

	submitText(tm, "/list")
	waitForOutput(t, tm, "no channels")
}

func TestChatScreen_invite_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/invite anthropic/claude-3-haiku")
	waitForOutput(t, tm, "fakenick (anthropic/claude-3-haiku) has joined #general")
}

func TestChatScreen_invite_with_persona(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/invite anthropic/claude-3-haiku --persona Helpful assistant")
	waitForOutput(t, tm, "fakenick (anthropic/claude-3-haiku) has joined #general", `persona "Helpful assistant"`)
}

func TestChatScreen_invite_no_args(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/invite")
	waitForOutput(t, tm, "usage: /invite <model-id> [--persona <text>]")
}

func TestChatScreen_invite_existing_instance(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	seedChannel(t, sess, "#random")

	_, err := sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", "")
	require.NoError(t, err)

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#random")

	submitText(tm, "/join #random")
	waitForOutput(t, tm, "testuser has joined #random")

	submitText(tm, "/invite fakenick")
	waitForOutput(t, tm, "fakenick (anthropic/claude-3-haiku) has joined #random")
}

func TestChatScreen_kick_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	_, err := sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", "")
	require.NoError(t, err)

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/kick fakenick")
	waitForOutput(t, tm, "fakenick has been kicked from #general")
}

func TestChatScreen_config_usage(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/config")
	waitForOutput(t, tm, "usage: /config api-key <value>", "poke-interval")
}

func TestChatScreen_config_set_api_key(t *testing.T) {
	cfgStore := newFakeConfigStore()
	sess := newTestSessionWithConfigStore(t, cfgStore)
	sess.SetAPIFactory(func(string) (api.Client, error) {
		return &fakeAPI{}, nil
	})
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/config api-key test-key")
	waitForOutput(t, tm, "OpenRouter API key saved and activated.")

	require.Equal(t, "test-key", cfgStore.cfg.APIKey)
}

func TestChatScreen_config_set_api_key_updates_live_model_suggestions(t *testing.T) {
	cfgStore := newFakeConfigStore()
	sess := newTestSessionWithConfigStore(t, cfgStore)
	sess.SetAPIFactory(func(string) (api.Client, error) {
		return &fakeAPI{
			listModelsFn: func(context.Context) ([]api.ModelInfo, error) {
				return []api.ModelInfo{
					{ID: "anthropic/claude-3-haiku", Name: "Claude Haiku"},
				}, nil
			},
		}, nil
	})
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/config api-key test-key")
	waitForOutput(t, tm, "OpenRouter API key saved and activated.")

	tm.Type("/invite anth")
	waitForOutput(t, tm, "anthropic/claude-3-haiku")
}

func TestChatScreen_config_set_poke_interval(t *testing.T) {
	cfgStore := newFakeConfigStore()
	sess := newTestSessionWithConfigStore(t, cfgStore)
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/config poke-interval 10m")
	waitForOutput(t, tm, "Poke interval set to 10m0s.")

	require.Equal(t, 10*time.Minute, cfgStore.cfg.PokeInterval)
}

func TestChatScreen_config_invalid_subcommand(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/config nonsense")
	waitForOutput(t, tm, "unknown config key: nonsense")
}

func TestChatScreen_config_invalid_duration(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/config poke-interval nope")
	waitForOutput(t, tm, "invalid duration")
}

func TestChatScreen_msg_command_opens_dm(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	_, err := sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", "")
	require.NoError(t, err)

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/msg fakenick")
	waitForOutput(t, tm, "Opened direct message with fakenick")
}

func TestChatScreen_msg_command_opens_dm_and_sends_message(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	_, err := sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", "")
	require.NoError(t, err)

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/msg fakenick hello there")
	waitForOutput(t, tm, "hello there", "fakenick")
}

func TestChatScreen_msg_command_unknown_nick(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/msg nobody hello")
	waitForOutput(t, tm, "no such nick: nobody")
}

func TestChatScreen_help_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/help")
	waitForOutput(t, tm, "/join", "/help")
}

func TestChatScreen_invalid_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/nick")
	waitForOutput(t, tm, "missing required argument <new-nick>")
}

func TestChatScreen_unknown_command_shows_error(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	submitText(tm, "/unknown")
	waitForOutput(t, tm, "unknown command: /unknown")
}

func TestChatScreen_View_responsive(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	view := finalView(t, tm)
	require.NotEmpty(t, view)
}

func TestChatScreen_KeyBindings_collect_active_bindings(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	view := finalView(t, tm)
	require.Contains(t, view, "↵ send")
	require.Contains(t, view, "^N nicks")
	require.Contains(t, view, "^C quit")
}

func TestChatScreen_KeyBindings_switch_to_popover_bindings(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "#general")

	tm.Type("/")

	// The popover adds Tab, ↑↓, Esc bindings. At 80 columns the
	// status bar falls back to key-only mode, so check for keys.
	waitForOutput(t, tm, "Tab")

	view := finalView(t, tm)
	require.Contains(t, view, "Tab")
	require.Contains(t, view, "Esc")
}

func TestChatScreen_WelcomeState_responsive(t *testing.T) {
	sess := newTestSession(t)

	tm := newChatApp(t, sess)
	waitForOutput(t, tm, "Welcome to modeloff", "/join #general")
}

type fakeConfigStore struct {
	cfg     config.Config
	saveErr error
}

func newFakeConfigStore() *fakeConfigStore {
	return &fakeConfigStore{
		cfg: config.Config{
			UserNick:     "testuser",
			PokeInterval: 5 * time.Minute,
		},
	}
}

func (f *fakeConfigStore) Load() (config.Config, error) {
	return f.cfg, nil
}

func (f *fakeConfigStore) Save(cfg config.Config) error {
	if f.saveErr != nil {
		return f.saveErr
	}

	f.cfg = cfg
	return nil
}
