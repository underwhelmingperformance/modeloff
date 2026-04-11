package screens_test

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/x/exp/teatest"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/store/storetest"
	uipkg "github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/screens"
	"github.com/laney/modeloff/internal/ui/uitest"
)

func newTestSession(t *testing.T) *session.Session {
	t.Helper()

	s := storetest.NewMemoryStore(t)
	return session.New(s, nil, &uitest.FakeAPI{}, "testuser", "", "")
}

func newTestSessionWithConfigStore(t *testing.T, cfgStore config.Store) *session.Session {
	t.Helper()

	s := storetest.NewMemoryStore(t)
	cfg, _ := cfgStore.Load(context.Background())
	return session.New(s, nil, &uitest.FakeAPI{}, "testuser", cfg.APIKey, cfg.SmallModel)
}

func newChatApp(t *testing.T, sess *session.Session) *uitest.App {
	t.Helper()

	return newChatAppWithConfig(t, sess, newFakeConfigStore())
}

func newChatAppWithConfig(t *testing.T, sess *session.Session, cfgStore config.Store) *uitest.App {
	t.Helper()

	uitest.DrainEvents(sess)

	root := uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess, cfgStore))
	return uitest.New(t, root, teatest.WithInitialTermSize(256, 256))
}

func newChatAppInChannel(t *testing.T, channel domain.ChannelName) (*uitest.App, *session.Session) {
	t.Helper()

	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, string(channel))

	tm := newChatApp(t, sess)
	tm.WaitFor(string(channel))

	return tm, sess
}

func TestChatScreen_Init_loads_channels(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedMessage(t, sess, "#general", "hello")

	tm := newChatApp(t, sess)
	tm.WaitFor("#general", "hello")
}

func TestChatScreen_Init_empty(t *testing.T) {
	sess := newTestSession(t)

	tm := newChatApp(t, sess)
	tm.WaitFor(
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
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("hello world")
	tm.WaitFor("hello world")
}

func TestChatScreen_join_new_channel(t *testing.T) {
	sess := newTestSession(t)

	tm := newChatApp(t, sess)
	tm.WaitFor("Welcome to modeloff")

	tm.Submit("/join #newchan")
	tm.WaitFor("Created channel #newchan")
}

func TestChatScreen_join_existing_channel(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#existing")

	tm := newChatApp(t, sess)
	tm.WaitFor("#existing")

	tm.Submit("/join #general")
	tm.WaitFor("#general", "testuser has joined #general")
}

func TestChatScreen_part_command(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	tm := newChatApp(t, sess)
	tm.WaitFor("#random")

	tm.Submit("/part")
}

func TestChatScreen_nick_command(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/nick newnick")
	tm.WaitFor("testuser is now known as newnick")
}

func TestChatScreen_nick_command_reports_persist_error(t *testing.T) {
	cfgStore := newFakeConfigStore()
	cfgStore.saveErr = context.DeadlineExceeded
	sess := newTestSessionWithConfigStore(t, cfgStore)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatAppWithConfig(t, sess, cfgStore)
	tm.WaitFor("#general")

	tm.Submit("/nick newnick")
	tm.WaitFor("save config", "context deadline exceeded")
}

func TestChatScreen_topic_command(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/topic cool topic")
	tm.WaitFor("topic for #general set by testuser: cool topic")
}

func TestChatScreen_topic_show_info(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	// Set a topic first.
	tm.Submit("/topic cool topic")
	tm.WaitFor("cool topic")

	// /topic with no args shows info.
	tm.Submit("/topic")
	tm.WaitFor("topic for #general: cool topic", "set by testuser")
}

func TestChatScreen_topic_show_info_no_topic(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/topic")
	tm.WaitFor("No topic set for #general")
}

func TestChatScreen_whois_command(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	require.NoError(t, sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", ""))

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Submit("/whois fakenick")
	tm.WaitFor("fakenick is anthropic/claude-3-haiku")
}

func TestChatScreen_whois_unknown_nick(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/whois nobody")
	tm.WaitFor("no such nick: nobody")
}

func TestChatScreen_list_command(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	tm := newChatApp(t, sess)
	tm.WaitFor("#random")

	tm.Submit("/list")
	tm.WaitFor("#general", "#random")
}

func TestChatScreen_list_empty(t *testing.T) {
	sess := newTestSession(t)

	tm := newChatApp(t, sess)
	tm.WaitFor("Welcome to modeloff")

	tm.Submit("/list")
	tm.WaitFor("no channels")
}

func TestChatScreen_invite_command(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/invite anthropic/claude-3-haiku")
	tm.WaitFor("fakenick (anthropic/claude-3-haiku) has joined #general")
}

func TestChatScreen_invite_with_persona(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/invite anthropic/claude-3-haiku --persona Helpful assistant")
	tm.WaitFor("fakenick (anthropic/claude-3-haiku) has joined #general", `persona "Helpful assistant"`)
}

func TestChatScreen_invite_no_args(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/invite")
	tm.WaitFor("usage: /invite <model-id> [--persona <text>]")
}

func TestChatScreen_invite_existing_instance(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	require.NoError(t, sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", ""))

	tm := newChatApp(t, sess)
	tm.WaitFor("#random")

	tm.Submit("/join #random")
	tm.WaitFor("testuser has joined #random")

	tm.Submit("/invite fakenick")
	tm.WaitFor("fakenick (anthropic/claude-3-haiku) has joined #random")
}

func TestChatScreen_kick_command(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	require.NoError(t, sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", ""))

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Submit("/kick fakenick")
	tm.WaitFor("fakenick has been kicked from #general")
}

func TestChatScreen_config_no_subcommand(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/config")
	tm.WaitFor("/config requires a subcommand")
}

func TestChatScreen_config_set_api_key(t *testing.T) {
	cfgStore := newFakeConfigStore()
	sess := newTestSessionWithConfigStore(t, cfgStore)
	sess.SetAPIFactory(func(string, string) (api.Client, error) {
		return &uitest.FakeAPI{}, nil
	})
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatAppWithConfig(t, sess, cfgStore)
	tm.WaitFor("#general")

	tm.Submit("/config api-key test-key")
	tm.WaitFor("OpenRouter API key saved and activated.")

	require.Equal(t, "test-key", cfgStore.cfg.APIKey)
}

func TestChatScreen_config_set_api_key_updates_live_model_suggestions(t *testing.T) {
	cfgStore := newFakeConfigStore()
	sess := newTestSessionWithConfigStore(t, cfgStore)
	sess.SetAPIFactory(func(string, string) (api.Client, error) {
		return &uitest.FakeAPI{
			ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
				return []api.ModelInfo{
					{ID: "anthropic/claude-3-haiku", Name: "Claude Haiku"},
				}, nil
			},
		}, nil
	})
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatAppWithConfig(t, sess, cfgStore)
	tm.WaitFor("#general")

	tm.Submit("/config api-key test-key")
	tm.WaitFor("OpenRouter API key saved and activated.")

	tm.Type("/invite anth")
	tm.WaitFor("anthropic/claude-3-haiku")
}

func TestChatScreen_config_set_poke_interval(t *testing.T) {
	cfgStore := newFakeConfigStore()
	sess := newTestSessionWithConfigStore(t, cfgStore)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatAppWithConfig(t, sess, cfgStore)
	tm.WaitFor("#general")

	tm.Submit("/config poke-interval 10m")
	tm.WaitFor("Poke interval set to 10m0s.")

	require.Equal(t, 10*time.Minute, cfgStore.cfg.PokeInterval)
}

func TestChatScreen_config_set_timestamp_format(t *testing.T) {
	cfgStore := newFakeConfigStore()
	sess := newTestSessionWithConfigStore(t, cfgStore)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatAppWithConfig(t, sess, cfgStore)
	tm.WaitFor("#general")

	tm.Submit("/config timestamp-format 02/01 15:04:05")
	tm.WaitFor("timestamp format set to 02/01 15:04:05")

	require.NotNil(t, cfgStore.cfg.TimestampFormat)
	require.Equal(t, "02/01 15:04:05", *cfgStore.cfg.TimestampFormat)
}

func TestChatScreen_config_disable_timestamp_format(t *testing.T) {
	cfgStore := newFakeConfigStore()
	sess := newTestSessionWithConfigStore(t, cfgStore)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatAppWithConfig(t, sess, cfgStore)
	tm.WaitFor("#general")

	tm.Submit("/config timestamp-format")
	tm.WaitFor("timestamps disabled")

	require.NotNil(t, cfgStore.cfg.TimestampFormat)
	require.Equal(t, "", *cfgStore.cfg.TimestampFormat)
}

func TestChatScreen_config_disable_timestamp_format_with_empty_quotes(t *testing.T) {
	cfgStore := newFakeConfigStore()
	sess := newTestSessionWithConfigStore(t, cfgStore)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatAppWithConfig(t, sess, cfgStore)
	tm.WaitFor("#general")

	tm.Submit(`/config timestamp-format ""`)
	tm.WaitFor("timestamps disabled")

	require.NotNil(t, cfgStore.cfg.TimestampFormat)
	require.Equal(t, "", *cfgStore.cfg.TimestampFormat)
}

func TestChatScreen_config_reset_poke_interval_from_parent_flag(t *testing.T) {
	cfgStore := newFakeConfigStore()
	cfgStore.cfg.PokeInterval = 10 * time.Minute
	sess := newTestSessionWithConfigStore(t, cfgStore)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatAppWithConfig(t, sess, cfgStore)
	tm.WaitFor("#general")

	tm.Submit("/config --reset poke-interval")
	tm.WaitFor("Poke interval reset to 5m0s.")

	require.Equal(t, config.DefaultPokeInterval, cfgStore.cfg.PokeInterval)
}

func TestChatScreen_config_reset_api_key_from_child_flag(t *testing.T) {
	cfgStore := newFakeConfigStore()
	cfgStore.cfg.APIKey = "test-key"
	sess := newTestSessionWithConfigStore(t, cfgStore)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatAppWithConfig(t, sess, cfgStore)
	tm.WaitFor("#general")

	tm.Submit("/config api-key --reset")
	tm.WaitFor("OpenRouter API key cleared.")

	require.Empty(t, cfgStore.cfg.APIKey)
}

func TestChatScreen_config_reset_timestamp_format(t *testing.T) {
	custom := "%c"
	cfgStore := newFakeConfigStore()
	cfgStore.cfg.TimestampFormat = &custom
	sess := newTestSessionWithConfigStore(t, cfgStore)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatAppWithConfig(t, sess, cfgStore)
	tm.WaitFor("#general")

	tm.Submit("/config --reset timestamp-format")
	tm.WaitFor("Timestamp format reset to locale default.")

	require.Nil(t, cfgStore.cfg.TimestampFormat)
}

func TestChatScreen_config_invalid_subcommand(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/config nonsense")
	tm.WaitFor("unknown subcommand")
}

func TestChatScreen_config_invalid_duration(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/config poke-interval nope")
	tm.WaitFor("invalid duration")
}

func TestChatScreen_msg_command_opens_dm(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	require.NoError(t, sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", ""))

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Submit("/msg fakenick")
	tm.WaitFor("Opened direct message with fakenick")
}

func TestChatScreen_msg_command_opens_dm_and_sends_message(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	require.NoError(t, sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", ""))

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Submit("/msg fakenick hello there")
	tm.WaitFor("hello there", "fakenick")
}

func TestChatScreen_msg_command_unknown_nick(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/msg nobody hello")
	tm.WaitFor("no such nick: nobody")
}

func TestChatScreen_help_command(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/help")
	tm.WaitFor("/join", "/help")
}

func TestChatScreen_invalid_command(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/nick")
	tm.WaitFor("missing required argument <new-nick>")
}

func TestChatScreen_unknown_command_shows_error(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/unknown")
	tm.WaitFor("unknown command: /unknown")
}

func TestChatScreen_part_command_with_message(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	tm := newChatApp(t, sess)
	tm.WaitFor("#random")

	tm.Submit("/part see you later")
}

func TestChatScreen_quit_command_with_message(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Submit("/quit goodbye world")
	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))
}

func TestChatScreen_View_responsive(t *testing.T) {
	newChatAppInChannel(t, "#general")
}

func TestChatScreen_KeyBindings_collect_active_bindings(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	view := tm.FinalView()
	require.Contains(t, view, "↵ send")
	require.Contains(t, view, "^N nicks")
	require.Contains(t, view, "^C quit")
}

func TestChatScreen_KeyBindings_switch_to_popover_bindings(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Type("/")

	// The popover adds Tab, up/down, Esc bindings. At 80 columns the
	// status bar falls back to key-only mode, so check for keys.
	tm.WaitFor("Tab", "Esc")
}

func TestChatScreen_WelcomeState_responsive(t *testing.T) {
	sess := newTestSession(t)

	tm := newChatApp(t, sess)
	tm.WaitFor("Welcome to modeloff", "/join #general")
}

func TestChatScreen_personas_command(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	require.NoError(t, sess.SetPersona(t.Context(), "pirate", "A salty sea dog"))
	require.NoError(t, sess.SetPersona(t.Context(), "wizard", "A wise old mage"))

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Submit("/personas")
	tm.WaitFor("pirate (user): A salty sea dog", "wizard (user): A wise old mage")
}

func TestChatScreen_personas_command_empty(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/personas")
	tm.WaitFor("No personas defined.")
}

func TestChatScreen_regenerate_personas_command(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatAppWithConfig(t, sess, newFakeConfigStore())
	tm.WaitFor("#general")

	tm.Submit("/regenerate-personas")
	tm.WaitFor("Generated")
}

func TestChatScreen_config_persona_command(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/config persona bard A travelling storyteller")
	tm.WaitFor("Persona bard saved.")
}

func TestChatScreen_config_persona_no_args(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/config persona")
	tm.WaitFor("usage: /config persona <id> <description...>")
}

func TestChatScreen_config_persona_no_description(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/config persona bard")
	tm.WaitFor("usage: /config persona <id> <description...>")
}

func TestChatScreen_config_persona_reset(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	require.NoError(t, sess.SetPersona(t.Context(), "pirate", "A salty sea dog"))
	require.NoError(t, sess.SetPersona(t.Context(), "wizard", "A wise old mage"))

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Submit("/config --reset persona")
	tm.WaitFor("Removed 2 user-defined persona(s).")
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

func (f *fakeConfigStore) Load(context.Context) (config.Config, error) {
	return f.cfg, nil
}

func (f *fakeConfigStore) OnChange(config.ChangeFunc) config.UnsubscribeFunc { return func() {} }

func (f *fakeConfigStore) Save(_ context.Context, cfg config.Config) error {
	if f.saveErr != nil {
		return f.saveErr
	}

	f.cfg = cfg
	return nil
}
