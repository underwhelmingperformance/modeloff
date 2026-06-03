package ui_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/exp/teatest"
	chromem "github.com/philippgille/chromem-go"
	"github.com/stretchr/testify/require"
	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/modelmanager"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
	"github.com/laney/modeloff/internal/testclient"
	uipkg "github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/screens"
	"github.com/laney/modeloff/internal/ui/uitest"
	"github.com/laney/modeloff/internal/userclient"
)

func TestApp_startup_without_api_key(t *testing.T) {
	root := uipkg.NewRoot(screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey: false,
		Nick:      "alice",
	}, nil))
	tm := uitest.New(t, root)

	advanceConnection(tm, 2)
	tm.WaitFor("/config", "No API key configured")
}

func TestApp_startup_with_saved_channels(t *testing.T) {
	sess, mgr, user, store, cfgStore := newIntegrationSession(t, &integrationAPI{})

	uitest.SeedChannel(t, user, "#general")
	uitest.SeedChannel(t, user, "#random")
	uitest.SeedMessage(t, sess, "#random", "hello from last time")
	uitest.Quit(t, user, "")
	uitest.DrainEvents(user)

	// The chat-screen needs a `UIStateStore` to persist its
	// `last_channel` write; pass the integration store through so
	// the final assertion on `GetLastChannel` reflects the focus
	// the screen actually settled on.
	chatScreen, err := screens.NewChatScreen(t.Context, sess, mgr, user, cfgStore, store, domain.KindStatus)
	require.NoError(t, err)

	root := uipkg.NewRoot(screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey:    true,
		ChannelCount: 2,
		Nick:         string(user.Nick()),
		Session:      sess,
		User:         user,
		BaseContext:  t.Context,
	}, chatScreen))
	tm := uitest.New(t, root)

	advanceConnection(tm, 7)
	tm.WaitFor("#general", "#random")

	// Messages from before this session must not appear in the
	// user's scrollback. The persisted event log is the models'
	// shared memory of channel activity while the user was offline,
	// not the user's view; the chat screen rebuilds its scrollback
	// purely from live events seen on this connection.
	require.NotContains(t, tm.CurrentView(), "hello from last time")

	last, err := store.GetLastChannel(t.Context())
	require.NoError(t, err)
	require.Equal(t, domain.ChannelName("#random"), last)
}

func TestApp_add_model_and_receive_reply(t *testing.T) {
	apiClient := &integrationAPI{
		generateNickFn: func(context.Context, domain.ModelID, string, []domain.Nick) (domain.Nick, error) {
			return "botty", nil
		},
		sendEventsFn: func(
			_ context.Context,
			_ domain.ModelID,
			_ string,
			_ []protocol.IRCMessage,
			events []protocol.IRCMessage,
		) (api.CompletionResult, error) {
			return msgToolCall(t, events[0].Target, "hello back"), nil
		},
	}
	sess, mgr, user, _, cfgStore := newIntegrationSession(t, apiClient)
	uitest.SeedChannel(t, user, "#general")

	chatScreen, err := screens.NewChatScreen(t.Context, sess, mgr, user, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	tm.WaitFor("#general")

	tm.Submit("/add-model test/model")
	tm.WaitFor("botty has joined #general")

	tm.Submit("hello world")
	tm.WaitFor("hello world", "hello back")
}

func TestApp_open_dm_and_send_message(t *testing.T) {
	apiClient := &integrationAPI{}
	sess, mgr, user, store, cfgStore := newIntegrationSession(t, apiClient)
	uitest.SeedChannel(t, user, "#general")
	seedInstance(t, sess, store, instanceSpec{
		Nick:    "botty",
		ModelID: "test/model",
	})

	chatScreen, err := screens.NewChatScreen(t.Context, sess, mgr, user, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	tm.WaitFor("#general")

	// `/query` opens the DM window and switches focus, so the
	// outgoing message body lands in the visible scrollback.
	tm.Submit("/query botty hello there")
	tm.WaitFor("hello there", "botty")
}

func TestApp_terminal_output_shows_full_model_nick_in_user_list(t *testing.T) {
	t.Skip("Pending MessageList redesign: the chat-screen's `restoreFocus`" +
		" `ChannelFocusMsg` races with the protocol-bus drain. Layout" +
		" widths get computed against the transient pre-focus state and" +
		" the nick column ends up too narrow to fit `+grok420_bot`. The" +
		" fix is to remove `HistoryLoadedMsg`/`loadHistory` and have" +
		" MessageList read scrollback through a getter, eliminating the" +
		" focus/event race entirely.")

	sess, mgr, user, store, cfgStore := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, user, "#general")

	channels := orderedmap.New[domain.ChannelName, time.Time]()
	channels.Set("#general", time.Now())

	grok := seedInstance(t, sess, store, instanceSpec{
		Nick:     "grok420_bot",
		ModelID:  "test/model",
		Channels: channels,
	})

	client := testclient.New(grok.Nick(), sess,
		testclient.WithInstanceID(grok.ID()),
		testclient.WithModelID(grok.ModelID),
		testclient.WithChannels("#general"),
	)
	require.NoError(t, client.Attach(t.Context()), "attach grok test client")
	t.Cleanup(client.Detach)

	resp, err := client.Send(t.Context(), protocol.Join{Channel: "#general"})
	require.NoError(t, err)
	require.NoError(t, resp.Err)

	chatScreen, err := screens.NewChatScreen(t.Context, sess, mgr, user, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen),
		teatest.WithInitialTermSize(365, 90))
	tm.WaitFor("#general", "grok420_bot")

	require.Equal(t, []string{"Nicks", "@testuser", "+grok420_bot"}, uitest.NonEmptyColumn(bodyColumns(tm.CurrentView())[2]))
}

func TestApp_periodic_poke_generates_message(t *testing.T) {
	apiClient := &integrationAPI{
		sendEventsFn: func(
			_ context.Context,
			_ domain.ModelID,
			_ string,
			_ []protocol.IRCMessage,
			events []protocol.IRCMessage,
		) (api.CompletionResult, error) {
			if len(events) == 1 && events[0].Kind == protocol.KindPoke {
				return msgToolCall(t, events[0].Target, "still alive"), nil
			}

			return api.CompletionResult{}, nil
		},
	}
	sess, mgr, user, _, cfgStore := newIntegrationSession(t, apiClient)
	uitest.SeedChannel(t, user, "#general")

	uitest.AddModel(t, user, "#general", "test/model", "")
	uitest.DrainEvents(user)

	chatScreen, err := screens.NewChatScreen(t.Context, sess, mgr, user, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	tm.WaitFor("#general")

	tm.Send(chatcmd.PokeRequested{})
	tm.WaitFor("still alive")
}

func TestApp_reuse_existing_instance(t *testing.T) {
	apiClient := &integrationAPI{
		// The model accepts the INVITE by issuing a `join` tool call;
		// the tool loop dispatches `protocol.Join` against the
		// model's client and the bot lands in the target channel.
		sendEventsFn: func(_ context.Context, _ domain.ModelID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
			for _, ev := range events {
				if ev.Kind == protocol.KindInvite {
					return api.CompletionResult{
						PendingToolCalls: []api.PendingToolCall{{
							ID:   "tc-join",
							Name: "join",
							Args: json.RawMessage(fmt.Sprintf(`{"channel":%q}`, ev.Target)),
						}},
					}, nil
				}
			}

			return api.CompletionResult{}, nil
		},
	}
	_, _, _, store, _ := newIntegrationSession(t, apiClient)
	memStore := memory.NewStoreAdapter(storetest.NewMemoryStore(t))
	require.NoError(t, memStore.Write(t.Context(), "botty", memory.Entry{
		Key:     "topic",
		Content: "favourite channel regular",
	}))

	cfgStore := &integrationConfigStore{}
	tools, err := chatcmd.BuildToolRegistry()
	require.NoError(t, err)

	sess, mgr, user := uitest.NewTestSession(t, store, apiClient, memStore, tools, cfgStore.cfg.APIKey, cfgStore.cfg.SmallModel, t.Context)

	uitest.SeedChannel(t, user, "#general")
	uitest.SeedChannel(t, user, "#random")

	uitest.AddModel(t, user, "#general", "test/model", "Helpful assistant")
	uitest.DrainEvents(user)

	chatScreen, err := screens.NewChatScreen(t.Context, sess, mgr, user, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	tm.WaitFor("#random")

	tm.Submit("/invite botty")
	tm.WaitFor("botty has joined #random")

	tm.Submit("/whois botty")
	tm.WaitFor("persona: Helpful assistant", "channels: #general, #random")
}

func TestApp_vector_memory_write_and_search(t *testing.T) {
	// Track which turn of ContinueWithToolResults we're on so we can
	// simulate: write_memory → search_memory → reply.
	var toolTurn int

	apiClient := &integrationAPI{
		generateNickFn: func(context.Context, domain.ModelID, string, []domain.Nick) (domain.Nick, error) {
			return "membot", nil
		},

		// First API call: model requests write_memory.
		sendEventsFn: func(
			_ context.Context,
			_ domain.ModelID,
			_ string,
			_ []protocol.IRCMessage,
			events []protocol.IRCMessage,
		) (api.CompletionResult, error) {
			// Only trigger the tool loop on a user message, not on
			// join events.
			for _, ev := range events {
				if ev.Kind == protocol.KindPrivMsg {
					return api.CompletionResult{
						PendingToolCalls: []api.PendingToolCall{{
							ID:   "tc-write",
							Name: "write_memory",
							Args: json.RawMessage(`{"key":"favourite-colour","content":"the user likes blue"}`),
						}},
					}, nil
				}
			}

			return api.CompletionResult{}, nil
		},

		// Tool loop turns:
		//   turn 0 (after write_memory): model requests search_memory
		//   turn 1 (after search_memory): model replies with what it found
		continueWithToolResultsFn: func(
			_ context.Context,
			_ *api.Conversation,
			results []api.ToolResult,
		) (api.CompletionResult, error) {
			turn := toolTurn
			toolTurn++

			switch turn {
			case 0:
				// write_memory succeeded — now search.
				return api.CompletionResult{
					PendingToolCalls: []api.PendingToolCall{{
						ID:   "tc-search",
						Name: "search_memory",
						Args: json.RawMessage(`{"query":"colour","limit":5}`),
					}},
				}, nil

			default:
				// search_memory returned results — reply with the
				// remembered content.
				var payload modelclient.ToolResultPayload
				if err := json.Unmarshal([]byte(results[0].Content), &payload); err != nil {
					return api.CompletionResult{}, err
				}

				data, err := json.Marshal(payload.Data)
				if err != nil {
					return api.CompletionResult{}, err
				}

				var matches []memory.SearchResult
				if err := json.Unmarshal(data, &matches); err != nil {
					return api.CompletionResult{}, err
				}

				body := "I found nothing"
				if len(matches) > 0 {
					body = "I found: " + matches[0].Entry.Content
				}

				return msgToolCall(t, "#lab", body), nil
			}
		},
	}

	// Use an IndexedStore backed by an in-memory chromem DB so vector
	// search works without a real embedding endpoint.
	backing := memory.NewStoreAdapter(storetest.NewMemoryStore(t))
	db := chromem.NewDB()
	embedder := func(_ context.Context, _ string) ([]float32, error) {
		return []float32{1.0, 0.0, 0.0}, nil
	}
	memStore := memory.NewIndexedStoreFromDB(backing, db, embedder)

	store := storetest.NewMemoryStore(t)
	cfgStore := &integrationConfigStore{
		cfg: config.Config{
			UserNick:     "testuser",
			PokeInterval: 5 * time.Minute,
		},
	}
	tools, err := chatcmd.BuildToolRegistry()
	require.NoError(t, err)

	sess, mgr, user := uitest.NewTestSession(t, store, apiClient, memStore, tools, cfgStore.cfg.APIKey, cfgStore.cfg.SmallModel, t.Context)

	uitest.SeedChannel(t, user, "#lab")

	chatScreen, err := screens.NewChatScreen(t.Context, sess, mgr, user, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen),
		teatest.WithInitialTermSize(200, 30))
	tm.WaitFor("#lab")

	tm.Submit("/add-model test/model")
	tm.WaitFor("membot has joined #lab")

	tm.Submit("what is my favourite colour?")
	tm.WaitFor("I found:", "the user likes blue")
}

type integrationAPI struct {
	mu                        sync.Mutex
	sendEventsFn              func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error)
	continueWithToolResultsFn func(context.Context, *api.Conversation, []api.ToolResult) (api.CompletionResult, error)
	generateNickFn            func(context.Context, domain.ModelID, string, []domain.Nick) (domain.Nick, error)
}

func (f *integrationAPI) ListModels(context.Context) ([]api.ModelInfo, error) {
	return nil, nil
}

func (f *integrationAPI) SendEvents(
	ctx context.Context,
	modelID domain.ModelID,
	_ domain.InstanceID,
	system string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
	_ ...api.ToolDefinition,
) (api.CompletionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.sendEventsFn != nil {
		return f.sendEventsFn(ctx, modelID, system, history, events)
	}

	return api.CompletionResult{}, nil
}

func (f *integrationAPI) ContinueWithToolResults(
	ctx context.Context,
	conv *api.Conversation,
	results []api.ToolResult,
	_ ...api.ToolDefinition,
) (api.CompletionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.continueWithToolResultsFn != nil {
		return f.continueWithToolResultsFn(ctx, conv, results)
	}

	return api.CompletionResult{}, nil
}

func (f *integrationAPI) GenerateNick(ctx context.Context, smallModel domain.ModelID, persona string, exclude []domain.Nick) (api.NicknameResult, error) {
	if f.generateNickFn != nil {
		nick, err := f.generateNickFn(ctx, smallModel, persona, exclude)
		return api.NicknameResult{Nick: nick}, err
	}

	return api.NicknameResult{Nick: "botty"}, nil
}

func (f *integrationAPI) GeneratePersonas(context.Context, domain.ModelID) ([]domain.Persona, error) {
	return nil, nil
}

type integrationConfigStore struct {
	cfg     config.Config
	saveErr error
}

func (s *integrationConfigStore) Load(context.Context) (config.Config, error) {
	return s.cfg, nil
}

func (s *integrationConfigStore) Save(_ context.Context, cfg config.Config) error {
	if s.saveErr != nil {
		return s.saveErr
	}

	s.cfg = cfg
	return nil
}

func (s *integrationConfigStore) OnChange(_ config.ChangeFunc) config.UnsubscribeFunc {
	return func() {}
}

func newIntegrationSession(t *testing.T, apiClient api.Client) (*session.Session, *modelmanager.Manager, *userclient.UserClient, *storemod.SQLiteStore, *integrationConfigStore) {
	t.Helper()

	cfgStore := &integrationConfigStore{
		cfg: config.Config{
			UserNick:     "testuser",
			PokeInterval: 5 * time.Minute,
		},
	}

	sess, mgr, user, store := newIntegrationSessionWithConfigStore(t, apiClient, cfgStore)
	return sess, mgr, user, store, cfgStore
}

func newIntegrationSessionWithConfigStore(
	t *testing.T,
	apiClient api.Client,
	cfgStore *integrationConfigStore,
) (*session.Session, *modelmanager.Manager, *userclient.UserClient, *storemod.SQLiteStore) {
	t.Helper()

	store := storetest.NewMemoryStore(t)
	memStore := memory.NewStoreAdapter(storetest.NewMemoryStore(t))
	tools, err := chatcmd.BuildToolRegistry()
	require.NoError(t, err)

	sess, mgr, user := uitest.NewTestSession(t, store, apiClient, memStore, tools, cfgStore.cfg.APIKey, cfgStore.cfg.SmallModel, t.Context)

	return sess, mgr, user, store
}

func advanceConnection(tm *uitest.App, ticks int) {
	for range ticks {
		tm.Send(screens.ConnectionTickMsg{})
	}
}

type instanceSpec struct {
	Nick     domain.Nick
	ModelID  domain.ModelID
	Persona  string
	Channels *orderedmap.OrderedMap[domain.ChannelName, time.Time]
}

func seedInstance(t *testing.T, sess *session.Session, _ *storemod.SQLiteStore, spec instanceSpec) *domain.Instance {
	t.Helper()

	opts := []testclient.Option{
		testclient.WithInstanceID(domain.InstanceID("inst-" + string(spec.Nick))),
		testclient.WithModelID(spec.ModelID),
		testclient.WithPersona(spec.Persona),
	}

	if spec.Channels != nil {
		channels := make([]domain.ChannelName, 0, spec.Channels.Len())
		for pair := spec.Channels.Oldest(); pair != nil; pair = pair.Next() {
			channels = append(channels, pair.Key)
		}
		opts = append(opts, testclient.WithChannels(channels...))
	}

	client := testclient.New(spec.Nick, sess, opts...)
	require.NoError(t, client.Attach(t.Context()), "attach test client for seeded instance")
	t.Cleanup(client.Detach)

	return client.Instance()
}

// msgToolCall builds an [api.CompletionResult] whose PendingToolCalls
// invoke the `msg` tool once with the given target and body — the
// wire-shape the model emits when it wants to say something. The
// `body` field on MsgCommand is a `[]string`, so the JSON shape is
// an array of words.
func msgToolCall(t *testing.T, target, body string) api.CompletionResult {
	t.Helper()

	args, err := json.Marshal(map[string]any{
		"target": target,
		"body":   []string{body},
	})
	require.NoError(t, err)

	return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{
		{ID: "call_msg_0", Name: "msg", Args: args},
	}}
}
