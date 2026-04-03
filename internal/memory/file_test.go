package memory

import (
	"context"
	"testing"

	"github.com/laney/modeloff/internal/domain"
)

func TestFileStore_ReadEmpty(t *testing.T) {
	store := NewFileStore(t.TempDir())

	got, err := store.Read(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("Read() returned %d entries, want 0", len(got))
	}
}

func TestFileStore_WriteAndRead(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	nick := domain.Nick("bob")

	entries := []Entry{
		{Key: "greeting", Content: "Hello, I like cats."},
		{Key: "preference", Content: "Prefers formal tone."},
	}

	for _, e := range entries {
		if err := store.Write(ctx, nick, e); err != nil {
			t.Fatalf("Write(%q) error: %v", e.Key, err)
		}
	}

	got, err := store.Read(ctx, nick)
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}

	if len(got) != len(entries) {
		t.Fatalf("Read() returned %d entries, want %d", len(got), len(entries))
	}

	for i, want := range entries {
		if got[i] != want {
			t.Errorf("entry[%d] = %+v, want %+v", i, got[i], want)
		}
	}
}

func TestFileStore_WriteOverwritesExistingKey(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	nick := domain.Nick("charlie")

	if err := store.Write(ctx, nick, Entry{Key: "mood", Content: "happy"}); err != nil {
		t.Fatalf("Write() error: %v", err)
	}

	if err := store.Write(ctx, nick, Entry{Key: "mood", Content: "excited"}); err != nil {
		t.Fatalf("Write() error: %v", err)
	}

	got, err := store.Read(ctx, nick)
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("Read() returned %d entries, want 1", len(got))
	}

	want := Entry{Key: "mood", Content: "excited"}
	if got[0] != want {
		t.Errorf("entry = %+v, want %+v", got[0], want)
	}
}

func TestFileStore_Delete(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	nick := domain.Nick("dave")

	entries := []Entry{
		{Key: "first", Content: "one"},
		{Key: "second", Content: "two"},
		{Key: "third", Content: "three"},
	}

	for _, e := range entries {
		if err := store.Write(ctx, nick, e); err != nil {
			t.Fatalf("Write(%q) error: %v", e.Key, err)
		}
	}

	if err := store.Delete(ctx, nick, "second"); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}

	got, err := store.Read(ctx, nick)
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}

	want := []Entry{
		{Key: "first", Content: "one"},
		{Key: "third", Content: "three"},
	}

	if len(got) != len(want) {
		t.Fatalf("Read() returned %d entries, want %d", len(got), len(want))
	}

	for i, w := range want {
		if got[i] != w {
			t.Errorf("entry[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

func TestFileStore_DeleteNonexistent(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())

	err := store.Delete(ctx, "eve", "nonexistent")
	if err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
}

func TestFileStore_IsolationBetweenNicks(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())

	if err := store.Write(ctx, "nick-a", Entry{Key: "k", Content: "from-a"}); err != nil {
		t.Fatalf("Write() error: %v", err)
	}

	if err := store.Write(ctx, "nick-b", Entry{Key: "k", Content: "from-b"}); err != nil {
		t.Fatalf("Write() error: %v", err)
	}

	gotA, err := store.Read(ctx, "nick-a")
	if err != nil {
		t.Fatalf("Read(nick-a) error: %v", err)
	}

	gotB, err := store.Read(ctx, "nick-b")
	if err != nil {
		t.Fatalf("Read(nick-b) error: %v", err)
	}

	if len(gotA) != 1 || gotA[0].Content != "from-a" {
		t.Errorf("nick-a entries = %+v, want [{Key:k Content:from-a}]", gotA)
	}

	if len(gotB) != 1 || gotB[0].Content != "from-b" {
		t.Errorf("nick-b entries = %+v, want [{Key:k Content:from-b}]", gotB)
	}
}
