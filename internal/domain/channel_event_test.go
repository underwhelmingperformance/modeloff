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
				Target:  "#general",
				Nick:    "bob",
				Message: "gone fishing",
				At:      ts,
			},
		},
		{
			name: "quit without message",
			event: domain.Quit{
				Target: "#general",
				Nick:   "bob",
				At:     ts,
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
			event: domain.ModeChange{
				Target: "#general",
				Nick:   "bob",
				Mode:   domain.ModeVoice,
				By:     "ChanServ",
				At:     ts,
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
				Target:  "#general",
				OldNick: "bob",
				NewNick: "robert",
				At:      ts,
			},
		},
		{
			name: "help",
			event: domain.Help{
				Target: "#general",
				At:     ts,
			},
		},
		{
			name: "whois",
			event: domain.Whois{
				Target:   "#general",
				Instance: domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil),
				At:       ts,
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
			name: "list end",
			event: domain.ListEnd{
				At: ts,
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
			name: "usage hint",
			event: domain.UsageHint{
				Target:  "#general",
				Command: "invite",
				Usage:   "/add-model <model-id> [--persona <text>]",
				At:      ts,
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
