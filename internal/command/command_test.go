package command

import (
	"testing"

	"github.com/stretchr/testify/require"
)

type testJoinCommand struct {
	Channel string `arg:"channel" help:"Channel to join"`
}

type testPartCommand struct{}

type testInviteCommand struct {
	Model   string   `arg:"" optional:"" help:"Model to invite"`
	Persona []string `optional:"" help:"Optional persona"`
}

type testKickCommand struct {
	Nick string `arg:"" help:"Nick to kick"`
}

type testMsgCommand struct {
	Nick string   `arg:"" help:"Nick to message"`
	Body []string `arg:"" optional:"" nargs:"1" help:"Message text"`
}

type testNickCommand struct {
	Nick string `arg:"new-nick" help:"New nickname"`
}

type testTopicCommand struct {
	Topic []string `arg:"" optional:"" help:"Topic text"`
}

type testWhoisCommand struct {
	Nick string `arg:"" help:"Nick to look up"`
}

type testHelpCommand struct{}
type testQuitCommand struct{}
type testListCommand struct{}

type testConfigCommand struct {
	Key   string   `arg:"" optional:"" help:"Configuration key"`
	Value []string `arg:"" optional:"" help:"Configuration value"`
}

type testGrammar struct {
	Join   testJoinCommand   `cmd:"" help:"Join a channel."`
	Part   testPartCommand   `cmd:"" help:"Part."`
	List   testListCommand   `cmd:"" help:"List channels."`
	Invite testInviteCommand `cmd:"" help:"Invite a model."`
	Kick   testKickCommand   `cmd:"" help:"Kick."`
	Msg    testMsgCommand    `cmd:"" help:"Message."`
	Nick   testNickCommand   `cmd:"" help:"Change nick."`
	Topic  testTopicCommand  `cmd:"" help:"Set topic."`
	Whois  testWhoisCommand  `cmd:"" help:"Whois."`
	Config testConfigCommand `cmd:"" help:"Config."`
	Help   testHelpCommand   `cmd:"" help:"Help."`
	Quit   testQuitCommand   `cmd:"" help:"Quit."`
}

func allCommands() Set {
	return Build(&testGrammar{})
}

func TestParseValue(t *testing.T) {
	cmds := allCommands()

	tests := []struct {
		name    string
		input   string
		want    any
		wantErr bool
	}{
		{
			name:  "join with arg",
			input: "/join test-channel",
			want:  testJoinCommand{Channel: "test-channel"},
		},
		{
			name:    "join without args",
			input:   "/join",
			wantErr: true,
		},
		{
			name:  "part",
			input: "/part",
			want:  testPartCommand{},
		},
		{
			name:    "part rejects extra args",
			input:   "/part extra stuff",
			wantErr: true,
		},
		{
			name:  "list",
			input: "/list",
			want:  testListCommand{},
		},
		{
			name:  "invite with model",
			input: "/invite anthropic/claude-3-haiku",
			want:  testInviteCommand{Model: "anthropic/claude-3-haiku"},
		},
		{
			name:  "invite with persona",
			input: "/invite anthropic/claude-3-haiku --persona Helpful assistant",
			want: testInviteCommand{
				Model:   "anthropic/claude-3-haiku",
				Persona: []string{"Helpful", "assistant"},
			},
		},
		{
			name:  "invite without args",
			input: "/invite",
			want:  testInviteCommand{},
		},
		{
			name:  "kick with nick",
			input: "/kick claud3",
			want:  testKickCommand{Nick: "claud3"},
		},
		{
			name:    "kick without args",
			input:   "/kick",
			wantErr: true,
		},
		{
			name:  "msg with nick and message",
			input: "/msg claud3 hello there",
			want:  testMsgCommand{Nick: "claud3", Body: []string{"hello", "there"}},
		},
		{
			name:  "msg with nick only",
			input: "/msg claud3",
			want:  testMsgCommand{Nick: "claud3"},
		},
		{
			name:    "msg without args",
			input:   "/msg",
			wantErr: true,
		},
		{
			name:  "nick with new name",
			input: "/nick alice",
			want:  testNickCommand{Nick: "alice"},
		},
		{
			name:    "nick without args",
			input:   "/nick",
			wantErr: true,
		},
		{
			name:  "topic with text",
			input: "/topic General Discussion",
			want:  testTopicCommand{Topic: []string{"General", "Discussion"}},
		},
		{
			name:  "topic without args clears",
			input: "/topic",
			want:  testTopicCommand{},
		},
		{
			name:  "whois with nick",
			input: "/whois claud3",
			want:  testWhoisCommand{Nick: "claud3"},
		},
		{
			name:    "whois without args",
			input:   "/whois",
			wantErr: true,
		},
		{
			name:  "help",
			input: "/help",
			want:  testHelpCommand{},
		},
		{
			name:  "quit",
			input: "/quit",
			want:  testQuitCommand{},
		},
		{
			name:  "config",
			input: "/config",
			want:  testConfigCommand{},
		},
		{
			name:  "config api key",
			input: "/config api-key test-key",
			want:  testConfigCommand{Key: "api-key", Value: []string{"test-key"}},
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
			input: "/join   test-channel  ",
			want:  testJoinCommand{Channel: "test-channel"},
		},
		{
			name:    "slash only",
			input:   "/",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := cmds.ParseValue(tt.input)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, parsed)
		})
	}
}

type testContext struct {
	Value string
}

type testResult string

type runnableCommand struct {
	Arg string `arg:"" help:"An argument"`
}

func (c runnableCommand) Run(ctx testContext) testResult {
	return testResult(ctx.Value + ":" + c.Arg)
}

type runnableGrammar struct {
	Do runnableCommand `cmd:"" help:"Do something."`
}

func TestParser_Parse_returns_typed_command(t *testing.T) {
	parser := BuildParser[testContext, testResult](&runnableGrammar{})

	cmd, err := parser.Parse("/do hello")
	require.NoError(t, err)

	result := cmd.Run(testContext{Value: "ctx"})
	require.Equal(t, testResult("ctx:hello"), result)
}

func TestParser_Parse_rejects_non_command(t *testing.T) {
	// testJoinCommand does not implement Command[testContext, testResult]
	type nonRunnableGrammar struct {
		Join testJoinCommand `cmd:"" help:"Join."`
	}

	parser := BuildParser[testContext, testResult](&nonRunnableGrammar{})

	_, err := parser.Parse("/join foo")
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not implement")
}

func TestParser_Set_returns_underlying_set(t *testing.T) {
	parser := BuildParser[testContext, testResult](&runnableGrammar{})

	set := parser.Set()
	require.Len(t, set.Commands, 1)
	require.Equal(t, "do", set.Commands[0].Name)
}
