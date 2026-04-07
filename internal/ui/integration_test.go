package ui_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/exp/teatest"
	chromem "github.com/philippgille/chromem-go"
	"github.com/stretchr/testify/require"

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
	sess, store := newIntegrationSession(t, &integrationAPI{})

	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")
	uitest.SeedMessage(t, sess, "#random", "hello from last time")

	root := uipkg.NewRoot(screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey:    true,
		ChannelCount: 2,
		Nick:         string(sess.UserNick()),
		Next:         screens.NewChatScreen(t.Context(), sess),
	}))
	tm := uitest.New(t, root)

	advanceConnection(tm, 4)
	tm.WaitFor("#general", "#random", "hello from", "last time")

	last, err := store.GetLastChannel(t.Context())
	require.NoError(t, err)
	require.Equal(t, domain.ChannelName("#random"), last)
}

func TestApp_invite_and_receive_reply(t *testing.T) {
	apiClient := &integrationAPI{
		generateNickFn: func(context.Context, domain.ModelID, domain.ModelID) (domain.Nick, error) {
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
	sess, _ := newIntegrationSession(t, apiClient)
	uitest.SeedChannel(t, sess, "#general")

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	tm.WaitFor("#general")

	tm.Submit("/invite test/model")
	tm.WaitFor("botty (test/model) has joined #general")

	tm.Submit("hello world")
	tm.WaitFor("hello world", "hello back")
}

func TestApp_open_dm_and_send_message(t *testing.T) {
	apiClient := &integrationAPI{}
	sess, store := newIntegrationSession(t, apiClient)
	uitest.SeedChannel(t, sess, "#general")
	seedInstance(t, store, domain.Instance{
		Nick:    "botty",
		ModelID: "test/model",
	})

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	tm.WaitFor("#general")

	tm.Submit("/msg botty hello there")
	tm.WaitFor("Opened direct message with botty", "hello there")
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
	sess, _ := newIntegrationSession(t, apiClient)
	uitest.SeedChannel(t, sess, "#general")

	require.NoError(t, sess.Invite(t.Context(), "#general", "test/model", ""))
	uitest.DrainEvents(sess)

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	tm.WaitFor("#general")

	tm.Send(screens.PokeTickMsg{})
	tm.WaitFor("still alive")
}

func TestApp_reuse_existing_instance(t *testing.T) {
	apiClient := &integrationAPI{}
	_, store := newIntegrationSession(t, apiClient)
	memStore := memory.NewStoreAdapter(storetest.NewMemoryStore(t))
	require.NoError(t, memStore.Write(t.Context(), "botty", memory.Entry{
		Key:     "topic",
		Content: "favourite channel regular",
	}))

	sess := session.New(store, memStore, apiClient, &integrationConfigStore{}, "testuser")

	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	require.NoError(t, sess.Invite(t.Context(), "#general", "test/model", "Helpful assistant"))
	uitest.DrainEvents(sess)

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	tm.WaitFor("#random")

	tm.Submit("/invite botty")
	tm.WaitFor("botty (test/model) has joined #random")

	tm.Submit("/whois botty")
	tm.WaitFor("persona: Helpful assistant", "channels: #general, #random")
}

func TestApp_vector_memory_write_and_search(t *testing.T) {
	// Track which turn of ContinueWithToolResults we're on so we can
	// simulate: write_memory → search_memory → reply.
	var toolTurn int

	apiClient := &integrationAPI{
		generateNickFn: func(context.Context, domain.ModelID, domain.ModelID) (domain.Nick, error) {
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
							Kind: api.ToolCallWriteMemory,
							Key:  "favourite-colour",
							Body: "the user likes blue",
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
						ID:    "tc-search",
						Kind:  api.ToolCallSearchMemory,
						Body:  "colour",
						Limit: 5,
					}},
				}, nil

			default:
				// search_memory returned results — reply with them.
				return api.CompletionResult{
					Response: protocol.ModelResponse{
						Kind:     protocol.ResponseReply,
						Messages: []protocol.ReplyPart{{Kind: protocol.ReplyMessage, Body: "I found: " + results[0].Content}},
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
	sess := session.New(store, memStore, apiClient, cfgStore, "testuser")

	uitest.SeedChannel(t, sess, "#lab")

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)),
		teatest.WithInitialTermSize(200, 30))
	tm.WaitFor("#lab")

	tm.Submit("/invite test/model")
	tm.WaitFor("membot (test/model) has joined #lab")

	tm.Submit("what is my favourite colour?")
	tm.WaitFor("I found:", "the user likes blue")
}

type integrationAPI struct {
	mu                        sync.Mutex
	sendEventsFn              func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error)
	sendEventsFullFn          func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error)
	continueWithToolResultsFn func(context.Context, *api.Conversation, []api.ToolResult) (api.CompletionResult, error)
	generateNickFn            func(context.Context, domain.ModelID, domain.ModelID) (domain.Nick, error)
}

func (f *integrationAPI) ListModels(context.Context) ([]api.ModelInfo, error) {
	return nil, nil
}

func (f *integrationAPI) SendEvents(
	ctx context.Context,
	modelID domain.ModelID,
	system string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
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

func (f *integrationAPI) GenerateNick(ctx context.Context, nickModel domain.ModelID, modelID domain.ModelID) (api.NicknameResult, error) {
	if f.generateNickFn != nil {
		nick, err := f.generateNickFn(ctx, nickModel, modelID)
		return api.NicknameResult{Nick: nick}, err
	}

	return api.NicknameResult{Nick: "botty"}, nil
}

type integrationConfigStore struct {
	cfg     config.Config
	saveErr error
}

func (s *integrationConfigStore) Load() (config.Config, error) {
	return s.cfg, nil
}

func (s *integrationConfigStore) Save(cfg config.Config) error {
	if s.saveErr != nil {
		return s.saveErr
	}

	s.cfg = cfg
	return nil
}

func (s *integrationConfigStore) OnChange(_ config.ChangeFunc) config.UnsubscribeFunc {
	return func() {}
}

func newIntegrationSession(t *testing.T, apiClient api.Client) (*session.Session, *storemod.SQLiteStore) {
	t.Helper()

	return newIntegrationSessionWithConfigStore(t, apiClient, &integrationConfigStore{
		cfg: config.Config{
			UserNick:     "testuser",
			PokeInterval: 5 * time.Minute,
		},
	})
}

func newIntegrationSessionWithConfigStore(
	t *testing.T,
	apiClient api.Client,
	cfgStore *integrationConfigStore,
) (*session.Session, *storemod.SQLiteStore) {
	t.Helper()

	store := storetest.NewMemoryStore(t)
	memStore := memory.NewStoreAdapter(storetest.NewMemoryStore(t))
	sess := session.New(store, memStore, apiClient, cfgStore, "testuser")

	return sess, store
}

func advanceConnection(tm *uitest.App, ticks int) {
	for range ticks {
		tm.Send(screens.ConnectionTickMsg{})
	}
}

func seedInstance(t *testing.T, store *storemod.SQLiteStore, inst domain.Instance) {
	t.Helper()

	require.NoError(t, store.SaveInstance(t.Context(), inst))
}
