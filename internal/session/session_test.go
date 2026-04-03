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
		Channel: "#general",
		Nick:    "testuser",
		Created: true,
		At:      fixedTime,
	}, evt)

	// Channel should be persisted.
	ch, err := s.GetChannel(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, domain.Channel{
		Name:    "#general",
		Kind:    domain.KindChannel,
		Members: []domain.Nick{"testuser"},
		Created: fixedTime,
	}, ch)

	// Last channel should be set.
	last, err := s.GetLastChannel(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.ChannelName("#general"), last)
}

func TestSession_JoinExistingChannel(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := context.Background()

	existing := domain.Channel{
		Name:    "#existing",
		Kind:    domain.KindChannel,
		Title:   "Already here",
		Members: []domain.Nick{"testuser"},
		Created: fixedTime.Add(-time.Hour),
	}
	require.NoError(t, s.SaveChannel(ctx, existing))

	evt, err := sess.Join(ctx, "#existing")
	require.NoError(t, err)
	require.Equal(t, domain.JoinEvent{
		Channel: "#existing",
		Nick:    "testuser",
		At:      fixedTime,
	}, evt)

	// Channel should not be overwritten.
	ch, err := s.GetChannel(ctx, "#existing")
	require.NoError(t, err)
	require.Equal(t, "Already here", ch.Title)
}

func TestSession_Leave(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := context.Background()

	ch := domain.Channel{Name: "#leaving", Kind: domain.KindChannel, Created: fixedTime}
	require.NoError(t, s.SaveChannel(ctx, ch))

	evt, err := sess.Leave(ctx, "#leaving")
	require.NoError(t, err)
	require.Equal(t, domain.PartEvent{
		Channel: "#leaving",
		Nick:    "testuser",
		At:      fixedTime,
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

	ch := domain.Channel{
		Name:    "#dev",
		Kind:    domain.KindChannel,
		Members: []domain.Nick{"testuser"},
		Created: fixedTime,
	}
	require.NoError(t, s.SaveChannel(ctx, ch))

	evt, err := sess.Invite(ctx, "#dev", "anthropic/claude-3-haiku")
	require.NoError(t, err)
	require.Equal(t, domain.ModelInvitedEvent{
		Channel: "#dev",
		Instance: domain.ModelInstance{
			Nick:     "fakenick",
			ModelID:  "anthropic/claude-3-haiku",
			Channels: []domain.ChannelName{"#dev"},
		},
		At: fixedTime,
	}, evt)

	// Instance should be persisted.
	inst, err := s.GetInstance(ctx, "fakenick")
	require.NoError(t, err)
	require.Equal(t, domain.ModelID("anthropic/claude-3-haiku"), inst.ModelID)

	// Channel should have new member.
	updated, err := s.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	require.Equal(t, []domain.Nick{"testuser", "fakenick"}, updated.Members)
}

func TestSession_Kick(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := context.Background()

	ch := domain.Channel{
		Name:    "#dev",
		Kind:    domain.KindChannel,
		Members: []domain.Nick{"testuser", "botty"},
		Created: fixedTime,
	}
	require.NoError(t, s.SaveChannel(ctx, ch))

	evt, err := sess.Kick(ctx, "#dev", "botty")
	require.NoError(t, err)
	require.Equal(t, domain.ModelKickedEvent{
		Channel: "#dev",
		Nick:    "botty",
		At:      fixedTime,
	}, evt)

	// Channel should no longer have the kicked member.
	updated, err := s.GetChannel(ctx, "#dev")
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
			ID:      fmt.Sprintf("%d", fixedTime.UnixNano()),
			Channel: "#general",
			From:    "testuser",
			Body:    "hello world",
			SentAt:  fixedTime,
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

	ch := domain.Channel{Name: "#dev", Kind: domain.KindChannel, Created: fixedTime}
	require.NoError(t, s.SaveChannel(ctx, ch))

	evt, err := sess.SetTitle(ctx, "#dev", "Development Chat")
	require.NoError(t, err)
	require.Equal(t, domain.TopicChangeEvent{
		Channel: "#dev",
		Title:   "Development Chat",
		By:      "testuser",
		At:      fixedTime,
	}, evt)

	// Channel title should be updated.
	updated, err := s.GetChannel(ctx, "#dev")
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
		Nick:     "botty",
		ModelID:  "test/model",
		Persona:  "A test bot",
		Channels: []domain.ChannelName{"#dev"},
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

func TestSession_InviteNonexistentChannel(t *testing.T) {
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

	ch := domain.Channel{
		Name:    "#dev",
		Kind:    domain.KindChannel,
		Members: []domain.Nick{"testuser"},
		Created: fixedTime,
	}
	require.NoError(t, s.SaveChannel(ctx, ch))

	_, err := sess.Invite(ctx, "#dev", "anthropic/claude-3-haiku")
	require.Error(t, err)
}

func TestSession_KickNonexistentChannel(t *testing.T) {
	sess, _ := newTestSession(t)

	_, err := sess.Kick(context.Background(), "#ghost", "botty")
	require.Error(t, err)
}

func TestSession_KickNonMember(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := context.Background()

	ch := domain.Channel{
		Name:    "#dev",
		Kind:    domain.KindChannel,
		Members: []domain.Nick{"testuser"},
		Created: fixedTime,
	}
	require.NoError(t, s.SaveChannel(ctx, ch))

	evt, err := sess.Kick(ctx, "#dev", "nobody")
	require.NoError(t, err)
	require.Equal(t, domain.ModelKickedEvent{
		Channel: "#dev",
		Nick:    "nobody",
		At:      fixedTime,
	}, evt)

	// Members should be unchanged.
	updated, err := s.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	require.Equal(t, []domain.Nick{"testuser"}, updated.Members)
}

func TestSession_SetTitleNonexistentChannel(t *testing.T) {
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
