package command

import (
	"testing"

	"github.com/stretchr/testify/require"
)

type testGrammar struct {
	Join   JoinCommand   `cmd:"" help:"Join a channel."`
	Leave  LeaveCommand  `cmd:"" help:"Leave."`
	List   ListCommand   `cmd:"" help:"List channels."`
	Invite InviteCommand `cmd:"" help:"Invite a model."`
	Kick   KickCommand   `cmd:"" help:"Kick."`
	Msg    MsgCommand    `cmd:"" help:"Message."`
	Nick   NickCommand   `cmd:"" help:"Change nick."`
	Topic  TopicCommand  `cmd:"" help:"Set topic."`
	Whois  WhoisCommand  `cmd:"" help:"Whois."`
	Config ConfigCommand `cmd:"" help:"Config."`
	Help   HelpCommand   `cmd:"" help:"Help."`
	Quit   QuitCommand   `cmd:"" help:"Quit."`
}

func allCommands() Set {
	return Build(&testGrammar{})
}

func TestParse(t *testing.T) {
	cmds := allCommands()

	tests := []struct {
		name    string
		input   string
		want    any
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
			name:    "leave rejects extra args",
			input:   "/leave extra stuff",
			wantErr: true,
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
				Persona: []string{"Helpful", "assistant"},
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
			want:  MsgCommand{Nick: "claud3", Body: []string{"hello", "there"}},
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
			want:  TopicCommand{Topic: []string{"General", "Discussion"}},
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
			want:  ConfigCommand{Key: "api-key", Value: []string{"test-key"}},
		},
		{
			name:  "config poke interval",
			input: "/config poke-interval 10m",
			want:  ConfigCommand{Key: "poke-interval", Value: []string{"10m"}},
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
			inv, err := cmds.Parse(tt.input)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, inv.parsed)
		})
	}
}
