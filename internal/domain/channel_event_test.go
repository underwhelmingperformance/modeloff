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
			name: "list output",
			event: domain.ChannelList{
				Channels: []domain.Channel{{Name: "#general"}, {Name: "#random"}},
				At:       ts,
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

func TestUnmarshalChannelEvent_unknown_type(t *testing.T) {
	_, err := domain.UnmarshalPersistableEvent([]byte(`{"type":"unknown","data":{}}`))
	require.Error(t, err)
	require.EqualError(t, err, `unknown channel event type: "unknown"`)
}

func TestUnmarshalChannelEvent_invalid_json(t *testing.T) {
	_, err := domain.UnmarshalPersistableEvent([]byte(`not json`))
	require.Error(t, err)
}
