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
		Nick:    "alice",
		Message: "gone fishing",
		At:      at,
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

// TestFromChannelEvent_propagates_instance_id pins that every
// channel event with an actor InstanceID carries it through into
// the resulting IRCMessage. Without it `buildMessages` files the
// bot's own JOIN/PART/TOPIC events as user-role messages and the
// model reads them as someone else's actions.
func TestFromChannelEvent_propagates_instance_id(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	const selfID = domain.InstanceID("inst-self")

	tests := []struct {
		name  string
		event domain.PersistableEvent
		want  IRCMessage
	}{
		{
			name: "join",
			event: domain.Join{
				Target:     "#room",
				Nick:       "botty",
				InstanceID: selfID,
				At:         at,
			},
			want: IRCMessage{
				Kind:       KindJoin,
				From:       "botty",
				InstanceID: selfID,
				Target:     "#room",
				At:         at,
			},
		},
		{
			name: "part",
			event: domain.Part{
				Target:     "#room",
				Nick:       "botty",
				InstanceID: selfID,
				Message:    "afk",
				At:         at,
			},
			want: IRCMessage{
				Kind:       KindPart,
				From:       "botty",
				InstanceID: selfID,
				Target:     "#room",
				Body:       "afk",
				At:         at,
			},
		},
		{
			name: "quit",
			event: domain.Quit{
				Nick:       "botty",
				InstanceID: selfID,
				Message:    "bye",
				At:         at,
			},
			want: IRCMessage{
				Kind:       KindQuit,
				From:       "botty",
				InstanceID: selfID,
				Body:       "bye",
				At:         at,
			},
		},
		{
			name: "topic_change",
			event: domain.TopicChange{
				Target:     "#room",
				Topic:      "new topic",
				By:         "botty",
				InstanceID: selfID,
				At:         at,
			},
			want: IRCMessage{
				Kind:       KindTopic,
				From:       "botty",
				InstanceID: selfID,
				Target:     "#room",
				Body:       "new topic",
				At:         at,
			},
		},
		{
			name: "nick_change",
			event: domain.NickChange{
				OldNick:    "botty",
				NewNick:    "botstronger",
				InstanceID: selfID,
				At:         at,
			},
			want: IRCMessage{
				Kind:       KindNick,
				From:       "botty",
				InstanceID: selfID,
				Target:     "botstronger",
				At:         at,
			},
		},
		{
			name: "model_invited",
			event: domain.ModelInvited{
				Target:     "#room",
				Nick:       "botty",
				InstanceID: selfID,
				By:         "laney",
				At:         at,
			},
			want: IRCMessage{
				Kind:       KindInvite,
				From:       "botty",
				InstanceID: selfID,
				Target:     "#room",
				At:         at,
			},
		},
		{
			name: "model_kicked",
			event: domain.ModelKicked{
				Target:     "#room",
				Nick:       "botty",
				InstanceID: selfID,
				By:         "laney",
				At:         at,
			},
			want: IRCMessage{
				Kind:       KindKick,
				From:       "botty",
				InstanceID: selfID,
				Target:     "#room",
				At:         at,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := FromChannelEvent(tc.event)
			require.True(t, ok)
			require.Equal(t, tc.want, got)
		})
	}
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
