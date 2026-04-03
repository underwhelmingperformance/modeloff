package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
)

var fixedTime = time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

func newTestSession(t *testing.T) (*Session, *storemod.FileStore) {
	t.Helper()

	s := storemod.NewFileStore(t.TempDir())
	sess := New(s, nil, &fakeAPIClient{}, nil, "testuser")
	sess.now = func() time.Time { return fixedTime }

	return sess, s
}

func TestSession_Join(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := context.Background()

	evt, err := sess.Join(ctx, "¢general")
	require.NoError(t, err)
	require.Equal(t, domain.JoinEvent{
		Room: "¢general",
		Nick: "testuser",
		At:   fixedTime,
	}, evt)

	// Room should be persisted.
	room, err := s.GetRoom(ctx, "¢general")
	require.NoError(t, err)
	require.Equal(t, domain.RoomName("¢general"), room.Name)
	require.Equal(t, domain.RoomChannel, room.Kind)

	// Last room should be set.
	last, err := s.GetLastRoom(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.RoomName("¢general"), last)
}

func TestSession_JoinExistingRoom(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := context.Background()

	existing := domain.Room{
		Name:    "¢existing",
		Kind:    domain.RoomChannel,
		Title:   "Already here",
		Members: []domain.Nick{"testuser"},
		Created: fixedTime.Add(-time.Hour),
	}
	require.NoError(t, s.SaveRoom(ctx, existing))

	evt, err := sess.Join(ctx, "¢existing")
	require.NoError(t, err)
	require.Equal(t, domain.RoomName("¢existing"), evt.Room)

	// Room should not be overwritten.
	room, err := s.GetRoom(ctx, "¢existing")
	require.NoError(t, err)
	require.Equal(t, "Already here", room.Title)
}

func TestSession_Leave(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := context.Background()

	room := domain.Room{Name: "¢leaving", Kind: domain.RoomChannel, Created: fixedTime}
	require.NoError(t, s.SaveRoom(ctx, room))

	evt, err := sess.Leave(ctx, "¢leaving")
	require.NoError(t, err)
	require.Equal(t, domain.PartEvent{
		Room: "¢leaving",
		Nick: "testuser",
		At:   fixedTime,
	}, evt)
}

func TestSession_LeaveNonexistent(t *testing.T) {
	sess, _ := newTestSession(t)

	_, err := sess.Leave(context.Background(), "¢ghost")
	require.Error(t, err)
}

func TestSession_Invite(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := context.Background()

	room := domain.Room{
		Name:    "¢dev",
		Kind:    domain.RoomChannel,
		Members: []domain.Nick{"testuser"},
		Created: fixedTime,
	}
	require.NoError(t, s.SaveRoom(ctx, room))

	evt, err := sess.Invite(ctx, "¢dev", "anthropic/claude-3-haiku")
	require.NoError(t, err)
	require.Equal(t, domain.RoomName("¢dev"), evt.Room)
	require.Equal(t, domain.ModelID("anthropic/claude-3-haiku"), evt.Instance.ModelID)
	require.Equal(t, domain.Nick("fakenick"), evt.Instance.Nick)

	// Instance should be persisted.
	inst, err := s.GetInstance(ctx, "fakenick")
	require.NoError(t, err)
	require.Equal(t, domain.ModelID("anthropic/claude-3-haiku"), inst.ModelID)

	// Room should have new member.
	updated, err := s.GetRoom(ctx, "¢dev")
	require.NoError(t, err)
	require.Equal(t, []domain.Nick{"testuser", "fakenick"}, updated.Members)
}

func TestSession_Kick(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := context.Background()

	room := domain.Room{
		Name:    "¢dev",
		Kind:    domain.RoomChannel,
		Members: []domain.Nick{"testuser", "botty"},
		Created: fixedTime,
	}
	require.NoError(t, s.SaveRoom(ctx, room))

	evt, err := sess.Kick(ctx, "¢dev", "botty")
	require.NoError(t, err)
	require.Equal(t, domain.ModelKickedEvent{
		Room: "¢dev",
		Nick: "botty",
		At:   fixedTime,
	}, evt)

	// Room should no longer have the kicked member.
	updated, err := s.GetRoom(ctx, "¢dev")
	require.NoError(t, err)
	require.Equal(t, []domain.Nick{"testuser"}, updated.Members)
}

func TestSession_SendMessage(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := context.Background()

	evt, err := sess.SendMessage(ctx, "¢general", "hello world")
	require.NoError(t, err)
	require.Equal(t, domain.Nick("testuser"), evt.Message.From)
	require.Equal(t, domain.RoomName("¢general"), evt.Message.Room)
	require.Equal(t, "hello world", evt.Message.Body)

	// Message should be persisted.
	msgs, err := s.ListMessages(ctx, "¢general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{evt.Message}, msgs)
}

func TestSession_SetTitle(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := context.Background()

	room := domain.Room{Name: "¢dev", Kind: domain.RoomChannel, Created: fixedTime}
	require.NoError(t, s.SaveRoom(ctx, room))

	evt, err := sess.SetTitle(ctx, "¢dev", "Development Chat")
	require.NoError(t, err)
	require.Equal(t, domain.TopicChangeEvent{
		Room:  "¢dev",
		Title: "Development Chat",
		By:    "testuser",
		At:    fixedTime,
	}, evt)

	// Room title should be updated.
	updated, err := s.GetRoom(ctx, "¢dev")
	require.NoError(t, err)
	require.Equal(t, "Development Chat", updated.Title)
}

func TestSession_ChangeNick(t *testing.T) {
	sess, _ := newTestSession(t)

	evt := sess.ChangeNick("newname")
	require.Equal(t, domain.NickChangeEvent{
		OldNick: "testuser",
		NewNick: "newname",
		At:      fixedTime,
	}, evt)

	require.Equal(t, domain.Nick("newname"), sess.UserNick())
}

func TestSession_Whois(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := context.Background()

	inst := domain.ModelInstance{
		Nick:    "botty",
		ModelID: "test/model",
		Persona: "A test bot",
		Rooms:   []domain.RoomName{"¢dev"},
	}
	require.NoError(t, s.SaveInstance(ctx, inst))

	got, err := sess.Whois(ctx, "botty")
	require.NoError(t, err)
	require.Equal(t, inst, got)
}

func TestSession_WhoisNotFound(t *testing.T) {
	sess, _ := newTestSession(t)

	_, err := sess.Whois(context.Background(), "ghost")
	require.Error(t, err)
}

// --- Fake API client ---

type fakeAPIClient struct{}

func (f *fakeAPIClient) ListModels(_ context.Context) ([]api.ModelInfo, error) {
	return nil, nil
}

func (f *fakeAPIClient) SendEvents(
	_ context.Context,
	_ domain.ModelID,
	_ string,
	_ []protocol.IRCMessage,
	_ []protocol.IRCMessage,
) (protocol.ModelResponse, error) {
	return protocol.ModelResponse{Kind: protocol.ResponseSilence, Reason: "fake"}, nil
}

func (f *fakeAPIClient) GenerateNick(_ context.Context, _ domain.ModelID) (domain.Nick, error) {
	return "fakenick", nil
}
