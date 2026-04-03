package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

var testTime = time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)

func newTestStore(t *testing.T) *FileStore {
	t.Helper()
	return NewFileStore(t.TempDir())
}

// --- Rooms ---

func TestFileStore_ListRoomsEmpty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.ListRooms(context.Background())
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestFileStore_SaveAndGetRoom(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	room := domain.Room{
		Name:    "#general",
		Kind:    domain.RoomChannel,
		Title:   "General chat",
		Members: []domain.Nick{"alice", "bob"},
		Created: testTime,
	}

	require.NoError(t, s.SaveRoom(ctx, room))

	got, err := s.GetRoom(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, room, got)
}

func TestFileStore_GetRoomNotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.GetRoom(context.Background(), "#nonexistent")
	require.Error(t, err)
}

func TestFileStore_ListRooms(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	rooms := []domain.Room{
		{Name: "#alpha", Kind: domain.RoomChannel, Created: testTime},
		{Name: "#beta", Kind: domain.RoomChannel, Created: testTime.Add(time.Hour)},
	}

	for _, r := range rooms {
		require.NoError(t, s.SaveRoom(ctx, r))
	}

	got, err := s.ListRooms(ctx)
	require.NoError(t, err)
	require.Equal(t, rooms, got)
}

func TestFileStore_DeleteRoom(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	room := domain.Room{Name: "#deleteme", Kind: domain.RoomChannel, Created: testTime}
	require.NoError(t, s.SaveRoom(ctx, room))
	require.NoError(t, s.DeleteRoom(ctx, "#deleteme"))

	_, err := s.GetRoom(ctx, "#deleteme")
	require.Error(t, err)
}

func TestFileStore_SaveRoomOverwrites(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	room := domain.Room{Name: "#evolving", Kind: domain.RoomChannel, Created: testTime}
	require.NoError(t, s.SaveRoom(ctx, room))

	room.Title = "Updated title"
	room.Members = []domain.Nick{"charlie"}
	require.NoError(t, s.SaveRoom(ctx, room))

	got, err := s.GetRoom(ctx, "#evolving")
	require.NoError(t, err)
	require.Equal(t, room, got)
}

// --- Messages ---

func TestFileStore_ListMessagesEmpty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.ListMessages(context.Background(), "#empty")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestFileStore_SaveAndListMessages(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	msgs := []domain.Message{
		{ID: "msg-1", Room: "#general", From: "alice", Body: "hello", SentAt: testTime},
		{ID: "msg-2", Room: "#general", From: "bob", Body: "hi", SentAt: testTime.Add(time.Second)},
	}

	for _, m := range msgs {
		require.NoError(t, s.SaveMessage(ctx, m))
	}

	got, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, msgs, got)
}

func TestFileStore_MessagesIsolatedByRoom(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	require.NoError(t, s.SaveMessage(ctx, domain.Message{ID: "a", Room: "#room-a", From: "alice", Body: "a msg", SentAt: testTime}))
	require.NoError(t, s.SaveMessage(ctx, domain.Message{ID: "b", Room: "#room-b", From: "bob", Body: "b msg", SentAt: testTime}))

	gotA, err := s.ListMessages(ctx, "#room-a")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{
		{ID: "a", Room: "#room-a", From: "alice", Body: "a msg", SentAt: testTime},
	}, gotA)

	gotB, err := s.ListMessages(ctx, "#room-b")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{
		{ID: "b", Room: "#room-b", From: "bob", Body: "b msg", SentAt: testTime},
	}, gotB)
}

// --- Model instances ---

func TestFileStore_ListInstancesEmpty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.ListInstances(context.Background())
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestFileStore_SaveAndGetInstance(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	inst := domain.ModelInstance{
		Nick:    "claude",
		ModelID: "anthropic/claude-3-haiku",
		Persona: "Helpful assistant",
		Rooms:   []domain.RoomName{"#general", "#dev"},
	}

	require.NoError(t, s.SaveInstance(ctx, inst))

	got, err := s.GetInstance(ctx, "claude")
	require.NoError(t, err)
	require.Equal(t, inst, got)
}

func TestFileStore_GetInstanceNotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.GetInstance(context.Background(), "ghost")
	require.Error(t, err)
}

func TestFileStore_DeleteInstance(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	inst := domain.ModelInstance{Nick: "temp", ModelID: "test/model"}
	require.NoError(t, s.SaveInstance(ctx, inst))
	require.NoError(t, s.DeleteInstance(ctx, "temp"))

	_, err := s.GetInstance(ctx, "temp")
	require.Error(t, err)
}

func TestFileStore_ListInstances(t *testing.T) {
	ctx := context.Background()
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

// --- Last room state ---

func TestFileStore_GetLastRoomEmpty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.GetLastRoom(context.Background())
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestFileStore_SetAndGetLastRoom(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	require.NoError(t, s.SetLastRoom(ctx, "#general"))

	got, err := s.GetLastRoom(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.RoomName("#general"), got)
}

func TestFileStore_SetLastRoomOverwrites(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	require.NoError(t, s.SetLastRoom(ctx, "#first"))
	require.NoError(t, s.SetLastRoom(ctx, "#second"))

	got, err := s.GetLastRoom(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.RoomName("#second"), got)
}
