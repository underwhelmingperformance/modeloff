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

	// The layout renders normally: sidebar, input bar, and status bar
	// are all present.
	require.Contains(t, v, "No channels")
	require.Contains(t, v, ">")
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

func TestChatScreen_WelcomeState_accepts_commands(t *testing.T) {
	sess := newTestSession(t)

	m := initChatScreen(t, sess)

	// Simulate typing /join #general and pressing enter.
	for _, r := range "/join #general" {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	// Enter produces a tea.Cmd that yields CommandSubmitMsg.
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd)

	// Apply the CommandSubmitMsg.
	m, cmd = m.Update(cmd())
	require.NotNil(t, cmd)

	// Apply the commandResultMsg from the join operation.
	m, _ = m.Update(cmd())

	v := m.View(80, 24)
	require.Contains(t, v, "#general")
	require.NotContains(t, v, "Welcome to modeloff")
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

func TestChatScreen_message_on_welcome_screen_is_ignored(t *testing.T) {
	sess := newTestSession(t)

	m := initChatScreen(t, sess)

	// Type a non-command message on the welcome screen.
	for _, r := range "hello" {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	// Enter → tea.Cmd → MessageSubmitMsg → "join a channel first" warning.
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd)

	m, cmd = m.Update(cmd())

	if cmd != nil {
		m, _ = m.Update(cmd())
	}

	v := m.View(80, 24)

	// Should not show the message as a chat line — no active channel.
	require.NotContains(t, v, "<testuser> hello")

	// Should show the "join a channel first" warning.
	require.Contains(t, v, "join a channel first")
}

func TestChatScreen_unknown_command_on_welcome_screen(t *testing.T) {
	sess := newTestSession(t)

	m := initChatScreen(t, sess)

	// Type /foo and press enter on the welcome screen.
	for _, r := range "/foo" {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd)

	// Apply the CommandSubmitMsg.
	m, cmd = m.Update(cmd())

	// Apply the systemEventMsg if there is one.
	if cmd != nil {
		m, _ = m.Update(cmd())
	}

	v := m.View(80, 24)

	// Should show the error, not a phantom message.
	require.Contains(t, v, "unknown command: /foo")
	require.NotContains(t, v, "<testuser> as")
}

func TestChatScreen_quit_command(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	// Type /quit and press enter.
	for _, r := range "/quit" {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	// Enter produces a tea.Cmd that yields CommandSubmitMsg.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd)

	// Apply the CommandSubmitMsg — should return tea.Quit.
	_, cmd = m.Update(cmd())
	require.NotNil(t, cmd)

	// tea.Quit returns a tea.QuitMsg when executed.
	msg := cmd()
	_, ok := msg.(tea.QuitMsg)
	require.True(t, ok, "expected tea.QuitMsg, got %T", msg)
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
