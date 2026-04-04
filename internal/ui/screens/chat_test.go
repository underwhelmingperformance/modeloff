package screens_test

import (
	"context"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
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

// initChatScreen constructs a ChatScreen from the given session,
// runs Init, and applies the resulting message so the screen is fully
// loaded and ready for interaction.
func initChatScreen(t *testing.T, sess *session.Session) ui.Model {
	t.Helper()

	cs := screens.NewChatScreen(t.Context(), sess)
	msg := cs.Init()()
	m, _ := cs.Update(msg)

	return m
}

func seedChannel(t *testing.T, sess *session.Session, name string) {
	t.Helper()

	_, err := sess.Join(t.Context(), name)
	require.NoError(t, err)
}

func seedMessage(t *testing.T, sess *session.Session, channel, body string) {
	t.Helper()

	_, _, err := sess.SendMessage(t.Context(), domain.ChannelName(channel), body)
	require.NoError(t, err)
}

func commandMsg(raw string) components.CommandSubmitMsg {
	return components.CommandSubmitMsg{Raw: raw}
}

func typeChars(t *testing.T, m ui.Model, text string) ui.Model {
	t.Helper()

	for _, r := range text {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	return m
}

func TestChatScreen_Init_loads_channels(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	seedMessage(t, sess, "#general", "hello")

	m := initChatScreen(t, sess)

	v := m.View(80, 24)
	require.Contains(t, v, "#general")
	require.Contains(t, v, "hello")
}

func TestChatScreen_Init_empty(t *testing.T) {
	sess := newTestSession(t)

	m := initChatScreen(t, sess)

	v := m.View(80, 24)
	require.Contains(t, v, "Welcome to modeloff")
	require.Contains(t, v, "Connected as")
	require.Contains(t, v, "testuser")
	require.Contains(t, v, "/join #general")
	require.Contains(t, v, "/config api-key <value>")
	require.Contains(t, v, "ctrl+d, ctrl+u, ctrl+o")

	// The layout renders normally: sidebar, input bar, and status bar
	// are all present.
	require.Contains(t, v, "No channels")
	require.Contains(t, v, ">")
}

func TestChatScreen_send_message(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(components.MessageSubmitMsg{Text: "hello world"})
	require.NotNil(t, cmd)
	require.Contains(t, m.View(80, 24), "responding")

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "hello world")
	require.NotContains(t, v, "responding")
}

func TestChatScreen_join_new_channel(t *testing.T) {
	sess := newTestSession(t)
	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/join #newchan"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "Created channel #newchan")
}

func TestChatScreen_join_existing_channel(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	seedChannel(t, sess, "#existing")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/join #general"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "#general")
	require.Contains(t, v, "testuser has joined #general")
}

func TestChatScreen_leave_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	seedChannel(t, sess, "#random")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/leave"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	require.NotEmpty(t, m.View(80, 24))
}

func TestChatScreen_nick_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/nick newnick"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "testuser is now known as newnick")
}

func TestChatScreen_nick_command_reports_persist_error(t *testing.T) {
	cfgStore := newFakeConfigStore()
	cfgStore.saveErr = context.DeadlineExceeded
	sess := newTestSessionWithConfigStore(t, cfgStore)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/nick newnick"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "save config")
	require.Contains(t, v, "context deadline exceeded")
}

func TestChatScreen_topic_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/topic cool topic"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "topic for #general set to: cool topic")
}

func TestChatScreen_topic_clear(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/topic"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "topic for #general cleared")
}

func TestChatScreen_whois_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	_, err := sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", "")
	require.NoError(t, err)

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/whois fakenick"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "fakenick is anthropic/claude-3-haiku")
}

func TestChatScreen_whois_unknown_nick(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/whois nobody"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "no such nick: nobody")
}

func TestChatScreen_list_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	seedChannel(t, sess, "#random")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/list"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "#general")
	require.Contains(t, v, "#random")
}

func TestChatScreen_list_empty(t *testing.T) {
	sess := newTestSession(t)
	m := initChatScreen(t, sess)

	_, cmd := m.Update(commandMsg("/list"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "no channels")
}

func TestChatScreen_invite_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/invite anthropic/claude-3-haiku"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "fakenick (anthropic/claude-3-haiku) has joined #general")
}

func TestChatScreen_invite_with_persona(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/invite anthropic/claude-3-haiku --persona Helpful assistant"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "fakenick (anthropic/claude-3-haiku) has joined #general")
	require.Contains(t, v, `persona "Helpful assistant"`)
}

func TestChatScreen_invite_no_args(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/invite"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "usage: /invite <model-id> [--persona <text>]")
}

func TestChatScreen_invite_existing_instance(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	seedChannel(t, sess, "#random")

	_, err := sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", "")
	require.NoError(t, err)

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/join #random"))
	require.NotNil(t, cmd)
	m, _ = m.Update(cmd())

	m, cmd = m.Update(commandMsg("/invite fakenick"))
	require.NotNil(t, cmd)
	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "fakenick (anthropic/claude-3-haiku) has joined #random")
}

func TestChatScreen_kick_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	_, err := sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", "")
	require.NoError(t, err)

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/kick fakenick"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "fakenick has been kicked from #general")
}

func TestChatScreen_config_usage(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/config"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "usage: /config api-key <value>")
	require.Contains(t, v, "poke-interval")
}

func TestChatScreen_config_set_api_key(t *testing.T) {
	cfgStore := newFakeConfigStore()
	sess := newTestSessionWithConfigStore(t, cfgStore)
	sess.SetAPIFactory(func(string) (api.Client, error) {
		return &fakeAPI{}, nil
	})
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/config api-key test-key"))
	require.NotNil(t, cmd)

	msg := cmd()
	m, cmd = m.Update(msg)
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "OpenRouter API key saved and activated.")
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

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/config api-key test-key"))
	require.NotNil(t, cmd)

	msg := cmd()
	m, cmd = m.Update(msg)
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())
	m = typeChars(t, m, "/invite anth")

	v := m.View(80, 24)
	require.Contains(t, v, "anthropic/claude-3-haiku")
}

func TestChatScreen_config_set_poke_interval(t *testing.T) {
	cfgStore := newFakeConfigStore()
	sess := newTestSessionWithConfigStore(t, cfgStore)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/config poke-interval 10m"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "Poke interval set to 10m0s.")
	require.Equal(t, 10*time.Minute, cfgStore.cfg.PokeInterval)
}

func TestChatScreen_config_invalid_subcommand(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/config nonsense"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "unknown config key: nonsense")
}

func TestChatScreen_config_invalid_duration(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/config poke-interval nope"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "invalid duration")
}

func TestChatScreen_msg_command_opens_dm(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	_, err := sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", "")
	require.NoError(t, err)

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/msg fakenick"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "Opened direct message with fakenick")
	require.Contains(t, v, "fakenick")
}

func TestChatScreen_msg_command_opens_dm_and_sends_message(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	_, err := sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", "")
	require.NoError(t, err)

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/msg fakenick hello there"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "hello there")
	require.Contains(t, v, "fakenick")
}

func TestChatScreen_msg_command_unknown_nick(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/msg nobody hello"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "no such nick: nobody")
}

func TestChatScreen_help_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/help"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "/join")
	require.Contains(t, v, "/help")
}

func TestChatScreen_invalid_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/nick"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "missing required argument <new-nick>")
}

func TestChatScreen_unknown_command_shows_error(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(commandMsg("/unknown"))
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "unknown command: /unknown")
}

func TestChatScreen_View_responsive(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	sizes := []struct{ w, h int }{
		{80, 24},
		{80, 10},
		{200, 50},
	}

	for _, sz := range sizes {
		v := m.View(sz.w, sz.h)
		require.NotEmpty(t, v, "View(%d, %d) should not be empty", sz.w, sz.h)
	}
}

func TestChatScreen_WelcomeState_responsive(t *testing.T) {
	sess := newTestSession(t)

	m := initChatScreen(t, sess)

	sizes := []struct{ w, h int }{
		{80, 24},
		{80, 10},
	}

	for _, sz := range sizes {
		v := m.View(sz.w, sz.h)
		require.NotEmpty(t, v, "View(%d, %d) should not be empty", sz.w, sz.h)
		require.Contains(t, v, "Welcome to modeloff")
		require.Contains(t, v, "/join #general")
	}

	narrow := m.View(79, 12)
	require.Contains(t, narrow, "Resize terminal to 80+ columns")
	require.NotContains(t, narrow, "Welcome to modeloff")
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
