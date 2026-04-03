package store

import (
	"context"
	"testing"
	"time"

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
	if err != nil {
		t.Fatalf("ListRooms() error: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("ListRooms() returned %d rooms, want 0", len(got))
	}
}

func TestFileStore_SaveAndGetRoom(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	room := domain.Room{
		Name:    "¢general",
		Kind:    domain.RoomChannel,
		Title:   "General chat",
		Members: []domain.Nick{"alice", "bob"},
		Created: testTime,
	}

	if err := s.SaveRoom(ctx, room); err != nil {
		t.Fatalf("SaveRoom() error: %v", err)
	}

	got, err := s.GetRoom(ctx, "¢general")
	if err != nil {
		t.Fatalf("GetRoom() error: %v", err)
	}

	assertRoom(t, got, room)
}

func TestFileStore_GetRoomNotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.GetRoom(context.Background(), "¢nonexistent")
	if err == nil {
		t.Fatal("GetRoom() expected error for missing room, got nil")
	}
}

func TestFileStore_ListRooms(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	rooms := []domain.Room{
		{Name: "¢alpha", Kind: domain.RoomChannel, Created: testTime},
		{Name: "¢beta", Kind: domain.RoomChannel, Created: testTime.Add(time.Hour)},
	}

	for _, r := range rooms {
		if err := s.SaveRoom(ctx, r); err != nil {
			t.Fatalf("SaveRoom(%q) error: %v", r.Name, err)
		}
	}

	got, err := s.ListRooms(ctx)
	if err != nil {
		t.Fatalf("ListRooms() error: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("ListRooms() returned %d rooms, want 2", len(got))
	}
}

func TestFileStore_DeleteRoom(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	room := domain.Room{Name: "¢deleteme", Kind: domain.RoomChannel, Created: testTime}
	if err := s.SaveRoom(ctx, room); err != nil {
		t.Fatalf("SaveRoom() error: %v", err)
	}

	if err := s.DeleteRoom(ctx, "¢deleteme"); err != nil {
		t.Fatalf("DeleteRoom() error: %v", err)
	}

	_, err := s.GetRoom(ctx, "¢deleteme")
	if err == nil {
		t.Fatal("GetRoom() expected error after delete, got nil")
	}
}

func TestFileStore_SaveRoomOverwrites(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	room := domain.Room{Name: "¢evolving", Kind: domain.RoomChannel, Created: testTime}
	if err := s.SaveRoom(ctx, room); err != nil {
		t.Fatalf("SaveRoom() error: %v", err)
	}

	room.Title = "Updated title"
	room.Members = []domain.Nick{"charlie"}
	if err := s.SaveRoom(ctx, room); err != nil {
		t.Fatalf("SaveRoom() error: %v", err)
	}

	got, err := s.GetRoom(ctx, "¢evolving")
	if err != nil {
		t.Fatalf("GetRoom() error: %v", err)
	}

	assertRoom(t, got, room)
}

// --- Messages ---

func TestFileStore_ListMessagesEmpty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.ListMessages(context.Background(), "¢empty")
	if err != nil {
		t.Fatalf("ListMessages() error: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("ListMessages() returned %d messages, want 0", len(got))
	}
}

func TestFileStore_SaveAndListMessages(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	msgs := []domain.Message{
		{ID: "msg-1", Room: "¢general", From: "alice", Body: "hello", SentAt: testTime},
		{ID: "msg-2", Room: "¢general", From: "bob", Body: "hi", SentAt: testTime.Add(time.Second)},
	}

	for _, m := range msgs {
		if err := s.SaveMessage(ctx, m); err != nil {
			t.Fatalf("SaveMessage(%q) error: %v", m.ID, err)
		}
	}

	got, err := s.ListMessages(ctx, "¢general")
	if err != nil {
		t.Fatalf("ListMessages() error: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("ListMessages() returned %d messages, want 2", len(got))
	}

	for i, want := range msgs {
		if got[i] != want {
			t.Errorf("message[%d] = %+v, want %+v", i, got[i], want)
		}
	}
}

func TestFileStore_MessagesIsolatedByRoom(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.SaveMessage(ctx, domain.Message{ID: "a", Room: "¢room-a", From: "alice", Body: "a msg", SentAt: testTime}); err != nil {
		t.Fatalf("SaveMessage() error: %v", err)
	}

	if err := s.SaveMessage(ctx, domain.Message{ID: "b", Room: "¢room-b", From: "bob", Body: "b msg", SentAt: testTime}); err != nil {
		t.Fatalf("SaveMessage() error: %v", err)
	}

	gotA, err := s.ListMessages(ctx, "¢room-a")
	if err != nil {
		t.Fatalf("ListMessages(room-a) error: %v", err)
	}

	if len(gotA) != 1 || gotA[0].ID != "a" {
		t.Errorf("room-a messages = %+v, want [msg a]", gotA)
	}

	gotB, err := s.ListMessages(ctx, "¢room-b")
	if err != nil {
		t.Fatalf("ListMessages(room-b) error: %v", err)
	}

	if len(gotB) != 1 || gotB[0].ID != "b" {
		t.Errorf("room-b messages = %+v, want [msg b]", gotB)
	}
}

// --- Model instances ---

func TestFileStore_ListInstancesEmpty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.ListInstances(context.Background())
	if err != nil {
		t.Fatalf("ListInstances() error: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("ListInstances() returned %d instances, want 0", len(got))
	}
}

func TestFileStore_SaveAndGetInstance(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	inst := domain.ModelInstance{
		Nick:    "claude",
		ModelID: "anthropic/claude-3-haiku",
		Persona: "Helpful assistant",
		Rooms:   []domain.RoomName{"¢general", "¢dev"},
	}

	if err := s.SaveInstance(ctx, inst); err != nil {
		t.Fatalf("SaveInstance() error: %v", err)
	}

	got, err := s.GetInstance(ctx, "claude")
	if err != nil {
		t.Fatalf("GetInstance() error: %v", err)
	}

	assertInstance(t, got, inst)
}

func TestFileStore_GetInstanceNotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.GetInstance(context.Background(), "ghost")
	if err == nil {
		t.Fatal("GetInstance() expected error for missing instance, got nil")
	}
}

func TestFileStore_DeleteInstance(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	inst := domain.ModelInstance{Nick: "temp", ModelID: "test/model"}
	if err := s.SaveInstance(ctx, inst); err != nil {
		t.Fatalf("SaveInstance() error: %v", err)
	}

	if err := s.DeleteInstance(ctx, "temp"); err != nil {
		t.Fatalf("DeleteInstance() error: %v", err)
	}

	_, err := s.GetInstance(ctx, "temp")
	if err == nil {
		t.Fatal("GetInstance() expected error after delete, got nil")
	}
}

func TestFileStore_ListInstances(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	instances := []domain.ModelInstance{
		{Nick: "a", ModelID: "model/a"},
		{Nick: "b", ModelID: "model/b"},
	}

	for _, inst := range instances {
		if err := s.SaveInstance(ctx, inst); err != nil {
			t.Fatalf("SaveInstance(%q) error: %v", inst.Nick, err)
		}
	}

	got, err := s.ListInstances(ctx)
	if err != nil {
		t.Fatalf("ListInstances() error: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("ListInstances() returned %d instances, want 2", len(got))
	}
}

// --- Last room state ---

func TestFileStore_GetLastRoomEmpty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.GetLastRoom(context.Background())
	if err != nil {
		t.Fatalf("GetLastRoom() error: %v", err)
	}

	if got != "" {
		t.Errorf("GetLastRoom() = %q, want empty", got)
	}
}

func TestFileStore_SetAndGetLastRoom(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.SetLastRoom(ctx, "¢general"); err != nil {
		t.Fatalf("SetLastRoom() error: %v", err)
	}

	got, err := s.GetLastRoom(ctx)
	if err != nil {
		t.Fatalf("GetLastRoom() error: %v", err)
	}

	if got != "¢general" {
		t.Errorf("GetLastRoom() = %q, want %q", got, "¢general")
	}
}

func TestFileStore_SetLastRoomOverwrites(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.SetLastRoom(ctx, "¢first"); err != nil {
		t.Fatalf("SetLastRoom() error: %v", err)
	}

	if err := s.SetLastRoom(ctx, "¢second"); err != nil {
		t.Fatalf("SetLastRoom() error: %v", err)
	}

	got, err := s.GetLastRoom(ctx)
	if err != nil {
		t.Fatalf("GetLastRoom() error: %v", err)
	}

	if got != "¢second" {
		t.Errorf("GetLastRoom() = %q, want %q", got, "¢second")
	}
}

// --- Helpers ---

func assertRoom(t *testing.T, got, want domain.Room) {
	t.Helper()

	if got.Name != want.Name {
		t.Errorf("Name = %q, want %q", got.Name, want.Name)
	}

	if got.Kind != want.Kind {
		t.Errorf("Kind = %d, want %d", got.Kind, want.Kind)
	}

	if got.Title != want.Title {
		t.Errorf("Title = %q, want %q", got.Title, want.Title)
	}

	if !got.Created.Equal(want.Created) {
		t.Errorf("Created = %v, want %v", got.Created, want.Created)
	}

	if len(got.Members) != len(want.Members) {
		t.Errorf("Members length = %d, want %d", len(got.Members), len(want.Members))
		return
	}

	for i, m := range want.Members {
		if got.Members[i] != m {
			t.Errorf("Members[%d] = %q, want %q", i, got.Members[i], m)
		}
	}
}

func assertInstance(t *testing.T, got, want domain.ModelInstance) {
	t.Helper()

	if got.Nick != want.Nick {
		t.Errorf("Nick = %q, want %q", got.Nick, want.Nick)
	}

	if got.ModelID != want.ModelID {
		t.Errorf("ModelID = %q, want %q", got.ModelID, want.ModelID)
	}

	if got.Persona != want.Persona {
		t.Errorf("Persona = %q, want %q", got.Persona, want.Persona)
	}

	if len(got.Rooms) != len(want.Rooms) {
		t.Errorf("Rooms length = %d, want %d", len(got.Rooms), len(want.Rooms))
		return
	}

	for i, r := range want.Rooms {
		if got.Rooms[i] != r {
			t.Errorf("Rooms[%d] = %q, want %q", i, got.Rooms[i], r)
		}
	}
}
