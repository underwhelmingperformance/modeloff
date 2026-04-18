package protocol

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

func TestFromJoinEvent(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	alice := domain.NewModelInstance("inst-alice", "alice", "test/model", "", nil)

	got := FromJoinEvent(domain.JoinEvent{
		Channel:  "#general",
		Instance: alice,
		At:       at,
	})

	require.Equal(t, IRCMessage{
		Kind:   KindJoin,
		From:   "alice",
		Target: "#general",
		At:     at,
	}, got)
}

func TestFromPartEvent(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	alice := domain.NewModelInstance("inst-alice", "alice", "test/model", "", nil)

	got := FromPartEvent(domain.PartEvent{
		Channel:  "#general",
		Instance: alice,
		At:       at,
	})

	require.Equal(t, IRCMessage{
		Kind:   KindPart,
		From:   "alice",
		Target: "#general",
		At:     at,
	}, got)
}

func TestFromTopicChangeEvent(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	got := FromTopicChangeEvent(domain.TopicChangeEvent{
		Channel: "#general",
		Topic:   "Discussion",
		By:      "alice",
		At:      at,
	})

	require.Equal(t, IRCMessage{
		Kind:   KindTopic,
		From:   "alice",
		Target: "#general",
		Body:   "Discussion",
		At:     at,
	}, got)
}

func TestFromJoinEvent_with_message(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	alice := domain.NewModelInstance("inst-alice", "alice", "test/model", "", nil)

	got := FromJoinEvent(domain.JoinEvent{
		Channel:  "#general",
		Instance: alice,
		Message:  "hello everyone",
		At:       at,
	})

	require.Equal(t, IRCMessage{
		Kind:   KindJoin,
		From:   "alice",
		Target: "#general",
		Body:   "hello everyone",
		At:     at,
	}, got)
}

func TestFromPartEvent_with_message(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	alice := domain.NewModelInstance("inst-alice", "alice", "test/model", "", nil)

	got := FromPartEvent(domain.PartEvent{
		Channel:  "#general",
		Instance: alice,
		Message:  "goodbye",
		At:       at,
	})

	require.Equal(t, IRCMessage{
		Kind:   KindPart,
		From:   "alice",
		Target: "#general",
		Body:   "goodbye",
		At:     at,
	}, got)
}

func TestFromQuitEvent(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	alice := domain.NewModelInstance("inst-alice", "alice", "test/model", "", nil)

	got := FromQuitEvent(domain.QuitEvent{
		Instance: alice,
		Message:  "gone fishing",
		At:       at,
	})

	require.Equal(t, IRCMessage{
		Kind: KindQuit,
		From: "alice",
		Body: "gone fishing",
		At:   at,
	}, got)
}

func TestFromNickChangeEvent(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	got := FromNickChangeEvent(domain.NickChangeEvent{
		OldNick: "alice",
		NewNick: "ally",
		At:      at,
	})

	require.Equal(t, IRCMessage{
		Kind:   KindNick,
		From:   "alice",
		Target: "ally",
		At:     at,
	}, got)
}

func TestValidateReplyPart(t *testing.T) {
	t.Run("valid body", func(t *testing.T) {
		err := ValidateReplyPart(ReplyPart{
			Kind: ReplyMessage,
			Body: "hello world",
		})

		require.NoError(t, err)
	})

	t.Run("valid spans", func(t *testing.T) {
		fg := uint8(4)
		err := ValidateReplyPart(ReplyPart{
			Kind: ReplyMessage,
			Spans: []ReplySpan{
				{Text: "hello "},
				{Text: "world", Style: &ReplyStyle{Bold: true, FG: &fg}},
			},
		})

		require.NoError(t, err)
	})

	t.Run("rejects body and spans together", func(t *testing.T) {
		err := ValidateReplyPart(ReplyPart{
			Body: "hello",
			Spans: []ReplySpan{
				{Text: "world"},
			},
		})

		require.ErrorContains(t, err, "exactly one of body or spans")
	})

	t.Run("rejects empty span", func(t *testing.T) {
		err := ValidateReplyPart(ReplyPart{
			Spans: []ReplySpan{
				{Text: ""},
			},
		})

		require.ErrorContains(t, err, "span 0 is empty")
	})

	t.Run("rejects invalid style colour", func(t *testing.T) {
		fg := uint8(16)
		err := ValidateReplyPart(ReplyPart{
			Spans: []ReplySpan{
				{Text: "hello", Style: &ReplyStyle{FG: &fg}},
			},
		})

		require.ErrorContains(t, err, "foreground colour 16 is out of range")
	})

	t.Run("rejects missing body and spans", func(t *testing.T) {
		err := ValidateReplyPart(ReplyPart{})

		require.ErrorContains(t, err, "exactly one of body or spans")
	})
}
