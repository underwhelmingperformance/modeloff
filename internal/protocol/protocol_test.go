package protocol

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

// TestFromChannelEvent covers the basic rendering for each channel
// event kind FromChannelEvent handles, with no actor InstanceID and
// no channel-specific fields beyond what the event itself carries.
// TestFromChannelEvent_propagates_instance_id below covers the
// InstanceID-carrying shape of the same events.
func TestFromChannelEvent(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		event domain.PersistableEvent
		want  IRCMessage
	}{
		{
			name:  "join",
			event: domain.Join{Source: domain.LegacyClientSource("alice"), Target: "#general", At: at},
			want:  IRCMessage{Kind: KindJoin, Source: domain.LegacyClientSource("alice"), Target: "#general", At: at},
		},
		{
			name:  "part",
			event: domain.Part{Source: domain.LegacyClientSource("alice"), Target: "#general", At: at},
			want:  IRCMessage{Kind: KindPart, Source: domain.LegacyClientSource("alice"), Target: "#general", At: at},
		},
		{
			name:  "topic_change",
			event: domain.TopicChange{Source: domain.LegacyClientSource("alice"), Target: "#general", Topic: "Discussion", At: at},
			want:  IRCMessage{Kind: KindTopic, Source: domain.LegacyClientSource("alice"), Target: "#general", Body: "Discussion", At: at},
		},
		{
			name:  "join_with_message",
			event: domain.Join{Source: domain.LegacyClientSource("alice"), Target: "#general", Message: "hello everyone", At: at},
			want:  IRCMessage{Kind: KindJoin, Source: domain.LegacyClientSource("alice"), Target: "#general", Body: "hello everyone", At: at},
		},
		{
			name:  "part_with_message",
			event: domain.Part{Source: domain.LegacyClientSource("alice"), Target: "#general", Message: "goodbye", At: at},
			want:  IRCMessage{Kind: KindPart, Source: domain.LegacyClientSource("alice"), Target: "#general", Body: "goodbye", At: at},
		},
		{
			name:  "quit",
			event: domain.Quit{Source: domain.LegacyClientSource("alice"), Message: "gone fishing", At: at},
			want:  IRCMessage{Kind: KindQuit, Source: domain.LegacyClientSource("alice"), Body: "gone fishing", At: at},
		},
		{
			name:  "nick_change",
			event: domain.NickChange{Source: domain.LegacyClientSource("alice"), NewNick: "ally", At: at},
			want:  IRCMessage{Kind: KindNick, Source: domain.LegacyClientSource("alice"), Target: "ally", At: at},
		},
		{
			name: "topic_info",
			event: domain.TopicInfo{
				Target:     "#general",
				Topic:      "Discussion",
				TopicSetBy: "alice",
				TopicSetAt: at.Add(-time.Hour),
				At:         at,
			},
			want: IRCMessage{
				Kind: KindServerReply, Source: domain.ServerSource("modeloff"),
				Target: "#general", Body: "topic #general: Discussion (set by alice at 2025-06-15T11:00:00Z)", At: at,
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
				Source: domain.ClientSource(selfID, "botty"), Target: "#room", At: at,
			},
			want: IRCMessage{
				Kind: KindJoin, Source: domain.ClientSource(selfID, "botty"), Target: "#room", At: at,
			},
		},
		{
			name: "part",
			event: domain.Part{
				Source: domain.ClientSource(selfID, "botty"), Target: "#room", Message: "afk", At: at,
			},
			want: IRCMessage{
				Kind: KindPart, Source: domain.ClientSource(selfID, "botty"),
				Target: "#room", Body: "afk", At: at,
			},
		},
		{
			name: "quit",
			event: domain.Quit{
				Source: domain.ClientSource(selfID, "botty"), Message: "bye", At: at,
			},
			want: IRCMessage{
				Kind: KindQuit, Source: domain.ClientSource(selfID, "botty"), Body: "bye", At: at,
			},
		},
		{
			name: "topic_change",
			event: domain.TopicChange{
				Source: domain.ClientSource(selfID, "botty"), Target: "#room", Topic: "new topic", At: at,
			},
			want: IRCMessage{
				Kind: KindTopic, Source: domain.ClientSource(selfID, "botty"),
				Target: "#room", Body: "new topic", At: at,
			},
		},
		{
			name: "nick_change",
			event: domain.NickChange{
				Source: domain.ClientSource(selfID, "botty"), NewNick: "botstronger", At: at,
			},
			want: IRCMessage{
				Kind: KindNick, Source: domain.ClientSource(selfID, "botty"), Target: "botstronger", At: at,
			},
		},
		{
			name: "invited carries the inviter (actor) as From/InstanceID",
			event: domain.Invited{
				Source: domain.ClientSource(selfID, "laney"), Target: "#room", Invitee: "botty", At: at,
			},
			want: IRCMessage{
				Kind: KindInvite, Source: domain.ClientSource(selfID, "laney"), Target: "#room", At: at,
			},
		},
		{
			name:  "inviting is the server confirmation to the issuer",
			event: domain.Inviting{Target: "#room", Invitee: "botty", At: at},
			want: IRCMessage{
				Kind: KindServerReply, Source: domain.ServerSource("modeloff"),
				Target: "#room", Body: "invited botty to #room", At: at,
			},
		},
		{
			name: "kicked carries the kicker as From/InstanceID and the kicked nick as Subject",
			event: domain.Kicked{
				Source: domain.ClientSource(selfID, "laney"), Target: "#room", Subject: "botty", At: at,
			},
			want: IRCMessage{
				Kind: KindKick, Source: domain.ClientSource(selfID, "laney"),
				Target: "#room", Subject: "botty", At: at,
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

// TestFromChannelEvent_channel_mode_change covers the MODE arm. A
// model granted `+v` in a `+m` channel only learns it may speak if
// the mode change is rendered into its prompt, so every shape of
// MODE has to render: a member mode puts the affected nick in
// `Subject`, an attribute mode leaves `Subject` empty, and a
// parametric attribute mode carries its value alongside the flag.
func TestFromChannelEvent_channel_mode_change(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		event domain.ChannelModeChange
		want  IRCMessage
	}{
		{
			name: "member mode grant",
			event: domain.ChannelModeChange{
				Source: domain.LegacyClientSource("laney"), Target: "#room", Subject: "botty",
				Flag: domain.ModeChannelVoice, Add: true, At: at,
			},
			want: IRCMessage{
				Kind: KindMode, Source: domain.LegacyClientSource("laney"),
				Target: "#room", Subject: "botty", Body: "+v", At: at,
			},
		},
		{
			name: "member mode revoke",
			event: domain.ChannelModeChange{
				Source: domain.LegacyClientSource("laney"), Target: "#room", Subject: "botty",
				Flag: domain.ModeOperator, Add: false, At: at,
			},
			want: IRCMessage{
				Kind: KindMode, Source: domain.LegacyClientSource("laney"),
				Target: "#room", Subject: "botty", Body: "-o", At: at,
			},
		},
		{
			name: "boolean attribute mode",
			event: domain.ChannelModeChange{
				Source: domain.LegacyClientSource("laney"), Target: "#room",
				Flag: domain.ModeModerated, Add: true, At: at,
			},
			want: IRCMessage{
				Kind: KindMode, Source: domain.LegacyClientSource("laney"),
				Target: "#room", Body: "+m", At: at,
			},
		},
		{
			name: "parametric attribute mode",
			event: domain.ChannelModeChange{
				Source: domain.LegacyClientSource("laney"), Target: "#room",
				Flag: domain.ModeUserLimit, Add: true, Param: "20", At: at,
			},
			want: IRCMessage{
				Kind: KindMode, Source: domain.LegacyClientSource("laney"),
				Target: "#room", Body: "+l 20", At: at,
			},
		},
		{
			name: "server-issued change has no actor",
			event: domain.ChannelModeChange{
				Source: domain.ServerSource(""), Target: "#room",
				Flag: domain.ModeNoExternal, Add: true, At: at,
			},
			want: IRCMessage{
				Kind: KindMode, Source: domain.ServerSource(""), Target: "#room", Body: "+n", At: at,
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
	fg := ReplyPaletteIndex(4)
	outOfRange := ReplyPaletteIndex(16)

	tests := []struct {
		name    string
		part    ReplyPart
		wantErr error
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
			wantErr: ReplyPartShapeError{HasBody: true, HasSpans: true},
		},
		{
			name:    "rejects newline in body",
			part:    ReplyPart{Kind: ReplyMessage, Body: "line one\nline two"},
			wantErr: domain.InvalidMessageBodyError{Command: "PRIVMSG"},
		},
		{
			name:    "rejects carriage return in body",
			part:    ReplyPart{Kind: ReplyMessage, Body: "line one\rline two"},
			wantErr: domain.InvalidMessageBodyError{Command: "PRIVMSG"},
		},
		{
			name:    "rejects NUL in body",
			part:    ReplyPart{Kind: ReplyMessage, Body: "before\x00after"},
			wantErr: domain.InvalidMessageBodyError{Command: "PRIVMSG"},
		},
		{
			name:    "rejects empty span",
			part:    ReplyPart{Spans: []ReplySpan{{Text: ""}}},
			wantErr: EmptyReplySpanError{Index: 0},
		},
		{
			name:    "rejects newline in span",
			part:    ReplyPart{Spans: []ReplySpan{{Text: "line one\nline two"}}},
			wantErr: InvalidReplySpanTextError{Index: 0},
		},
		{
			name:    "rejects carriage return in span",
			part:    ReplyPart{Spans: []ReplySpan{{Text: "line one\rline two"}}},
			wantErr: InvalidReplySpanTextError{Index: 0},
		},
		{
			name:    "rejects NUL in span",
			part:    ReplyPart{Spans: []ReplySpan{{Text: "before\x00after"}}},
			wantErr: InvalidReplySpanTextError{Index: 0},
		},
		{
			name:    "rejects out-of-range foreground colour",
			part:    ReplyPart{Spans: []ReplySpan{{Text: "hello", Style: &ReplyStyle{FG: &outOfRange}}}},
			wantErr: ReplyColourOutOfRangeError{Index: 0, Colour: ReplyForeground, Value: 16},
		},
		{
			name:    "rejects out-of-range background colour",
			part:    ReplyPart{Spans: []ReplySpan{{Text: "hello", Style: &ReplyStyle{BG: &outOfRange}}}},
			wantErr: ReplyColourOutOfRangeError{Index: 0, Colour: ReplyBackground, Value: 16},
		},
		{
			name:    "rejects missing body and spans",
			part:    ReplyPart{},
			wantErr: ReplyPartShapeError{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateReplyPart(tc.part)
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.Equal(t, tc.wantErr, err)
		})
	}
}

func TestReplyPaletteIndex_rejects_invalid_JSON_integers_with_a_typed_error(t *testing.T) {
	tests := []struct {
		raw  string
		want *InvalidReplyColourError
	}{
		{raw: "4.5", want: &InvalidReplyColourError{Value: "4.5"}},
		{raw: "16", want: &InvalidReplyColourError{Value: "16"}},
		{raw: "-1", want: &InvalidReplyColourError{Value: "-1"}},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			var colour ReplyPaletteIndex
			require.Equal(t, tt.want, colour.UnmarshalJSON([]byte(tt.raw)))
		})
	}
}

func TestValidateMessageBody(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		body    string
		wantErr error
	}{
		{name: "text", body: "hello"},
		{name: "whitespace", body: " \t "},
		{name: "IRC formatting controls", body: "\x01ACTION waves\x01 \x02bold\x02"},
		{name: "escape", body: "\x1b[31mred"},
		{
			name:    "empty",
			wantErr: domain.NoTextToSendError{Command: "PRIVMSG", At: at},
		},
		{
			name:    "NUL",
			body:    "before\x00after",
			wantErr: domain.InvalidMessageBodyError{Command: "PRIVMSG", At: at},
		},
		{
			name:    "carriage return",
			body:    "before\rafter",
			wantErr: domain.InvalidMessageBodyError{Command: "PRIVMSG", At: at},
		},
		{
			name:    "newline",
			body:    "before\nafter",
			wantErr: domain.InvalidMessageBodyError{Command: "PRIVMSG", At: at},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMessageBody("PRIVMSG", tc.body, at)
			require.Equal(t, tc.wantErr, err)
		})
	}
}
