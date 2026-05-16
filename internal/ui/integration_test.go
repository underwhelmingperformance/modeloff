package ui_test

import (
	"context"
	"encoding/json"
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
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
	uipkg "github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/screens"
	"github.com/laney/modeloff/internal/ui/uitest"
)

func TestApp_startup_without_api_key(t *testing.T) {
	root := uipkg.NewRoot(screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey: false,
		Nick:      "alice",
	}))
	tm := uitest.New(t, root)

	advanceConnection(tm, 2)
	tm.WaitFor("/config", "No API key configured")
}

func TestApp_startup_with_saved_channels(t *testing.T) {
	sess, store, cfgStore := newIntegrationSession(t, &integrationAPI{})

	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")
	uitest.SeedMessage(t, sess, "#random", "hello from last time")
	require.NoError(t, sess.Quit(t.Context(), ""))
	uitest.DrainEvents(sess)

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, domain.KindStatus)
	require.NoError(t, err)

	root := uipkg.NewRoot(screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey:    true,
		ChannelCount: 2,
		Nick:         string(sess.UserNick()),
		Next:         chatScreen,
		Session:      sess,
		Ctx:          t.Context(),
	}))
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
			context.Context,
			domain.ModelID,
			string,
			[]protocol.IRCMessage,
			[]protocol.IRCMessage,
		) (protocol.ModelResponse, error) {
			return protocol.ModelResponse{
				Kind:     protocol.ResponseReply,
				Messages: []protocol.ReplyPart{{Kind: protocol.ReplyMessage, Body: "hello back"}},
			}, nil
		},
	}
	sess, _, cfgStore := newIntegrationSession(t, apiClient)
	uitest.SeedChannel(t, sess, "#general")

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, domain.KindStatus)
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
	sess, store, cfgStore := newIntegrationSession(t, apiClient)
	uitest.SeedChannel(t, sess, "#general")
	seedInstance(t, sess, store, instanceSpec{
		Nick:    "botty",
		ModelID: "test/model",
	})

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	tm.WaitFor("#general")

	// `/query` opens the DM window and switches focus, so the
	// outgoing message body lands in the visible scrollback.
	tm.Submit("/query botty hello there")
	tm.WaitFor("hello there", "botty")
}

func TestApp_terminal_output_shows_full_model_nick_in_user_list(t *testing.T) {
	sess, store, cfgStore := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")

	channels := orderedmap.New[domain.ChannelName, time.Time]()
	channels.Set("#general", time.Now())

	grok := seedInstance(t, sess, store, instanceSpec{
		Nick:     "grok420_bot",
		ModelID:  "test/model",
		Channels: channels,
	})

	client := sess.Model(t.Context(), protocol.ClientID(grok.ID()))
	require.NotNil(t, client, "model client for seeded grok must exist")
	resp, err := client.Send(t.Context(), protocol.Join{Channel: "#general"})
	require.NoError(t, err)
	require.NoError(t, resp.Err)

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, domain.KindStatus)
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
		) (protocol.ModelResponse, error) {
			if len(events) == 1 && events[0].Kind == protocol.KindPoke {
				return protocol.ModelResponse{
					Kind:     protocol.ResponseReply,
					Messages: []protocol.ReplyPart{{Kind: protocol.ReplyMessage, Body: "still alive"}},
				}, nil
			}

			return protocol.ModelResponse{Kind: protocol.ResponseSilence}, nil
		},
	}
	sess, _, cfgStore := newIntegrationSession(t, apiClient)
	uitest.SeedChannel(t, sess, "#general")

	require.NoError(t, sess.AddModel(t.Context(), "#general", "test/model", ""))
	uitest.DrainEvents(sess)

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen))
	tm.WaitFor("#general")

	tm.Send(screens.PokeTickMsg{})
	tm.WaitFor("still alive")
}

func TestApp_reuse_existing_instance(t *testing.T) {
	apiClient := &integrationAPI{}
	_, store, _ := newIntegrationSession(t, apiClient)
	memStore := memory.NewStoreAdapter(storetest.NewMemoryStore(t))
	require.NoError(t, memStore.Write(t.Context(), "botty", memory.Entry{
		Key:     "topic",
		Content: "favourite channel regular",
	}))

	cfgStore := &integrationConfigStore{}
	sess := session.New(t.Context, store, memStore, apiClient, "testuser", cfgStore.cfg.APIKey, cfgStore.cfg.SmallModel)
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	require.NoError(t, sess.AddModel(t.Context(), "#general", "test/model", "Helpful assistant"))
	uitest.DrainEvents(sess)

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, domain.KindStatus)
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
		sendEventsFullFn: func(
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

			return api.CompletionResult{
				Response: protocol.ModelResponse{Kind: protocol.ResponseSilence},
			}, nil
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
				var payload session.ToolResultPayload
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

				return api.CompletionResult{
					Response: protocol.ModelResponse{
						Kind:     protocol.ResponseReply,
						Messages: []protocol.ReplyPart{{Kind: protocol.ReplyMessage, Body: body}},
					},
				}, nil
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
	sess := session.New(t.Context, store, memStore, apiClient, "testuser", cfgStore.cfg.APIKey, cfgStore.cfg.SmallModel)
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

	uitest.SeedChannel(t, sess, "#lab")

	chatScreen, err := screens.NewChatScreen(t.Context(), sess, cfgStore, domain.KindStatus)
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
	sendEventsFn              func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error)
	sendEventsFullFn          func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error)
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

	if f.sendEventsFullFn != nil {
		return f.sendEventsFullFn(ctx, modelID, system, history, events)
	}

	if f.sendEventsFn != nil {
		response, err := f.sendEventsFn(ctx, modelID, system, history, events)
		return api.CompletionResult{Response: response}, err
	}

	return api.CompletionResult{
		Response: protocol.ModelResponse{Kind: protocol.ResponseSilence},
	}, nil
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

	return api.CompletionResult{
		Response: protocol.ModelResponse{Kind: protocol.ResponseSilence},
	}, nil
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

func newIntegrationSession(t *testing.T, apiClient api.Client) (*session.Session, *storemod.SQLiteStore, *integrationConfigStore) {
	t.Helper()

	cfgStore := &integrationConfigStore{
		cfg: config.Config{
			UserNick:     "testuser",
			PokeInterval: 5 * time.Minute,
		},
	}

	sess, store := newIntegrationSessionWithConfigStore(t, apiClient, cfgStore)
	return sess, store, cfgStore
}

func newIntegrationSessionWithConfigStore(
	t *testing.T,
	apiClient api.Client,
	cfgStore *integrationConfigStore,
) (*session.Session, *storemod.SQLiteStore) {
	t.Helper()

	store := storetest.NewMemoryStore(t)
	memStore := memory.NewStoreAdapter(storetest.NewMemoryStore(t))
	sess := session.New(t.Context, store, memStore, apiClient, "testuser", cfgStore.cfg.APIKey, cfgStore.cfg.SmallModel)
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

	return sess, store
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

func seedInstance(t *testing.T, sess *session.Session, store *storemod.SQLiteStore, spec instanceSpec) *domain.Instance {
	t.Helper()

	inst := domain.NewModelInstance(
		domain.InstanceID("inst-"+string(spec.Nick)),
		spec.Nick,
		spec.ModelID,
		spec.Persona,
		spec.Channels,
	)
	require.NoError(t, store.SaveInstance(t.Context(), inst))

	// Pair the persistent write with a `Session.Model` lookup so
	// the model-client subscription is registered and its dispatch
	// goroutine running before any test fans out an event to it.
	// This mirrors the production lifecycle, where
	// `attachInstanceToChannel` ensures the same invariant.
	sess.Model(t.Context(), protocol.ClientID(inst.ID()))

	return inst
}
