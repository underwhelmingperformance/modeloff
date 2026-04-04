package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/set"
)

var testTime = time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)

func newTestStore(t *testing.T) *FileStore {
	t.Helper()
	return NewFileStore(t.TempDir())
}

// --- Channels ---

func TestFileStore_ListChannelsEmpty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.ListChannels(t.Context())
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestFileStore_SaveAndGetChannel(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	ch := domain.Channel{
		Name:    "#general",
		Kind:    domain.KindChannel,
		Topic:   "General chat",
		Members: set.NewOrdered[domain.Nick]("alice", "bob"),
		Created: testTime,
	}

	require.NoError(t, s.SaveChannel(ctx, ch))

	got, err := s.GetChannel(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, ch, got)
}

func TestFileStore_GetChannelNotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.GetChannel(t.Context(), "#nonexistent")
	require.Error(t, err)
}

func TestFileStore_ListChannels(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	channels := []domain.Channel{
		{Name: "#alpha", Kind: domain.KindChannel, Created: testTime},
		{Name: "#beta", Kind: domain.KindChannel, Created: testTime.Add(time.Hour)},
	}

	for _, ch := range channels {
		require.NoError(t, s.SaveChannel(ctx, ch))
	}

	got, err := s.ListChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, channels, got)
}

func TestFileStore_ListChannels_includes_dms(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	channels := []domain.Channel{
		{Name: "#alpha", Kind: domain.KindChannel, Created: testTime},
		{Name: "botty", Kind: domain.KindDM, Created: testTime.Add(time.Hour)},
	}

	for _, ch := range channels {
		require.NoError(t, s.SaveChannel(ctx, ch))
	}

	got, err := s.ListChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, channels, got)
}

func TestFileStore_DeleteChannel(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	ch := domain.Channel{Name: "#deleteme", Kind: domain.KindChannel, Created: testTime}
	require.NoError(t, s.SaveChannel(ctx, ch))
	require.NoError(t, s.DeleteChannel(ctx, "#deleteme"))

	_, err := s.GetChannel(ctx, "#deleteme")
	require.Error(t, err)
}

func TestFileStore_SaveChannelOverwrites(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	ch := domain.Channel{Name: "#evolving", Kind: domain.KindChannel, Created: testTime}
	require.NoError(t, s.SaveChannel(ctx, ch))

	ch.Topic = "Updated topic"
	ch.Members = set.NewOrdered[domain.Nick]("charlie")
	require.NoError(t, s.SaveChannel(ctx, ch))

	got, err := s.GetChannel(ctx, "#evolving")
	require.NoError(t, err)
	require.Equal(t, ch, got)
}

// --- Messages ---

func TestFileStore_ListMessagesEmpty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.ListMessages(t.Context(), "#empty")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestFileStore_SaveAndListMessages(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	msgs := []domain.Message{
		{ID: "msg-1", Channel: "#general", From: "alice", Body: "hello", SentAt: testTime},
		{ID: "msg-2", Channel: "#general", From: "bob", Body: "hi", SentAt: testTime.Add(time.Second)},
	}

	for _, m := range msgs {
		require.NoError(t, s.SaveMessage(ctx, m))
	}

	got, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, msgs, got)
}

func TestFileStore_MessagesIsolatedByChannel(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SaveMessage(ctx, domain.Message{ID: "a", Channel: "#chan-a", From: "alice", Body: "a msg", SentAt: testTime}))
	require.NoError(t, s.SaveMessage(ctx, domain.Message{ID: "b", Channel: "#chan-b", From: "bob", Body: "b msg", SentAt: testTime}))

	gotA, err := s.ListMessages(ctx, "#chan-a")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{
		{ID: "a", Channel: "#chan-a", From: "alice", Body: "a msg", SentAt: testTime},
	}, gotA)

	gotB, err := s.ListMessages(ctx, "#chan-b")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{
		{ID: "b", Channel: "#chan-b", From: "bob", Body: "b msg", SentAt: testTime},
	}, gotB)
}

// --- Model instances ---

func TestFileStore_ListInstancesEmpty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.ListInstances(t.Context())
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestFileStore_SaveAndGetInstance(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	inst := domain.ModelInstance{
		Nick:     "claude",
		ModelID:  "anthropic/claude-3-haiku",
		Persona:  "Helpful assistant",
		Channels: set.NewOrdered[domain.ChannelName]("#general", "#dev"),
	}

	require.NoError(t, s.SaveInstance(ctx, inst))

	got, err := s.GetInstance(ctx, "claude")
	require.NoError(t, err)
	require.Equal(t, inst, got)
}

func TestFileStore_GetInstanceNotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.GetInstance(t.Context(), "ghost")
	require.Error(t, err)
}

func TestFileStore_DeleteInstance(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	inst := domain.ModelInstance{Nick: "temp", ModelID: "test/model"}
	require.NoError(t, s.SaveInstance(ctx, inst))
	require.NoError(t, s.DeleteInstance(ctx, "temp"))

	_, err := s.GetInstance(ctx, "temp")
	require.Error(t, err)
}

func TestFileStore_ListInstances(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	instances := []domain.ModelInstance{
		{Nick: "a", ModelID: "model/a"},
		{Nick: "b", ModelID: "model/b"},
	}

	for _, inst := range instances {
		require.NoError(t, s.SaveInstance(ctx, inst))
	}

	got, err := s.ListInstances(ctx)
	require.NoError(t, err)
	require.Equal(t, instances, got)
}

// --- Last channel state ---

func TestFileStore_GetLastChannelEmpty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.GetLastChannel(t.Context())
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestFileStore_SetAndGetLastChannel(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SetLastChannel(ctx, "#general"))

	got, err := s.GetLastChannel(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.ChannelName("#general"), got)
}

func TestFileStore_SetLastChannelOverwrites(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SetLastChannel(ctx, "#first"))
	require.NoError(t, s.SetLastChannel(ctx, "#second"))

	got, err := s.GetLastChannel(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.ChannelName("#second"), got)
}

func TestFileStore_GetLastReadEmpty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.GetLastRead(t.Context(), "#general")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestFileStore_SetAndGetLastRead(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SetLastRead(ctx, "#general", "msg-42"))

	got, err := s.GetLastRead(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, "msg-42", got)
}

func TestFileStore_SetLastRead_independent_per_channel(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SetLastRead(ctx, "#general", "msg-1"))
	require.NoError(t, s.SetLastRead(ctx, "#random", "msg-2"))

	g, err := s.GetLastRead(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, "msg-1", g)

	r, err := s.GetLastRead(ctx, "#random")
	require.NoError(t, err)
	require.Equal(t, "msg-2", r)
}

func TestFileStore_SetLastRead_overwrites(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SetLastRead(ctx, "#general", "msg-1"))
	require.NoError(t, s.SetLastRead(ctx, "#general", "msg-5"))

	got, err := s.GetLastRead(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, "msg-5", got)
}
