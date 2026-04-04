package command

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    Command
		wantErr bool
	}{
		// /join
		{
			name:  "join with channel name",
			input: "/join #general",
			want:  JoinCommand{Channel: "#general"},
		},
		{
			name:  "join without # prefix adds it",
			input: "/join general",
			want:  JoinCommand{Channel: "#general"},
		},
		{
			name:    "join without args",
			input:   "/join",
			wantErr: true,
		},

		// /leave
		{
			name:  "leave",
			input: "/leave",
			want:  LeaveCommand{},
		},
		{
			name:  "leave ignores extra args",
			input: "/leave extra stuff",
			want:  LeaveCommand{},
		},

		// /list
		{
			name:  "list",
			input: "/list",
			want:  ListCommand{},
		},

		// /invite
		{
			name:  "invite with model",
			input: "/invite anthropic/claude-3-haiku",
			want:  InviteCommand{Model: "anthropic/claude-3-haiku"},
		},
		{
			name:  "invite with persona",
			input: "/invite anthropic/claude-3-haiku --persona Helpful assistant",
			want: InviteCommand{
				Model:   "anthropic/claude-3-haiku",
				Persona: "Helpful assistant",
			},
		},
		{
			name:  "invite without args opens picker",
			input: "/invite",
			want:  InviteCommand{},
		},

		// /kick
		{
			name:  "kick with nick",
			input: "/kick claud3",
			want:  KickCommand{Nick: "claud3"},
		},
		{
			name:    "kick without args",
			input:   "/kick",
			wantErr: true,
		},

		// /msg
		{
			name:  "msg with nick and message",
			input: "/msg claud3 hello there",
			want:  MsgCommand{Nick: "claud3", Body: "hello there"},
		},
		{
			name:  "msg with nick only",
			input: "/msg claud3",
			want:  MsgCommand{Nick: "claud3"},
		},
		{
			name:    "msg without args",
			input:   "/msg",
			wantErr: true,
		},

		// /nick
		{
			name:  "nick with new name",
			input: "/nick alice",
			want:  NickCommand{Nick: "alice"},
		},
		{
			name:    "nick without args",
			input:   "/nick",
			wantErr: true,
		},

		// /topic
		{
			name:  "topic with text",
			input: "/topic General Discussion",
			want:  TopicCommand{Topic: "General Discussion"},
		},
		{
			name:  "topic without args clears",
			input: "/topic",
			want:  TopicCommand{},
		},

		// /whois
		{
			name:  "whois with nick",
			input: "/whois claud3",
			want:  WhoisCommand{Nick: "claud3"},
		},
		{
			name:    "whois without args",
			input:   "/whois",
			wantErr: true,
		},

		// /help
		{
			name:  "help",
			input: "/help",
			want:  HelpCommand{},
		},

		// /quit
		{
			name:  "quit",
			input: "/quit",
			want:  QuitCommand{},
		},

		// /config
		{
			name:  "config",
			input: "/config",
			want:  ConfigCommand{},
		},
		{
			name:  "config api key",
			input: "/config api-key test-key",
			want:  ConfigCommand{Key: "api-key", Value: "test-key"},
		},
		{
			name:  "config poke interval",
			input: "/config poke-interval 10m",
			want:  ConfigCommand{Key: "poke-interval", Value: "10m"},
		},

		// Edge cases
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:    "not a command",
			input:   "hello world",
			wantErr: true,
		},
		{
			name:    "unknown command",
			input:   "/unknown",
			wantErr: true,
		},
		{
			name:  "command with extra whitespace",
			input: "/join   #general  ",
			want:  JoinCommand{Channel: "#general"},
		},
		{
			name:    "slash only",
			input:   "/",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.input)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
