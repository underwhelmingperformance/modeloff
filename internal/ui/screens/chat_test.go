package screens_test

import (
	"context"
	"fmt"
	"strings"
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
	"github.com/laney/modeloff/internal/ui/chatcmd"
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
	cfg, _ := cfgStore.Load(t.Context())
	return session.New(s, nil, &uitest.FakeAPI{}, "testuser", cfg.APIKey, cfg.SmallModel)
}

func newChatApp(t *testing.T, sess *session.Session) *uitest.App {
	t.Helper()

	return newChatAppWithConfig(t, sess, newFakeConfigStore())
}

func newChatAppWithConfig(t *testing.T, sess *session.Session, cfgStore config.Store) *uitest.App {
	t.Helper()

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, domain.KindStatus)
	require.NoError(t, err)

	root := uipkg.NewRoot(chatScreen)
	return uitest.New(t, root, teatest.WithInitialTermSize(256, 256))
}

func newChatAppInChannel(t *testing.T, channel domain.ChannelName) (*uitest.App, *session.Session) {
	t.Helper()

	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, string(channel))

	tm := newChatApp(t, sess)
	// Wait for the startup focus-restore (Init asynchronously focuses
	// the last channel) to settle: handleChannelFocus renders the
	// "Created channel" banner and clears the welcome placeholder.
	// Tests that subsequently capture FinalView need this so the final
	// rendered frame is a fully-initialised chat screen with the
	// status bar — not a transient welcome-state frame.
	tm.WaitFor(fmt.Sprintf("Created channel %s", channel))

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
		"✗", "API key not configured",
		"/config api-key <value>",
		"✗", "No channels joined",
		"/join #general",
		"Set an API key first",
		"^D, ^U, ^O",
		"No channels",
		">",
	)
}

func TestChatScreen_checklist_api_key_set_no_channels(t *testing.T) {
	cfgStore := newFakeConfigStore()
	sess := newTestSessionWithConfigStore(t, cfgStore)
	sess.SetAPIFactory(func(string, string) (api.Client, error) {
		return &uitest.FakeAPI{}, nil
	})

	tm := newChatAppWithConfig(t, sess, cfgStore)
	tm.WaitFor("API key not configured")

	tm.Submit("/config api-key test-key")
	tm.WaitFor("Models available")
}

func TestChatScreen_checklist_channels_exist_no_checklist(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	// Wait for the focus-restore handshake to finish (handleChannelFocus
	// renders the "Created channel" banner and clears the checklist
	// placeholder), not just for the sidebar to show the channel.
	tm.WaitFor("Created channel #general")

	view := tm.CurrentView()
	require.NotContains(t, view, "Welcome to modeloff",
		"checklist should not appear when channels exist")
}

func TestChatScreen_checklist_part_last_channel_shows_checklist(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Submit("/part")
	tm.WaitFor("Welcome to modeloff", "✗", "No channels joined")
}

func TestChatScreen_checklist_api_key_set_updates_live(t *testing.T) {
	cfgStore := newFakeConfigStore()
	sess := newTestSessionWithConfigStore(t, cfgStore)
	sess.SetAPIFactory(func(string, string) (api.Client, error) {
		return &uitest.FakeAPI{}, nil
	})

	tm := newChatAppWithConfig(t, sess, cfgStore)
	tm.WaitFor("Set an API key first")

	tm.Submit("/config api-key test-key")
	tm.WaitFor("Models available")
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
	uitest.SeedMessage(t, sess, "#general", "general msg")
	uitest.SeedChannel(t, sess, "#existing")

	tm := newChatApp(t, sess)
	tm.WaitFor("#existing")

	tm.Submit("/join #general")
	tm.WaitFor("#general", "general msg")
}

func TestChatScreen_rejoin_hides_pre_session_history(t *testing.T) {
	ctx := t.Context()
	s := storetest.NewMemoryStore(t)

	oldTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	// Simulate post-quit state: channel exists but user is not a member.
	general := domain.NewChannelWindow("#general", oldTime)
	general.Topic = "welcome topic"
	general.TopicSetBy = "admin"
	general.TopicSetAt = oldTime
	require.NoError(t, s.SaveWindow(ctx, general))

	require.NoError(t, s.SetAutojoinChannels(ctx, []domain.ChannelName{"#general"}))
	require.NoError(t, s.SetLastChannel(ctx, "#general"))

	// Persist a message from a previous session. The user must NOT
	// see this on rejoin: the stored event log is the models' shared
	// memory of channel activity while the user was offline, not the
	// user's scrollback. Mirrors IRC's "you don't see what happened
	// before you joined" rule.
	_, err := s.AppendEvent(ctx, "#general", domain.Message{
		Target: "#general",
		From:   "oldnick",
		Body:   "previous session message",
		At:     oldTime,
	})
	require.NoError(t, err)

	// A persisted command error from a previous session likewise
	// stays out of the user's view.
	_, err = s.AppendEvent(ctx, "#general", domain.CommandError{
		Target: "#general",
		Err:    "ancient dispatch failure",
		At:     oldTime,
	})
	require.NoError(t, err)

	sess := session.New(s, nil, &uitest.FakeAPI{}, "testuser", "", "")
	require.NoError(t, sess.JoinAutojoinChannels(ctx))

	tm := newChatApp(t, sess)

	// The join protocol events emitted by JoinAutojoinChannels
	// should appear: join, ChanServ +o, and topic info.
	tm.WaitFor(
		"has joined #general",
		"ChanServ sets mode",
		"welcome topic",
	)

	// Send a new message and pin the full visible content. Anchor the
	// assertion to the snapshot returned by WaitForView (the exact
	// view that satisfied the predicate) rather than calling
	// RenderedView or FinalModel.View() afterwards: under -race the
	// model state can drift between predicate success and a
	// subsequent re-sample, so capturing atomically is what keeps the
	// test deterministic.
	tm.Submit("fresh message")
	view := tm.WaitForViewContains("<testuser> fresh message")
	body, _ := uitest.SplitBodyAndStatus(view)
	content := normaliseContent(uitest.NonEmptyColumn(uitest.VisibleColumns(body)[1]))

	// Replace the topic-separator rule row with a stable placeholder
	// so the assertion pins the column's overall shape without
	// tracking the rule's width glyph.
	shaped := replaceTopicSeparator(content)

	require.Equal(t, []string{
		"welcome topic",
		"<topic-separator>",
		"*** testuser has joined #general",
		"*** ChanServ sets mode +o testuser",
		"*** topic for #general: welcome topic (set by admin on Wed 01 Jan 2020 00:00:00 UTC)",
		"<testuser> fresh message",
		"testuser >",
	}, shaped,
		"events from before the session start must not appear in the user's scrollback (covers both 'previous session message' and 'ancient dispatch failure')")
}

// replaceTopicSeparator substitutes the horizontal-rule row that the
// topic header draws beneath itself with a stable placeholder, so
// full-slice content assertions aren't coupled to the rule's exact
// rendered width.
func replaceTopicSeparator(lines []string) []string {
	out := make([]string, len(lines))

	for i, line := range lines {
		if line != "" && strings.Trim(line, "─") == "" {
			out[i] = "<topic-separator>"
			continue
		}

		out[i] = line
	}

	return out
}

func TestChatScreen_persists_last_channel_on_focus(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	tm := newChatApp(t, sess)
	// `SeedChannel`'s last call (#random) is what the chat screen's
	// startup focus-restore lands on; wait for the resulting banner
	// before driving the channel switch.
	tm.WaitFor("Created channel #random")

	tm.Send(chatcmd.ChannelFocusMsg{Channel: "#general"})
	tm.WaitFor("Created channel #general")

	require.Eventually(t, func() bool {
		last, err := sess.LastChannel(t.Context())
		return err == nil && last == "#general"
	}, time.Second, 10*time.Millisecond,
		"chat screen should have persisted #general as last_channel after focus")
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

func TestChatScreen_nick_command_updates_input_bar(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/nick newnick")
	tm.WaitFor("testuser is now known as newnick")

	body, _ := uitest.SplitBodyAndStatus(tm.CurrentView())
	content := uitest.NonEmptyColumn(uitest.VisibleColumns(body)[1])
	require.Equal(t, "newnick >", uitest.CompactLine(content[len(content)-1]),
		"input bar should show the new nick after /nick command")
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

	require.NoError(t, sess.AddModel(t.Context(), "#general", "anthropic/claude-3-haiku", ""))

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Submit("/whois fakenick")
	tm.WaitFor("fakenick is anthropic/claude-3-haiku")
}

func TestChatScreen_whois_persists_to_status_channel(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	require.NoError(t, sess.AddModel(t.Context(), "#general", "anthropic/claude-3-haiku", ""))

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Submit("/whois fakenick")
	tm.WaitFor("fakenick is anthropic/claude-3-haiku")

	// Status channel must carry a persisted copy so the IRC-style
	// server log shows every /whois the user ran, regardless of
	// where they typed it.
	statusEvents, err := sess.EventsBefore(t.Context(), domain.StatusChannelName, nil, 100)
	require.NoError(t, err)

	type whoisKey struct {
		Target domain.ChannelName
		Nick   domain.Nick
	}

	whoisKeys := func(events []domain.StoredEvent) []whoisKey {
		out := []whoisKey{}
		for _, ev := range events {
			if w, ok := ev.Event.(domain.Whois); ok {
				out = append(out, whoisKey{Target: w.Target, Nick: w.Nick})
			}
		}

		return out
	}

	require.Equal(t, []whoisKey{
		{Target: domain.StatusChannelName, Nick: "fakenick"},
	}, whoisKeys(statusEvents),
		"&modeloff must carry exactly one Whois for fakenick")

	// Active channel display is ephemeral: the #general event log
	// should NOT carry any Whois entries.
	generalEvents, err := sess.EventsBefore(t.Context(), "#general", nil, 100)
	require.NoError(t, err)

	require.Equal(t, []whoisKey{}, whoisKeys(generalEvents),
		"whois should only land on &modeloff, never on the active channel")
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
	tm.WaitFor("End of /list")
}

func TestChatScreen_add_model_command(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/add-model anthropic/claude-3-haiku")
	tm.WaitFor("fakenick has joined #general")
}

func TestChatScreen_add_model_with_persona(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/add-model anthropic/claude-3-haiku --persona Helpful assistant")
	tm.WaitFor("fakenick has joined #general")
}

func TestChatScreen_add_model_no_args(t *testing.T) {
	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/add-model")
	tm.WaitFor("usage: /add-model <model-id> [--persona <text>]")
}

func TestChatScreen_invite_existing_instance(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	require.NoError(t, sess.AddModel(t.Context(), "#general", "anthropic/claude-3-haiku", ""))

	tm := newChatApp(t, sess)
	tm.WaitFor("#random")

	tm.Submit("/invite fakenick")
	tm.WaitFor("fakenick has joined #random")
}

func TestChatScreen_kick_command(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	require.NoError(t, sess.AddModel(t.Context(), "#general", "anthropic/claude-3-haiku", ""))

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Submit("/kick fakenick")
	tm.WaitFor("fakenick was kicked from #general by testuser")
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

	tm.Type("/add-model anth")
	tm.WaitFor("anthropic/claude-3-haiku")
}

func TestChatScreen_config_set_api_key_surfaces_live_model_failure(t *testing.T) {
	cfgStore := newFakeConfigStore()
	sess := newTestSessionWithConfigStore(t, cfgStore)
	sess.SetAPIFactory(func(string, string) (api.Client, error) {
		return &uitest.FakeAPI{
			ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
				return nil, fmt.Errorf("upstream 503")
			},
		}, nil
	})
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatAppWithConfig(t, sess, cfgStore)
	tm.WaitFor("#general")

	tm.Submit("/config api-key test-key")
	tm.WaitFor("Model list unavailable: upstream 503.")
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

	require.Equal(t, "", cfgStore.cfg.APIKey)
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

func TestChatScreen_config_reset_base_url(t *testing.T) {
	cfgStore := newFakeConfigStore()
	cfgStore.cfg.BaseURL = "https://custom.example.com/v1"
	sess := newTestSessionWithConfigStore(t, cfgStore)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatAppWithConfig(t, sess, cfgStore)
	tm.WaitFor("#general")

	tm.Submit("/config --reset base-url")
	tm.WaitFor("base URL reset to https://openrouter.ai/api/v1")

	require.Equal(t, config.DefaultBaseURL, cfgStore.cfg.BaseURL)
}

func TestChatScreen_config_reset_small_model(t *testing.T) {
	cfgStore := newFakeConfigStore()
	cfgStore.cfg.SmallModel = "custom/small-model"
	sess := newTestSessionWithConfigStore(t, cfgStore)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatAppWithConfig(t, sess, cfgStore)
	tm.WaitFor("#general")

	tm.Submit("/config --reset small-model")
	tm.WaitFor("Small model reset to " + string(config.DefaultSmallModel) + ".")

	require.Equal(t, config.DefaultSmallModel, cfgStore.cfg.SmallModel)
}

func TestChatScreen_config_reset_embedding_model(t *testing.T) {
	cfgStore := newFakeConfigStore()
	cfgStore.cfg.EmbeddingModel = "custom/embedding-model"
	sess := newTestSessionWithConfigStore(t, cfgStore)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatAppWithConfig(t, sess, cfgStore)
	tm.WaitFor("#general")

	tm.Submit("/config --reset embedding-model")
	tm.WaitFor("embedding model reset to openai/text-embedding-3-small")

	require.Equal(t, config.DefaultEmbeddingModel, cfgStore.cfg.EmbeddingModel)
}

func TestChatScreen_config_reset_highlight(t *testing.T) {
	cfgStore := newFakeConfigStore()
	cfgStore.cfg.HighlightWords = []string{"custom", "words"}
	sess := newTestSessionWithConfigStore(t, cfgStore)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatAppWithConfig(t, sess, cfgStore)
	tm.WaitFor("#general")

	tm.Submit("/config --reset highlight")
	tm.WaitFor("highlight words reset to: [$nick]")

	require.Equal(t, config.DefaultHighlightWords, cfgStore.cfg.HighlightWords)
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

// TestChatScreen_msg_command_requires_body verifies that
// `/msg <nick>` without a trailing body is rejected — `/msg` is
// a send command, and the sister `/query` is the open-and-focus
// affordance.
func TestChatScreen_msg_command_requires_body(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	require.NoError(t, sess.AddModel(t.Context(), "#general", "anthropic/claude-3-haiku", ""))

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Submit("/msg fakenick")
	tm.WaitFor("message body is required")
}

// TestChatScreen_query_command_opens_dm exercises the
// `/query <nick>` blank-window-and-focus path.
func TestChatScreen_query_command_opens_dm(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	require.NoError(t, sess.AddModel(t.Context(), "#general", "anthropic/claude-3-haiku", ""))

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Submit("/query fakenick")
	tm.WaitFor("fakenick")
}

// TestChatScreen_query_command_opens_dm_and_sends_message
// exercises `/query <nick> <body>`: window opens, focus
// switches, body sends.
func TestChatScreen_query_command_opens_dm_and_sends_message(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	require.NoError(t, sess.AddModel(t.Context(), "#general", "anthropic/claude-3-haiku", ""))

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Submit("/query fakenick hello there")
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

func TestChatScreen_clear_command_removes_messages(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedMessage(t, sess, "#general", "visible text")

	tm := newChatApp(t, sess)
	tm.WaitFor("#general", "visible text")

	tm.Submit("/clear")
	tm.WaitFor("No messages yet")

	body, _ := uitest.SplitBodyAndStatus(tm.CurrentView())
	content := uitest.NonEmptyColumn(uitest.VisibleColumns(body)[1])
	require.Equal(t, []string{"No messages yet", "testuser >"}, []string{
		content[0],
		uitest.CompactLine(content[1]),
	})
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

	_, status := uitest.SplitBodyAndStatus(tm.FinalView())

	tokens := strings.Fields(status)
	require.Subset(t, tokens, []string{"^D/^U", "^O", "↵", "^W", "^C"},
		"status bar must surface core navigation, submit and quit bindings")
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

func TestChatScreen_add_model_short_circuits_when_model_list_unavailable(t *testing.T) {
	cfgStore := newFakeConfigStore()
	cfgStore.cfg.APIKey = "test-key"

	api := &uitest.FakeAPI{
		ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
			return nil, fmt.Errorf("upstream 503")
		},
	}

	s := storetest.NewMemoryStore(t)
	sess := session.New(s, nil, api, "testuser", cfgStore.cfg.APIKey, cfgStore.cfg.SmallModel)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatAppWithConfig(t, sess, cfgStore)
	// Wait for the failed `loadLiveModels` to surface and flip
	// `listModelsState` to `failed` before issuing `/add-model`.
	tm.WaitFor("Model list unavailable: upstream 503.")

	tm.Submit("/add-model anthropic/claude-3-haiku")
	tm.WaitFor("add-model: model list unavailable")
}

func TestChatScreen_add_model_completion_hides_popover_when_model_list_unavailable(t *testing.T) {
	cfgStore := newFakeConfigStore()
	cfgStore.cfg.APIKey = "test-key"

	api := &uitest.FakeAPI{
		ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
			return nil, fmt.Errorf("upstream 503")
		},
	}

	s := storetest.NewMemoryStore(t)
	sess := session.New(s, nil, api, "testuser", cfgStore.cfg.APIKey, cfgStore.cfg.SmallModel)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatAppWithConfig(t, sess, cfgStore)
	tm.WaitFor("Model list unavailable: upstream 503.")

	tm.Type("/add-model ")
	tm.WaitFor("> /add-model ")

	_, status := uitest.SplitBodyAndStatus(tm.CurrentView())
	require.NotContains(t, status, "Tab",
		"failed live-model completion should suppress popover bindings rather than leaving a blank popover state")
	require.NotContains(t, status, "Esc")
}
