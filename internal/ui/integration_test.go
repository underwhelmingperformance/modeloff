package ui_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	storemod "github.com/laney/modeloff/internal/store"
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

	view := tm.FinalView()
	require.Contains(t, view, "/config")
	require.Contains(t, view, "No API key configured")
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
	tm.WaitFor("#random", "hello from last time")

	last, err := store.GetLastChannel(t.Context())
	require.NoError(t, err)
	require.Equal(t, domain.ChannelName("#random"), last)

	view := tm.FinalView()
	require.Contains(t, view, "#general")
	require.Contains(t, view, "#random")
	require.Contains(t, view, "hello from last time")
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
	tm.WaitFor("hello back")

	view := tm.FinalView()
	require.Contains(t, view, "hello world")
	require.Contains(t, view, "hello back")
}

func TestApp_open_dm_and_send_message(t *testing.T) {
	apiClient := &integrationAPI{}
	sess, store := newIntegrationSession(t, apiClient)
	uitest.SeedChannel(t, sess, "#general")
	seedInstance(t, store, domain.ModelInstance{
		Nick:    "botty",
		ModelID: "test/model",
	})

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	tm.WaitFor("#general")

	tm.Submit("/msg botty hello there")
	tm.WaitFor("Opened direct message with botty")

	view := tm.FinalView()
	require.Contains(t, view, "botty")
	require.Contains(t, view, "hello there")
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

	_, err := sess.Invite(t.Context(), "#general", "test/model", "")
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	tm.WaitFor("#general")

	tm.Send(screens.PokeTickMsg{})
	tm.WaitFor("still alive")

	view := tm.FinalView()
	require.Contains(t, view, "still alive")
}

func TestApp_reuse_existing_instance(t *testing.T) {
	apiClient := &integrationAPI{}
	_, store := newIntegrationSession(t, apiClient)
	memStore := memory.NewFileStore(t.TempDir())
	require.NoError(t, memStore.Write(t.Context(), "botty", memory.Entry{
		Key:     "topic",
		Content: "favourite channel regular",
	}))

	sess := session.New(store, memStore, apiClient, &integrationConfigStore{}, "testuser")

	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	_, err := sess.Invite(t.Context(), "#general", "test/model", "Helpful assistant")
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	tm.WaitFor("#random")

	tm.Submit("/invite botty")
	tm.WaitFor("botty (test/model) has joined #random")

	tm.Submit("/whois botty")
	tm.WaitFor("persona: Helpful assistant", "channels: #general, #random")

	view := tm.FinalView()
	require.Contains(t, view, "persona: Helpful assistant")
	require.Contains(t, view, "channels: #general, #random")
}

type integrationAPI struct {
	mu             sync.Mutex
	sendEventsFn   func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error)
	generateNickFn func(context.Context, domain.ModelID, domain.ModelID) (domain.Nick, error)
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
) (protocol.ModelResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.sendEventsFn != nil {
		return f.sendEventsFn(ctx, modelID, system, history, events)
	}

	return protocol.ModelResponse{Kind: protocol.ResponseSilence}, nil
}

func (f *integrationAPI) GenerateNick(ctx context.Context, nickModel domain.ModelID, modelID domain.ModelID) (domain.Nick, error) {
	if f.generateNickFn != nil {
		return f.generateNickFn(ctx, nickModel, modelID)
	}

	return "botty", nil
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

func newIntegrationSession(t *testing.T, apiClient api.Client) (*session.Session, *storemod.FileStore) {
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
) (*session.Session, *storemod.FileStore) {
	t.Helper()

	store := storemod.NewFileStore(t.TempDir())
	memStore := memory.NewFileStore(t.TempDir())
	sess := session.New(store, memStore, apiClient, cfgStore, "testuser")

	return sess, store
}

func advanceConnection(tm *uitest.App, ticks int) {
	for range ticks {
		tm.Send(screens.ConnectionTickMsg{})
	}
}

func seedInstance(t *testing.T, store *storemod.FileStore, inst domain.ModelInstance) {
	t.Helper()

	require.NoError(t, store.SaveInstance(t.Context(), inst))
}
