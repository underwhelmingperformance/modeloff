package screens_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
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
	sess := session.New(s, nil, &fakeAPI{}, nil, "testuser")

	return sess
}

func seedRoom(t *testing.T, sess *session.Session, name string) {
	t.Helper()

	_, err := sess.Join(context.Background(), name)
	require.NoError(t, err)
}

func seedMessage(t *testing.T, sess *session.Session, room, body string) {
	t.Helper()

	_, err := sess.SendMessage(context.Background(), domain.RoomName(room), body)
	require.NoError(t, err)
}

func TestChatScreen_Init_loads_rooms(t *testing.T) {
	sess := newTestSession(t)
	seedRoom(t, sess, "¢general")
	seedMessage(t, sess, "¢general", "hello")

	cs := screens.NewChatScreen(sess)
	cmd := cs.Init()
	require.NotNil(t, cmd)

	// Execute the init command.
	msg := cmd()
	m, _ := cs.Update(msg)

	v := m.View(80, 24)
	require.Contains(t, v, "¢general")
	require.Contains(t, v, "hello")
}

func TestChatScreen_Init_empty(t *testing.T) {
	sess := newTestSession(t)

	cs := screens.NewChatScreen(sess)
	cmd := cs.Init()
	msg := cmd()
	m, _ := cs.Update(msg)

	v := m.View(80, 24)
	require.Contains(t, v, "No rooms")
	require.Contains(t, v, "No messages")
}

func TestChatScreen_room_selection(t *testing.T) {
	sess := newTestSession(t)
	seedRoom(t, sess, "¢general")
	seedRoom(t, sess, "¢random")
	seedMessage(t, sess, "¢random", "random msg")

	cs := screens.NewChatScreen(sess)

	// Load initial state.
	msg := cs.Init()()
	var m ui.Model
	m, _ = cs.Update(msg)

	// Select ¢random.
	m, cmd := m.Update(components.RoomSelectedMsg{Room: "¢random"})
	require.NotNil(t, cmd)

	// Execute the switch command.
	msg = cmd()
	m, _ = m.Update(msg)

	v := m.View(80, 24)
	require.Contains(t, v, "random msg")
}

func TestChatScreen_send_message(t *testing.T) {
	sess := newTestSession(t)
	seedRoom(t, sess, "¢general")

	cs := screens.NewChatScreen(sess)

	// Load initial state.
	msg := cs.Init()()
	var m ui.Model
	m, _ = cs.Update(msg)

	// Send a message.
	m, cmd := m.Update(components.MessageSubmitMsg{Text: "hello world"})
	require.NotNil(t, cmd)

	// Execute the send command.
	msg = cmd()
	m, _ = m.Update(msg)

	v := m.View(80, 24)
	require.Contains(t, v, "hello world")
}

func TestChatScreen_join_command(t *testing.T) {
	sess := newTestSession(t)

	cs := screens.NewChatScreen(sess)

	// Load initial state.
	msg := cs.Init()()
	var m ui.Model
	m, _ = cs.Update(msg)

	// Issue /join command.
	m, cmd := m.Update(components.CommandSubmitMsg{Name: "join", Args: "¢newroom"})
	require.NotNil(t, cmd)

	msg = cmd()
	m, _ = m.Update(msg)

	v := m.View(80, 24)
	require.Contains(t, v, "¢newroom")
}

func TestChatScreen_leave_command(t *testing.T) {
	sess := newTestSession(t)
	seedRoom(t, sess, "¢general")
	seedRoom(t, sess, "¢random")

	cs := screens.NewChatScreen(sess)

	// Load.
	msg := cs.Init()()
	var m ui.Model
	m, _ = cs.Update(msg)

	// Leave the active room.
	m, cmd := m.Update(components.CommandSubmitMsg{Name: "leave", Args: ""})
	require.NotNil(t, cmd)

	msg = cmd()
	m, _ = m.Update(msg)

	// Should still have rooms visible (switched to the other one).
	v := m.View(80, 24)
	require.NotEmpty(t, v)
}

func TestChatScreen_nick_command(t *testing.T) {
	sess := newTestSession(t)
	seedRoom(t, sess, "¢general")

	cs := screens.NewChatScreen(sess)

	msg := cs.Init()()
	var m ui.Model
	m, _ = cs.Update(msg)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "nick", Args: "newnick"})
	require.NotNil(t, cmd)

	msg = cmd()
	m, _ = m.Update(msg)

	v := m.View(80, 24)
	require.Contains(t, v, "testuser is now known as newnick")
}

func TestChatScreen_title_command(t *testing.T) {
	sess := newTestSession(t)
	seedRoom(t, sess, "¢general")

	cs := screens.NewChatScreen(sess)

	msg := cs.Init()()
	var m ui.Model
	m, _ = cs.Update(msg)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "title", Args: "cool topic"})
	require.NotNil(t, cmd)

	msg = cmd()
	m, _ = m.Update(msg)

	v := m.View(80, 24)
	require.Contains(t, v, "topic for ¢general set to: cool topic")
}

func TestChatScreen_title_clear(t *testing.T) {
	sess := newTestSession(t)
	seedRoom(t, sess, "¢general")

	cs := screens.NewChatScreen(sess)

	msg := cs.Init()()
	var m ui.Model
	m, _ = cs.Update(msg)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "title", Args: ""})
	require.NotNil(t, cmd)

	msg = cmd()
	m, _ = m.Update(msg)

	v := m.View(80, 24)
	require.Contains(t, v, "topic for ¢general cleared")
}

func TestChatScreen_whois_command(t *testing.T) {
	sess := newTestSession(t)
	seedRoom(t, sess, "¢general")

	// Invite a model so there's an instance to whois.
	_, err := sess.Invite(context.Background(), "¢general", "anthropic/claude-3-haiku")
	require.NoError(t, err)

	cs := screens.NewChatScreen(sess)

	msg := cs.Init()()
	var m ui.Model
	m, _ = cs.Update(msg)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "whois", Args: "fakenick"})
	require.NotNil(t, cmd)

	msg = cmd()
	m, _ = m.Update(msg)

	v := m.View(80, 24)
	require.Contains(t, v, "fakenick is anthropic/claude-3-haiku")
}

func TestChatScreen_whois_unknown_nick(t *testing.T) {
	sess := newTestSession(t)
	seedRoom(t, sess, "¢general")

	cs := screens.NewChatScreen(sess)

	msg := cs.Init()()
	var m ui.Model
	m, _ = cs.Update(msg)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "whois", Args: "nobody"})
	require.NotNil(t, cmd)

	msg = cmd()
	m, _ = m.Update(msg)

	v := m.View(80, 24)
	require.Contains(t, v, "no such nick: nobody")
}

func TestChatScreen_list_command(t *testing.T) {
	sess := newTestSession(t)
	seedRoom(t, sess, "¢general")
	seedRoom(t, sess, "¢random")

	cs := screens.NewChatScreen(sess)

	msg := cs.Init()()
	var m ui.Model
	m, _ = cs.Update(msg)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "list", Args: ""})
	require.NotNil(t, cmd)

	msg = cmd()
	m, _ = m.Update(msg)

	v := m.View(80, 24)
	require.Contains(t, v, "¢general")
	require.Contains(t, v, "¢random")
}

func TestChatScreen_list_empty(t *testing.T) {
	sess := newTestSession(t)

	cs := screens.NewChatScreen(sess)

	msg := cs.Init()()
	var m ui.Model
	m, _ = cs.Update(msg)

	_, cmd := m.Update(components.CommandSubmitMsg{Name: "list", Args: ""})
	require.NotNil(t, cmd)

	msg = cmd()
	m, _ = m.Update(msg)

	v := m.View(80, 24)
	require.Contains(t, v, "no rooms")
}

func TestChatScreen_unimplemented_command(t *testing.T) {
	sess := newTestSession(t)
	seedRoom(t, sess, "¢general")

	cs := screens.NewChatScreen(sess)

	msg := cs.Init()()
	var m ui.Model
	m, _ = cs.Update(msg)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "config", Args: ""})
	require.NotNil(t, cmd)

	msg = cmd()
	m, _ = m.Update(msg)

	v := m.View(80, 24)
	require.Contains(t, v, "/config is not yet implemented")
}

func TestChatScreen_invalid_command(t *testing.T) {
	sess := newTestSession(t)
	seedRoom(t, sess, "¢general")

	cs := screens.NewChatScreen(sess)

	msg := cs.Init()()
	var m ui.Model
	m, _ = cs.Update(msg)

	// /nick without args is a parse error.
	m, cmd := m.Update(components.CommandSubmitMsg{Name: "nick", Args: ""})
	require.NotNil(t, cmd)

	msg = cmd()
	m, _ = m.Update(msg)

	v := m.View(80, 24)
	require.Contains(t, v, "/nick requires a new nickname")
}

func TestChatScreen_unknown_command_shows_error(t *testing.T) {
	sess := newTestSession(t)
	seedRoom(t, sess, "¢general")

	cs := screens.NewChatScreen(sess)

	msg := cs.Init()()
	var m ui.Model
	m, _ = cs.Update(msg)

	m, cmd := m.Update(components.CommandSubmitMsg{Name: "unknown", Args: ""})
	require.NotNil(t, cmd)

	msg = cmd()
	m, _ = m.Update(msg)

	v := m.View(80, 24)
	require.Contains(t, v, "unknown command: /unknown")
}

func TestChatScreen_View_responsive(t *testing.T) {
	sess := newTestSession(t)
	seedRoom(t, sess, "¢general")

	cs := screens.NewChatScreen(sess)

	msg := cs.Init()()
	m, _ := cs.Update(msg)

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
