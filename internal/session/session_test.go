package session

import (
	"context"
	"fmt"
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

	return newTestSessionWithAPI(t, &fakeAPIClient{})
}

func newTestSessionWithAPI(t *testing.T, apiClient api.Client) (*Session, *storemod.FileStore) {
	t.Helper()

	s := storemod.NewFileStore(t.TempDir())
	sess := New(s, nil, apiClient, nil, "testuser")
	sess.now = func() time.Time { return fixedTime }

	return sess, s
}

func TestSession_Join(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := context.Background()

	evt, err := sess.Join(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, domain.JoinEvent{
		Room:    "#general",
		Nick:    "testuser",
		Created: true,
		At:      fixedTime,
	}, evt)

	// Room should be persisted.
	room, err := s.GetRoom(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, domain.Room{
		Name:    "#general",
		Kind:    domain.RoomChannel,
		Members: []domain.Nick{"testuser"},
		Created: fixedTime,
	}, room)

	// Last room should be set.
	last, err := s.GetLastRoom(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.RoomName("#general"), last)
}

func TestSession_JoinExistingRoom(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := context.Background()

	existing := domain.Room{
		Name:    "#existing",
		Kind:    domain.RoomChannel,
		Title:   "Already here",
		Members: []domain.Nick{"testuser"},
		Created: fixedTime.Add(-time.Hour),
	}
	require.NoError(t, s.SaveRoom(ctx, existing))

	evt, err := sess.Join(ctx, "#existing")
	require.NoError(t, err)
	require.Equal(t, domain.JoinEvent{
		Room: "#existing",
		Nick: "testuser",
		At:   fixedTime,
	}, evt)

	// Room should not be overwritten.
	room, err := s.GetRoom(ctx, "#existing")
	require.NoError(t, err)
	require.Equal(t, "Already here", room.Title)
}

func TestSession_Leave(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := context.Background()

	room := domain.Room{Name: "#leaving", Kind: domain.RoomChannel, Created: fixedTime}
	require.NoError(t, s.SaveRoom(ctx, room))

	evt, err := sess.Leave(ctx, "#leaving")
	require.NoError(t, err)
	require.Equal(t, domain.PartEvent{
		Room: "#leaving",
		Nick: "testuser",
		At:   fixedTime,
	}, evt)
}

func TestSession_LeaveNonexistent(t *testing.T) {
	sess, _ := newTestSession(t)

	_, err := sess.Leave(context.Background(), "#ghost")
	require.Error(t, err)
}

func TestSession_Invite(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := context.Background()

	room := domain.Room{
		Name:    "#dev",
		Kind:    domain.RoomChannel,
		Members: []domain.Nick{"testuser"},
		Created: fixedTime,
	}
	require.NoError(t, s.SaveRoom(ctx, room))

	evt, err := sess.Invite(ctx, "#dev", "anthropic/claude-3-haiku")
	require.NoError(t, err)
	require.Equal(t, domain.ModelInvitedEvent{
		Room: "#dev",
		Instance: domain.ModelInstance{
			Nick:    "fakenick",
			ModelID: "anthropic/claude-3-haiku",
			Rooms:   []domain.RoomName{"#dev"},
		},
		At: fixedTime,
	}, evt)

	// Instance should be persisted.
	inst, err := s.GetInstance(ctx, "fakenick")
	require.NoError(t, err)
	require.Equal(t, domain.ModelID("anthropic/claude-3-haiku"), inst.ModelID)

	// Room should have new member.
	updated, err := s.GetRoom(ctx, "#dev")
	require.NoError(t, err)
	require.Equal(t, []domain.Nick{"testuser", "fakenick"}, updated.Members)
}

func TestSession_Kick(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := context.Background()

	room := domain.Room{
		Name:    "#dev",
		Kind:    domain.RoomChannel,
		Members: []domain.Nick{"testuser", "botty"},
		Created: fixedTime,
	}
	require.NoError(t, s.SaveRoom(ctx, room))

	evt, err := sess.Kick(ctx, "#dev", "botty")
	require.NoError(t, err)
	require.Equal(t, domain.ModelKickedEvent{
		Room: "#dev",
		Nick: "botty",
		At:   fixedTime,
	}, evt)

	// Room should no longer have the kicked member.
	updated, err := s.GetRoom(ctx, "#dev")
	require.NoError(t, err)
	require.Equal(t, []domain.Nick{"testuser"}, updated.Members)
}

func TestSession_SendMessage(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := context.Background()

	evt, err := sess.SendMessage(ctx, "#general", "hello world")
	require.NoError(t, err)
	require.Equal(t, domain.MessageEvent{
		Message: domain.Message{
			ID:     fmt.Sprintf("%d", fixedTime.UnixNano()),
			Room:   "#general",
			From:   "testuser",
			Body:   "hello world",
			SentAt: fixedTime,
		},
	}, evt)

	// Message should be persisted.
	msgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{evt.Message}, msgs)
}

func TestSession_SetTitle(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := context.Background()

	room := domain.Room{Name: "#dev", Kind: domain.RoomChannel, Created: fixedTime}
	require.NoError(t, s.SaveRoom(ctx, room))

	evt, err := sess.SetTitle(ctx, "#dev", "Development Chat")
	require.NoError(t, err)
	require.Equal(t, domain.TopicChangeEvent{
		Room:  "#dev",
		Title: "Development Chat",
		By:    "testuser",
		At:    fixedTime,
	}, evt)

	// Room title should be updated.
	updated, err := s.GetRoom(ctx, "#dev")
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
		Rooms:   []domain.RoomName{"#dev"},
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

func TestSession_InviteNonexistentRoom(t *testing.T) {
	sess, _ := newTestSession(t)

	_, err := sess.Invite(context.Background(), "#ghost", "anthropic/claude-3-haiku")
	require.Error(t, err)
}

func TestSession_InviteGenerateNickError(t *testing.T) {
	fake := &fakeAPIClient{
		generateNickFn: func(_ context.Context, _ domain.ModelID) (domain.Nick, error) {
			return "", fmt.Errorf("API unavailable")
		},
	}

	sess, s := newTestSessionWithAPI(t, fake)
	ctx := context.Background()

	room := domain.Room{
		Name:    "#dev",
		Kind:    domain.RoomChannel,
		Members: []domain.Nick{"testuser"},
		Created: fixedTime,
	}
	require.NoError(t, s.SaveRoom(ctx, room))

	_, err := sess.Invite(ctx, "#dev", "anthropic/claude-3-haiku")
	require.Error(t, err)
}

func TestSession_KickNonexistentRoom(t *testing.T) {
	sess, _ := newTestSession(t)

	_, err := sess.Kick(context.Background(), "#ghost", "botty")
	require.Error(t, err)
}

func TestSession_KickNonMember(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := context.Background()

	room := domain.Room{
		Name:    "#dev",
		Kind:    domain.RoomChannel,
		Members: []domain.Nick{"testuser"},
		Created: fixedTime,
	}
	require.NoError(t, s.SaveRoom(ctx, room))

	evt, err := sess.Kick(ctx, "#dev", "nobody")
	require.NoError(t, err)
	require.Equal(t, domain.ModelKickedEvent{
		Room: "#dev",
		Nick: "nobody",
		At:   fixedTime,
	}, evt)

	// Members should be unchanged.
	updated, err := s.GetRoom(ctx, "#dev")
	require.NoError(t, err)
	require.Equal(t, []domain.Nick{"testuser"}, updated.Members)
}

func TestSession_SetTitleNonexistentRoom(t *testing.T) {
	sess, _ := newTestSession(t)

	_, err := sess.SetTitle(context.Background(), "#ghost", "title")
	require.Error(t, err)
}

// --- Fake API client ---

type fakeAPIClient struct {
	listModelsFn   func(context.Context) ([]api.ModelInfo, error)
	sendEventsFn   func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error)
	generateNickFn func(context.Context, domain.ModelID) (domain.Nick, error)
}

func (f *fakeAPIClient) ListModels(ctx context.Context) ([]api.ModelInfo, error) {
	if f.listModelsFn != nil {
		return f.listModelsFn(ctx)
	}

	return nil, nil
}

func (f *fakeAPIClient) SendEvents(
	ctx context.Context,
	modelID domain.ModelID,
	system string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
) (protocol.ModelResponse, error) {
	if f.sendEventsFn != nil {
		return f.sendEventsFn(ctx, modelID, system, history, events)
	}

	return protocol.ModelResponse{Kind: protocol.ResponseSilence, Reason: "fake"}, nil
}

func (f *fakeAPIClient) GenerateNick(ctx context.Context, modelID domain.ModelID) (domain.Nick, error) {
	if f.generateNickFn != nil {
		return f.generateNickFn(ctx, modelID)
	}

	return "fakenick", nil
}
