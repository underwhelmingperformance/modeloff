package screens_test

import (
	"context"
	"testing"
	"time"

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

type fakeAPI struct{}

func (f *fakeAPI) ListModels(context.Context) ([]api.ModelInfo, error) {
	return nil, nil
}

func (f *fakeAPI) SendEvents(
	context.Context, domain.ModelID, string,
	[]protocol.IRCMessage, []protocol.IRCMessage,
) (protocol.ModelResponse, error) {
	return protocol.ModelResponse{Kind: protocol.ResponseSilence}, nil
}

func (f *fakeAPI) GenerateNick(context.Context, domain.ModelID) (domain.Nick, error) {
	return "fakenick", nil
}

func newTestSession(t *testing.T) *session.Session {
	t.Helper()

	s := storemod.NewFileStore(t.TempDir())
	sess := session.New(s, nil, &fakeAPI{}, newFakeConfigStore(), "testuser")

	return sess
}

func newTestSessionWithConfigStore(t *testing.T, cfgStore config.Store) *session.Session {
	t.Helper()

	s := storemod.NewFileStore(t.TempDir())
	sess := session.New(s, nil, &fakeAPI{}, cfgStore, "testuser")

	return sess
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

	_, err := sess.SendMessage(t.Context(), domain.ChannelName(channel), body)
	require.NoError(t, err)
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
}

func TestChatScreen_send_message(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	// Send a message.
	m, cmd := m.Update(components.MessageSubmitMsg{Text: "hello world"})
	require.NotNil(t, cmd)

	// Execute the send command.
	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "hello world")
}

func TestChatScreen_nick_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "nick", Args: "newnick"})
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

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "nick", Args: "newnick"})
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "save config")
	require.Contains(t, v, "context deadline exceeded")
}

func TestChatScreen_title_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "title", Args: "cool topic"})
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "topic for #general set to: cool topic")
}

func TestChatScreen_title_clear(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "title", Args: ""})
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "topic for #general cleared")
}

func TestChatScreen_whois_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	// Invite a model so there's an instance to whois.
	_, err := sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", "")
	require.NoError(t, err)

	m := initChatScreen(t, sess)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "whois", Args: "fakenick"})
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "fakenick is anthropic/claude-3-haiku")
}

func TestChatScreen_whois_unknown_nick(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "whois", Args: "nobody"})
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

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "list", Args: ""})
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "#general")
	require.Contains(t, v, "#random")
}

func TestChatScreen_list_empty(t *testing.T) {
	sess := newTestSession(t)

	m := initChatScreen(t, sess)

	_, cmd := m.Update(components.CommandSubmitMsg{Name: "list", Args: ""})
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "Welcome to modeloff")
	require.Contains(t, v, "/join #general")
}

func TestChatScreen_invite_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "invite", Args: "anthropic/claude-3-haiku"})
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "fakenick (anthropic/claude-3-haiku) has joined #general")
}

func TestChatScreen_invite_with_persona(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(components.CommandSubmitMsg{
		Name: "invite",
		Args: "anthropic/claude-3-haiku --persona Helpful assistant",
	})
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, `fakenick (anthropic/claude-3-haiku) has joined #general with persona "Helpful assistant"`)
}

func TestChatScreen_invite_no_args(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "invite", Args: ""})
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

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "join", Args: "#random"})
	require.NotNil(t, cmd)
	m, _ = m.Update(cmd())

	m, cmd = m.Update(components.CommandSubmitMsg{Name: "invite", Args: "fakenick"})
	require.NotNil(t, cmd)
	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "fakenick (anthropic/claude-3-haiku) has joined #random")
}

func TestChatScreen_kick_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	// Invite a model so there's someone to kick.
	_, err := sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", "")
	require.NoError(t, err)

	m := initChatScreen(t, sess)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "kick", Args: "fakenick"})
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "fakenick has been kicked from #general")
}

func TestChatScreen_config_usage(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "config", Args: ""})
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "usage: /config api-key <value> | /config poke-interval <duration>")
}

func TestChatScreen_config_set_api_key(t *testing.T) {
	cfgStore := newFakeConfigStore()
	sess := newTestSessionWithConfigStore(t, cfgStore)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "config", Args: "api-key test-key"})
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "OpenRouter API key saved. Restart modeloff to use it.")
	require.Equal(t, "test-key", cfgStore.cfg.APIKey)
}

func TestChatScreen_config_set_poke_interval(t *testing.T) {
	cfgStore := newFakeConfigStore()
	sess := newTestSessionWithConfigStore(t, cfgStore)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "config", Args: "poke-interval 10m"})
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

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "config", Args: "nonsense"})
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "unknown config key: nonsense")
}

func TestChatScreen_config_invalid_duration(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "config", Args: "poke-interval nope"})
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

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "msg", Args: "fakenick"})
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

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "msg", Args: "fakenick hello there"})
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

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "msg", Args: "nobody hello"})
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "no such nick: nobody")
}

func TestChatScreen_help_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "help", Args: ""})
	require.NotNil(t, cmd)

	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "/join")
	require.Contains(t, v, "/leave")
	require.Contains(t, v, "/list")
	require.Contains(t, v, "/invite")
	require.Contains(t, v, "/kick")
	require.Contains(t, v, "/msg")
	require.Contains(t, v, "/nick")
	require.Contains(t, v, "/title")
	require.Contains(t, v, "/whois")
	require.Contains(t, v, "/config")
	require.Contains(t, v, "/help")
}

func TestChatScreen_View_responsive(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	sizes := []struct{ w, h int }{
		{80, 24},
		{40, 10},
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
		{40, 10},
	}

	for _, sz := range sizes {
		v := m.View(sz.w, sz.h)
		require.NotEmpty(t, v, "View(%d, %d) should not be empty", sz.w, sz.h)
		require.Contains(t, v, "Welcome to modeloff")
		require.Contains(t, v, "/join #general")
	}

	narrow := m.View(28, 12)
	require.Contains(t, narrow, "Resize terminal to 40+ columns")
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
