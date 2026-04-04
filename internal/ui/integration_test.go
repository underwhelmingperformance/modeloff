package ui_test

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
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
)

func TestApp_startup_without_api_key(t *testing.T) {
	root := uipkg.NewRoot(screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey: false,
		Nick:      "alice",
	}))
	tm := newTestApp(t, root)

	advanceConnection(tm, 2)
	waitForOutput(t, tm, "/config", "No API key configured")

	view := finalView(t, tm)
	require.Contains(t, view, "/config")
	require.Contains(t, view, "No API key configured")
}

func TestApp_startup_with_saved_channels(t *testing.T) {
	sess, store := newIntegrationSession(t, &integrationAPI{})

	seedChannel(t, sess, "#general")
	seedChannel(t, sess, "#random")
	seedMessage(t, sess, "#random", "hello from last time")

	root := uipkg.NewRoot(screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey:    true,
		ChannelCount: 2,
		Nick:         string(sess.UserNick()),
		Next:         screens.NewChatScreen(t.Context(), sess),
	}))
	tm := newTestApp(t, root)

	advanceConnection(tm, 4)
	waitForOutput(t, tm, "#random", "hello from last time")

	last, err := store.GetLastChannel(t.Context())
	require.NoError(t, err)
	require.Equal(t, domain.ChannelName("#random"), last)

	view := finalView(t, tm)
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
				Kind: protocol.ResponseReply,
				Body: "hello back",
			}, nil
		},
	}
	sess, _ := newIntegrationSession(t, apiClient)
	seedChannel(t, sess, "#general")

	tm := newTestApp(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	waitForOutput(t, tm, "#general")

	submitText(tm, "/invite test/model")
	waitForOutput(t, tm, "botty (test/model) has joined #general")

	submitText(tm, "hello world")
	waitForOutput(t, tm, "hello back")

	view := finalView(t, tm)
	require.Contains(t, view, "hello world")
	require.Contains(t, view, "hello back")
}

func TestApp_open_dm_and_send_message(t *testing.T) {
	apiClient := &integrationAPI{}
	sess, store := newIntegrationSession(t, apiClient)
	seedChannel(t, sess, "#general")
	seedInstance(t, store, domain.ModelInstance{
		Nick:    "botty",
		ModelID: "test/model",
	})

	tm := newTestApp(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	waitForOutput(t, tm, "#general")

	submitText(tm, "/msg botty hello there")
	waitForOutput(t, tm, "Opened direct message with botty")

	view := finalView(t, tm)
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
					Kind: protocol.ResponseReply,
					Body: "still alive",
				}, nil
			}

			return protocol.ModelResponse{Kind: protocol.ResponseSilence}, nil
		},
	}
	sess, _ := newIntegrationSession(t, apiClient)
	seedChannel(t, sess, "#general")

	_, err := sess.Invite(t.Context(), "#general", "test/model", "")
	require.NoError(t, err)

	tm := newTestApp(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	waitForOutput(t, tm, "#general")

	tm.Send(screens.PokeTickMsg{})
	waitForOutput(t, tm, "still alive")

	view := finalView(t, tm)
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

	seedChannel(t, sess, "#general")
	seedChannel(t, sess, "#random")

	_, err := sess.Invite(t.Context(), "#general", "test/model", "Helpful assistant")
	require.NoError(t, err)

	tm := newTestApp(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess)))
	waitForOutput(t, tm, "#random")

	submitText(tm, "/invite botty")
	waitForOutput(t, tm, "botty (test/model) has joined #random")

	submitText(tm, "/whois botty")
	waitForOutput(t, tm, "persona: Helpful assistant", "channels: #general, #random")

	view := finalView(t, tm)
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

func newTestApp(t *testing.T, root uipkg.Root) *teatest.TestModel {
	t.Helper()

	tm := teatest.NewTestModel(t, root, teatest.WithInitialTermSize(80, 24))
	t.Cleanup(func() { _ = tm.Quit() })

	return tm
}

func advanceConnection(tm *teatest.TestModel, ticks int) {
	for range ticks {
		tm.Send(screens.ConnectionTickMsg{})
	}
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

func seedInstance(t *testing.T, store *storemod.FileStore, inst domain.ModelInstance) {
	t.Helper()

	require.NoError(t, store.SaveInstance(t.Context(), inst))
}
