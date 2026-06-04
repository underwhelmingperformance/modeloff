package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

func TestChannelEvent_JSON_round_trip(t *testing.T) {
	ts := time.Date(2026, 4, 6, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		event domain.PersistableEvent
	}{
		{
			name: "message",
			event: domain.Message{
				Target: "#general",
				From:   "alice",
				Body:   "hello world",
				At:     ts,
			},
		},
		{
			name: "action message",
			event: domain.Message{
				Target: "#general",
				From:   "alice",
				Body:   "waves",
				Action: true,
				At:     ts,
			},
		},
		{
			name: "join",
			event: domain.Join{
				Target: "#general",
				Nick:   "bob",
				At:     ts,
			},
		},
		{
			name: "join with created",
			event: domain.Join{
				Target:  "#new",
				Nick:    "bob",
				Created: true,
				At:      ts,
			},
		},
		{
			name: "join with message",
			event: domain.Join{
				Target:  "#general",
				Nick:    "bob",
				Message: "hello everyone",
				At:      ts,
			},
		},
		{
			name: "part",
			event: domain.Part{
				Target: "#general",
				Nick:   "bob",
				At:     ts,
			},
		},
		{
			name: "part with message",
			event: domain.Part{
				Target:  "#general",
				Nick:    "bob",
				Message: "see ya later",
				At:      ts,
			},
		},
		{
			name: "quit",
			event: domain.Quit{
				Nick:    "bob",
				Message: "gone fishing",
				At:      ts,
			},
		},
		{
			name: "quit without message",
			event: domain.Quit{
				Nick: "bob",
				At:   ts,
			},
		},
		{
			name: "topic change",
			event: domain.TopicChange{
				Target: "#general",
				Topic:  "new topic",
				By:     "alice",
				At:     ts,
			},
		},
		{
			name: "mode change",
			event: domain.ChannelModeChange{
				Target: "#general",
				Nick:   "bob",
				Flag:   domain.ModeChannelVoice, Add: true,
				By: "ChanServ",
				At: ts,
			},
		},
		{
			name: "model invited",
			event: domain.ModelInvited{
				Target: "#general",
				Nick:   "botty",
				By:     "alice",
				At:     ts,
			},
		},
		{
			name: "model kicked",
			event: domain.ModelKicked{
				Target: "#general",
				Nick:   "botty",
				By:     "alice",
				At:     ts,
			},
		},
		{
			name: "nick change",
			event: domain.NickChange{
				OldNick: "bob",
				NewNick: "robert",
				At:      ts,
			},
		},
		{
			name: "whois",
			event: domain.Whois{
				Target:  "#general",
				Nick:    "botty",
				ModelID: "test/model",
				At:      ts,
			},
		},
		{
			name: "list reply",
			event: domain.ListReply{
				Channel: "#general",
				Members: 3,
				Topic:   "welcome",
				At:      ts,
			},
		},
		{
			name: "command error",
			event: domain.CommandError{
				Target: "#general",
				Err:    "something went wrong",
				At:     ts,
			},
		},
		{
			name: "system notice",
			event: domain.SystemNotice{
				Target: "#general",
				Text:   "API key saved",
				At:     ts,
			},
		},
		{
			name: "personas list",
			event: domain.PersonasList{
				Personas: []domain.Persona{
					{ID: "pirate", Description: "A salty sea dog", Origin: domain.PersonaUser},
					{ID: "wizard", Description: "A wise old mage", Origin: domain.PersonaGenerated},
				},
				At: ts,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := domain.MarshalPersistableEvent(tt.event)
			require.NoError(t, err)

			got, err := domain.UnmarshalPersistableEvent(data)
			require.NoError(t, err)

			require.Equal(t, tt.event, got)
		})
	}
}

// TestRenderOnlyEvents_are_not_persistable pins the compile-time fact
// that the render-only feedback DTOs satisfy [domain.Event] but not
// [domain.PersistableEvent]: they render in the chat-screen scrollback
// yet never reach the store.
func TestRenderOnlyEvents_are_not_persistable(t *testing.T) {
	renderOnly := map[string]domain.Event{
		"help":       domain.Help{Target: "#general"},
		"usage hint": domain.UsageHint{Target: "#general", Command: "invite", Usage: "/add-model"},
	}

	for name, event := range renderOnly {
		t.Run(name, func(t *testing.T) {
			_, persistable := event.(domain.PersistableEvent)
			require.False(t, persistable,
				"%T must be Event-only, never PersistableEvent", event)
		})
	}
}

// TestPersistableEvent_partition checks that each persistable type it
// lists falls into exactly one of [domain.ChannelActivity] and
// [domain.IssuerReply] — disjoint, none left unclassified. The list is
// maintained by hand, so a new persistable type must be added here to
// be covered.
func TestPersistableEvent_partition(t *testing.T) {
	type classification struct {
		channelActivity bool
		issuerReply     bool
	}

	persistable := map[string]domain.PersistableEvent{
		"message":             domain.Message{},
		"join":                domain.Join{},
		"part":                domain.Part{},
		"quit":                domain.Quit{},
		"topic change":        domain.TopicChange{},
		"channel mode change": domain.ChannelModeChange{},
		"model invited":       domain.ModelInvited{},
		"model kicked":        domain.ModelKicked{},
		"nick change":         domain.NickChange{},
		"topic info":          domain.TopicInfo{},
		"whois":               domain.Whois{},
		"list reply":          domain.ListReply{},
		"command error":       domain.CommandError{},
		"system notice":       domain.SystemNotice{},
		"personas list":       domain.PersonasList{},
	}

	classify := func(e domain.PersistableEvent) classification {
		_, channelActivity := e.(domain.ChannelActivity)
		_, issuerReply := e.(domain.IssuerReply)

		return classification{channelActivity: channelActivity, issuerReply: issuerReply}
	}

	for name, event := range persistable {
		t.Run(name, func(t *testing.T) {
			got := classify(event)

			require.True(t, got.channelActivity != got.issuerReply,
				"%T must be exactly one of ChannelActivity or IssuerReply, got %+v", event, got)
		})
	}
}

// TestUnmarshalPersistableEvent_unknown_type ensures a stored row
// whose discriminator this build no longer recognises yields the
// [domain.ErrUnknownEventType] sentinel, so the channel-log read path
// can skip it rather than failing the whole batch.
func TestUnmarshalPersistableEvent_unknown_type(t *testing.T) {
	tests := map[string]string{
		"legacy help":       `{"type":"help","data":{"channel":"#general","at":"2026-04-06T12:00:00Z"}}`,
		"legacy usage hint": `{"type":"usage_hint","data":{"channel":"#general","command":"invite"}}`,
		"legacy list end":   `{"type":"list_end","data":{"at":"2026-04-06T12:00:00Z"}}`,
		"never known":       `{"type":"made_up","data":{}}`,
	}

	for name, row := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := domain.UnmarshalPersistableEvent([]byte(row))

			require.Nil(t, got)
			require.ErrorIs(t, err, domain.ErrUnknownEventType)
		})
	}
}

func TestMessage_RoutingKey(t *testing.T) {
	const userID domain.InstanceID = ""
	const bottyID domain.InstanceID = "inst-botty"
	const helperID domain.InstanceID = "inst-helper"

	tests := []struct {
		name     string
		msg      domain.Message
		self     domain.InstanceID
		wantKey  domain.ChannelName
		wantOK   bool
		whatItIs string
	}{
		{
			name:     "channel target routes to channel",
			msg:      domain.Message{Target: "#general", InstanceID: bottyID, From: "botty"},
			self:     userID,
			wantKey:  "#general",
			wantOK:   true,
			whatItIs: "channel events route by target regardless of self",
		},
		{
			name:     "user-to-model dm routes to model peer",
			msg:      domain.Message{Target: domain.ChannelName(bottyID), InstanceID: userID, From: "iain"},
			self:     userID,
			wantKey:  domain.ChannelName(bottyID),
			wantOK:   true,
			whatItIs: "user is the sender, peer is the recipient",
		},
		{
			name:     "model-to-user dm routes to model peer",
			msg:      domain.Message{Target: domain.ChannelName(userID), InstanceID: bottyID, From: "botty"},
			self:     userID,
			wantKey:  domain.ChannelName(bottyID),
			wantOK:   true,
			whatItIs: "user is the recipient, peer is the sender",
		},
		{
			name:     "model-to-model dm visible to one party",
			msg:      domain.Message{Target: domain.ChannelName(helperID), InstanceID: bottyID, From: "botty"},
			self:     bottyID,
			wantKey:  domain.ChannelName(helperID),
			wantOK:   true,
			whatItIs: "for the model that sent it, peer is the recipient",
		},
		{
			name:     "model-to-model dm visible to other party",
			msg:      domain.Message{Target: domain.ChannelName(helperID), InstanceID: bottyID, From: "botty"},
			self:     helperID,
			wantKey:  domain.ChannelName(bottyID),
			wantOK:   true,
			whatItIs: "for the model that received it, peer is the sender",
		},
		{
			name:     "foreign model-to-model dm hides from user",
			msg:      domain.Message{Target: domain.ChannelName(helperID), InstanceID: bottyID, From: "botty"},
			self:     userID,
			wantKey:  "",
			wantOK:   false,
			whatItIs: "user is not a party; routing returns ok=false so the chat screen ignores it",
		},
		{
			name:     "status target routes to status",
			msg:      domain.Message{Target: domain.StatusChannelName, InstanceID: userID, From: "iain"},
			self:     userID,
			wantKey:  domain.StatusChannelName,
			wantOK:   true,
			whatItIs: "status events route by target like channels",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotKey, gotOK := tt.msg.RoutingKey(tt.self)
			require.Equal(t, tt.wantOK, gotOK, tt.whatItIs)
			require.Equal(t, tt.wantKey, gotKey, tt.whatItIs)
		})
	}
}

func TestUnmarshalChannelEvent_unknown_type(t *testing.T) {
	_, err := domain.UnmarshalPersistableEvent([]byte(`{"type":"unknown","data":{}}`))
	require.Error(t, err)
	require.EqualError(t, err, `unknown channel event type: "unknown"`)
}

func TestUnmarshalChannelEvent_invalid_json(t *testing.T) {
	_, err := domain.UnmarshalPersistableEvent([]byte(`not json`))
	require.Error(t, err)
}
