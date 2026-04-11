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
		event domain.ChannelEvent
	}{
		{
			name: "message",
			event: domain.ChannelMessage{
				Channel: "#general",
				From:    "alice",
				Body:    "hello world",
				At:      ts,
			},
		},
		{
			name: "action message",
			event: domain.ChannelMessage{
				Channel: "#general",
				From:    "alice",
				Body:    "waves",
				Action:  true,
				At:      ts,
			},
		},
		{
			name: "join",
			event: domain.ChannelJoin{
				Channel: "#general",
				Nick:    "bob",
				At:      ts,
			},
		},
		{
			name: "join with created",
			event: domain.ChannelJoin{
				Channel: "#new",
				Nick:    "bob",
				Created: true,
				At:      ts,
			},
		},
		{
			name: "join with message",
			event: domain.ChannelJoin{
				Channel: "#general",
				Nick:    "bob",
				Message: "hello everyone",
				At:      ts,
			},
		},
		{
			name: "part",
			event: domain.ChannelPart{
				Channel: "#general",
				Nick:    "bob",
				At:      ts,
			},
		},
		{
			name: "part with message",
			event: domain.ChannelPart{
				Channel: "#general",
				Nick:    "bob",
				Message: "see ya later",
				At:      ts,
			},
		},
		{
			name: "quit",
			event: domain.ChannelQuit{
				Channel: "#general",
				Nick:    "bob",
				Message: "gone fishing",
				At:      ts,
			},
		},
		{
			name: "quit without message",
			event: domain.ChannelQuit{
				Channel: "#general",
				Nick:    "bob",
				At:      ts,
			},
		},
		{
			name: "topic change",
			event: domain.ChannelTopicChange{
				Channel: "#general",
				Topic:   "new topic",
				By:      "alice",
				At:      ts,
			},
		},
		{
			name: "mode change",
			event: domain.ChannelModeChange{
				Channel: "#general",
				Nick:    "bob",
				Mode:    domain.ModeVoice,
				At:      ts,
			},
		},
		{
			name: "model invited",
			event: domain.ChannelModelInvited{
				Channel: "#general",
				Nick:    "botty",
				ModelID: "anthropic/claude-3-haiku",
				At:      ts,
			},
		},
		{
			name: "model kicked",
			event: domain.ChannelModelKicked{
				Channel: "#general",
				Nick:    "botty",
				At:      ts,
			},
		},
		{
			name: "nick change",
			event: domain.ChannelNickChange{
				Channel: "#general",
				OldNick: "bob",
				NewNick: "robert",
				At:      ts,
			},
		},
		{
			name: "help",
			event: domain.ChannelHelp{
				Channel: "#general",
				At:      ts,
			},
		},
		{
			name: "whois",
			event: domain.ChannelWhois{
				Channel:  "#general",
				Instance: domain.Instance{Nick: "botty", ModelID: "test/model"},
				At:       ts,
			},
		},
		{
			name: "list output",
			event: domain.ChannelListOutput{
				Channels: []domain.Channel{{Name: "#general"}, {Name: "#random"}},
				At:       ts,
			},
		},
		{
			name: "command error",
			event: domain.ChannelCommandError{
				Channel: "#general",
				Err:     "something went wrong",
				At:      ts,
			},
		},
		{
			name: "usage hint",
			event: domain.ChannelUsageHint{
				Channel: "#general",
				Command: "invite",
				Usage:   "/add-model <model-id> [--persona <text>]",
				At:      ts,
			},
		},
		{
			name: "system notice",
			event: domain.ChannelSystemNotice{
				Channel: "#general",
				Text:    "API key saved",
				At:      ts,
			},
		},
		{
			name: "personas list",
			event: domain.ChannelPersonasList{
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
			data, err := domain.MarshalChannelEvent(tt.event)
			require.NoError(t, err)

			got, err := domain.UnmarshalChannelEvent(data)
			require.NoError(t, err)

			require.Equal(t, tt.event, got)
		})
	}
}

func TestUnmarshalChannelEvent_unknown_type(t *testing.T) {
	_, err := domain.UnmarshalChannelEvent([]byte(`{"type":"unknown","data":{}}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown channel event type")
}

func TestUnmarshalChannelEvent_invalid_json(t *testing.T) {
	_, err := domain.UnmarshalChannelEvent([]byte(`not json`))
	require.Error(t, err)
}
