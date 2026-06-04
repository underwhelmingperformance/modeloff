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
			name: "model_invited carries the inviter (actor) as From/InstanceID",
			event: domain.ModelInvited{
				Target:       "#room",
				Nick:         "botty",
				InstanceID:   "inst-botty",
				By:           "laney",
				ByInstanceID: selfID,
				At:           at,
			},
			want: IRCMessage{
				Kind:       KindInvite,
				From:       "laney",
				InstanceID: selfID,
				Target:     "#room",
				At:         at,
			},
		},
		{
			name: "model_kicked carries the kicker as From/InstanceID and the kicked nick as Subject",
			event: domain.ModelKicked{
				Target:       "#room",
				Nick:         "botty",
				InstanceID:   "inst-botty",
				By:           "laney",
				ByInstanceID: selfID,
				At:           at,
			},
			want: IRCMessage{
				Kind:       KindKick,
				From:       "laney",
				InstanceID: selfID,
				Target:     "#room",
				Subject:    "botty",
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
	fg := uint8(4)
	outOfRange := uint8(16)

	tests := []struct {
		name    string
		part    ReplyPart
		wantErr string
	}{
		{
			name: "valid body",
			part: ReplyPart{Kind: ReplyMessage, Body: "hello world"},
		},
		{
			name: "valid spans",
			part: ReplyPart{
				Kind: ReplyMessage,
				Spans: []ReplySpan{
					{Text: "hello "},
					{Text: "world", Style: &ReplyStyle{Bold: true, FG: &fg}},
				},
			},
		},
		{
			name:    "rejects body and spans together",
			part:    ReplyPart{Body: "hello", Spans: []ReplySpan{{Text: "world"}}},
			wantErr: "exactly one of body or spans",
		},
		{
			name:    "rejects newline in body",
			part:    ReplyPart{Kind: ReplyMessage, Body: "line one\nline two"},
			wantErr: "reply body must not contain newlines",
		},
		{
			name:    "rejects empty span",
			part:    ReplyPart{Spans: []ReplySpan{{Text: ""}}},
			wantErr: "span 0 is empty",
		},
		{
			name:    "rejects newline in span",
			part:    ReplyPart{Spans: []ReplySpan{{Text: "line one\nline two"}}},
			wantErr: "span 0 contains a newline",
		},
		{
			name:    "rejects out-of-range foreground colour",
			part:    ReplyPart{Spans: []ReplySpan{{Text: "hello", Style: &ReplyStyle{FG: &outOfRange}}}},
			wantErr: "foreground colour 16 is out of range",
		},
		{
			name:    "rejects out-of-range background colour",
			part:    ReplyPart{Spans: []ReplySpan{{Text: "hello", Style: &ReplyStyle{BG: &outOfRange}}}},
			wantErr: "background colour 16 is out of range",
		},
		{
			name:    "rejects missing body and spans",
			part:    ReplyPart{},
			wantErr: "exactly one of body or spans",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateReplyPart(tc.part)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}
