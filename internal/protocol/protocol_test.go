package protocol

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

func TestFromChannelEvent_join(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	got, ok := FromChannelEvent(domain.Join{
		Target: "#general",
		Nick:   "alice",
		At:     at,
	})

	require.True(t, ok)
	require.Equal(t, IRCMessage{
		Kind:   KindJoin,
		From:   "alice",
		Target: "#general",
		At:     at,
	}, got)
}

func TestFromChannelEvent_part(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	got, ok := FromChannelEvent(domain.Part{
		Target: "#general",
		Nick:   "alice",
		At:     at,
	})

	require.True(t, ok)
	require.Equal(t, IRCMessage{
		Kind:   KindPart,
		From:   "alice",
		Target: "#general",
		At:     at,
	}, got)
}

func TestFromChannelEvent_topic_change(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	got, ok := FromChannelEvent(domain.TopicChange{
		Target: "#general",
		Topic:  "Discussion",
		By:     "alice",
		At:     at,
	})

	require.True(t, ok)
	require.Equal(t, IRCMessage{
		Kind:   KindTopic,
		From:   "alice",
		Target: "#general",
		Body:   "Discussion",
		At:     at,
	}, got)
}

func TestFromChannelEvent_join_with_message(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	got, ok := FromChannelEvent(domain.Join{
		Target:  "#general",
		Nick:    "alice",
		Message: "hello everyone",
		At:      at,
	})

	require.True(t, ok)
	require.Equal(t, IRCMessage{
		Kind:   KindJoin,
		From:   "alice",
		Target: "#general",
		Body:   "hello everyone",
		At:     at,
	}, got)
}

func TestFromChannelEvent_part_with_message(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	got, ok := FromChannelEvent(domain.Part{
		Target:  "#general",
		Nick:    "alice",
		Message: "goodbye",
		At:      at,
	})

	require.True(t, ok)
	require.Equal(t, IRCMessage{
		Kind:   KindPart,
		From:   "alice",
		Target: "#general",
		Body:   "goodbye",
		At:     at,
	}, got)
}

func TestFromChannelEvent_quit(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	got, ok := FromChannelEvent(domain.Quit{
		Channels: []domain.ChannelName{"#general"},
		Nick:     "alice",
		Message:  "gone fishing",
		At:       at,
	})

	require.True(t, ok)
	require.Equal(t, IRCMessage{
		Kind: KindQuit,
		From: "alice",
		Body: "gone fishing",
		At:   at,
	}, got)
}

func TestFromChannelEvent_nick_change(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	got, ok := FromChannelEvent(domain.NickChange{
		OldNick: "alice",
		NewNick: "ally",
		At:      at,
	})

	require.True(t, ok)
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
