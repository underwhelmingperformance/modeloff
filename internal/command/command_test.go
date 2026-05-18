package command

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type testJoinCommand struct {
	Channel string `arg:"channel" help:"Channel to join"`
}

type testPartCommand struct{}

type testInviteCommand struct {
	Nick    string `arg:"" optional:"" help:"Nick to invite"`
	Channel string `arg:"channel" optional:"" help:"Channel to invite them to"`
}

type testAddModelCommand struct {
	Model   string   `arg:"" optional:"" help:"Model to add"`
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
	Join     testJoinCommand     `cmd:"" aliases:"j,jo" help:"Join a channel."`
	Part     testPartCommand     `cmd:"" help:"Part."`
	List     testListCommand     `cmd:"" help:"List channels."`
	AddModel testAddModelCommand `cmd:"" help:"Add a model."`
	Invite   testInviteCommand   `cmd:"" help:"Invite a model."`
	Kick     testKickCommand     `cmd:"" help:"Kick."`
	Msg      testMsgCommand      `cmd:"" help:"Message."`
	Nick     testNickCommand     `cmd:"" help:"Change nick."`
	Topic    testTopicCommand    `cmd:"" help:"Set topic."`
	Whois    testWhoisCommand    `cmd:"" help:"Whois."`
	Config   testConfigCommand   `cmd:"" help:"Config."`
	Help     testHelpCommand     `cmd:"" help:"Help."`
	Quit     testQuitCommand     `cmd:"" aliases:"q" help:"Quit."`
}

func allCommands(t *testing.T) Set[testCtx] {
	t.Helper()

	set, err := Build[testCtx](&testGrammar{})
	require.NoError(t, err)

	return set
}

func TestParseValue(t *testing.T) {
	cmds := allCommands(t)

	tests := []struct {
		name  string
		input string
		want  any
	}{
		{
			name:  "join with arg",
			input: "/join test-channel",
			want:  testJoinCommand{Channel: "test-channel"},
		},
		{
			name:  "part",
			input: "/part",
			want:  testPartCommand{},
		},
		{
			name:  "list",
			input: "/list",
			want:  testListCommand{},
		},
		{
			name:  "add-model with model",
			input: "/add-model anthropic/claude-3-haiku",
			want:  testAddModelCommand{Model: "anthropic/claude-3-haiku"},
		},
		{
			name:  "add-model with persona",
			input: "/add-model anthropic/claude-3-haiku --persona Helpful assistant",
			want: testAddModelCommand{
				Model:   "anthropic/claude-3-haiku",
				Persona: []string{"Helpful", "assistant"},
			},
		},
		{
			name:  "add-model without args",
			input: "/add-model",
			want:  testAddModelCommand{},
		},
		{
			name:  "invite with nick",
			input: "/invite botty",
			want:  testInviteCommand{Nick: "botty"},
		},
		{
			name:  "invite with nick and channel",
			input: "/invite botty #general",
			want:  testInviteCommand{Nick: "botty", Channel: "#general"},
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
			name:  "nick with new name",
			input: "/nick alice",
			want:  testNickCommand{Nick: "alice"},
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
		{
			name:  "command with extra whitespace",
			input: "/join   test-channel  ",
			want:  testJoinCommand{Channel: "test-channel"},
		},
		{
			name:  "alias resolves to command",
			input: "/j test-channel",
			want:  testJoinCommand{Channel: "test-channel"},
		},
		{
			name:  "second alias resolves to command",
			input: "/jo test-channel",
			want:  testJoinCommand{Channel: "test-channel"},
		},
		{
			name:  "single alias resolves to command",
			input: "/q",
			want:  testQuitCommand{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := cmds.ParseValue(tt.input)
			require.NoError(t, err)
			require.Equal(t, tt.want, parsed)
		})
	}
}

func TestParseValue_errors(t *testing.T) {
	cmds := allCommands(t)

	tests := []struct {
		name    string
		input   string
		wantErr error
	}{
		{name: "empty string", input: "", wantErr: &NotACommandError{}},
		{name: "not a command", input: "hello world", wantErr: &NotACommandError{}},
		{name: "slash only", input: "/", wantErr: &UnknownCommandError{}},
		{name: "unknown command", input: "/unknown", wantErr: &UnknownCommandError{}},
		{name: "unknown alias", input: "/z", wantErr: &UnknownCommandError{}},
		{name: "join without args", input: "/join", wantErr: &MissingArgError{}},
		{name: "part rejects extra args", input: "/part extra stuff", wantErr: &ExtraArgsError{}},
		{name: "kick without args", input: "/kick", wantErr: &MissingArgError{}},
		{name: "msg without args", input: "/msg", wantErr: &MissingArgError{}},
		{name: "nick without args", input: "/nick", wantErr: &MissingArgError{}},
		{name: "whois without args", input: "/whois", wantErr: &MissingArgError{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := cmds.ParseValue(tt.input)
			require.ErrorAs(t, err, &tt.wantErr)
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

func (c runnableCommand) Run(_ context.Context, rc testContext) testResult {
	return testResult(rc.Value + ":" + c.Arg)
}

type runnableGrammar struct {
	Do runnableCommand `cmd:"" help:"Do something."`
}

func TestParser_Parse_returns_typed_command(t *testing.T) {
	parser, err := BuildParser[testCtx, testContext, testResult](&runnableGrammar{})
	require.NoError(t, err)

	cmd, err := parser.Parse("/do hello")
	require.NoError(t, err)

	result := cmd.Run(t.Context(), testContext{Value: "ctx"})
	require.Equal(t, testResult("ctx:hello"), result)
}

func TestParser_Parse_rejects_non_command(t *testing.T) {
	type nonRunnableGrammar struct {
		Join testJoinCommand `cmd:"" help:"Join."`
	}

	parser, err := BuildParser[testCtx, testContext, testResult](&nonRunnableGrammar{})
	require.NoError(t, err)

	_, err = parser.Parse("/join foo")

	var cmdErr *InterfaceError
	require.ErrorAs(t, err, &cmdErr)
	require.Equal(t, &InterfaceError{Value: testJoinCommand{Channel: "foo"}}, cmdErr)
}

func TestParser_Set_returns_underlying_set(t *testing.T) {
	parser, err := BuildParser[testCtx, testContext, testResult](&runnableGrammar{})
	require.NoError(t, err)

	set := parser.Set()

	var metas []nodeMeta
	for _, n := range set.Commands {
		metas = append(metas, toNodeMeta(n))
	}

	require.Equal(t, []nodeMeta{
		{Name: "do", Help: "Do something.", Positionals: []positionalMeta{{Name: "arg", Help: "An argument"}}},
	}, metas)
}

// --- Subcommand support ---

type subGetCommand struct {
	Key string `arg:"" help:"Key to get"`
}

type subSetCommand struct {
	Key   string `arg:"" help:"Key to set"`
	Value string `arg:"" help:"Value to set"`
}

type subResetCommand struct{}

type parentConfigCommand struct {
	Format string `optional:"" help:"Output format"`

	Get   subGetCommand   `cmd:"" help:"Get a config value."`
	Set   subSetCommand   `cmd:"" help:"Set a config value."`
	Reset subResetCommand `cmd:"" help:"Reset all config."`
}

type subcommandGrammar struct {
	Config parentConfigCommand `cmd:"" help:"Manage configuration."`
	Quit   testQuitCommand     `cmd:"" help:"Quit."`
}

func TestParseValue_subcommands(t *testing.T) {
	cmds, err := Build[testCtx](&subcommandGrammar{})
	require.NoError(t, err)

	tests := []struct {
		name  string
		input string
		want  any
	}{
		{name: "subcommand with one arg", input: "/config get api-key", want: subGetCommand{Key: "api-key"}},
		{name: "subcommand with two args", input: "/config set api-key sk-1234", want: subSetCommand{Key: "api-key", Value: "sk-1234"}},
		{name: "subcommand with no args", input: "/config reset", want: subResetCommand{}},
		{name: "parent flag before child", input: "/config --format json set api-key sk-1234", want: subSetCommand{Key: "api-key", Value: "sk-1234"}},
		{name: "parent flag after child", input: "/config set --format json api-key sk-1234", want: subSetCommand{Key: "api-key", Value: "sk-1234"}},
		{name: "leaf command still works", input: "/quit", want: testQuitCommand{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := cmds.ParseValue(tt.input)
			require.NoError(t, err)
			require.Equal(t, tt.want, parsed)
		})
	}
}

func TestParseValue_subcommand_errors(t *testing.T) {
	cmds, err := Build[testCtx](&subcommandGrammar{})
	require.NoError(t, err)

	tests := []struct {
		name    string
		input   string
		wantErr error
	}{
		{name: "group node without subcommand", input: "/config", wantErr: &SubcommandError{}},
		{name: "unknown subcommand", input: "/config bogus", wantErr: &UnknownSubcommandError{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := cmds.ParseValue(tt.input)
			require.ErrorAs(t, err, &tt.wantErr)
		})
	}
}

// Deep nesting: three levels.

type deepLeafCommand struct {
	Name string `arg:"" help:"Name"`
}

type deepMidCommand struct {
	Theme string `optional:"" help:"Theme"`

	Leaf deepLeafCommand `cmd:"" help:"A leaf command."`
}

type deepTopCommand struct {
	Format string `optional:"" help:"Format"`

	Mid deepMidCommand `cmd:"" help:"Middle level."`
}

type deepGrammar struct {
	Top deepTopCommand `cmd:"" help:"Top level."`
}

func TestParseValue_deeply_nested_subcommands(t *testing.T) {
	cmds, err := Build[testCtx](&deepGrammar{})
	require.NoError(t, err)

	tests := []struct {
		name  string
		input string
		want  any
	}{
		{name: "three levels deep", input: "/top mid leaf hello", want: deepLeafCommand{Name: "hello"}},
		{name: "ancestor flags at multiple levels", input: "/top --format json mid --theme dark leaf hello", want: deepLeafCommand{Name: "hello"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := cmds.ParseValue(tt.input)
			require.NoError(t, err)
			require.Equal(t, tt.want, parsed)
		})
	}
}

func TestParseValue_deeply_nested_subcommand_errors(t *testing.T) {
	cmds, err := Build[testCtx](&deepGrammar{})
	require.NoError(t, err)

	tests := []struct {
		name    string
		input   string
		wantErr error
	}{
		{name: "stops at group node", input: "/top mid", wantErr: &SubcommandError{}},
		{name: "stops at top group node", input: "/top", wantErr: &SubcommandError{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := cmds.ParseValue(tt.input)
			require.ErrorAs(t, err, &tt.wantErr)
		})
	}
}

func TestParseInvocation_returns_branch_values(t *testing.T) {
	cmds, err := Build[testCtx](&subcommandGrammar{})
	require.NoError(t, err)

	invocation, err := cmds.ParseInvocation("/config set --format json api-key sk-1234")
	require.NoError(t, err)

	pathNames := make([]string, len(invocation.Path))
	for i, p := range invocation.Path {
		pathNames[i] = p.Node.Name
	}

	require.Equal(t, []string{"config", "set"}, pathNames)
	require.Equal(t, parentConfigCommand{Format: "json"}, invocation.Path[0].Value)
	require.Equal(t, subSetCommand{Key: "api-key", Value: "sk-1234"}, invocation.Path[1].Value)

	parentValue, _ := invocation.ValueFor(invocation.Path[0].Node)
	require.Equal(t, parentConfigCommand{Format: "json"}, parentValue)

	pathValue, _ := invocation.ValueAtPath("config")
	require.Equal(t, parentConfigCommand{Format: "json"}, pathValue)
}

func TestParseInvocation_unknown_flag_checks_active_ancestors(t *testing.T) {
	cmds, err := Build[testCtx](&subcommandGrammar{})
	require.NoError(t, err)

	_, err = cmds.ParseInvocation("/config set --unknown value api-key sk-1234")

	var unknown *UnknownFlagError
	require.ErrorAs(t, err, &unknown)
	require.Equal(t, "--unknown", unknown.Flag)
}

func TestBuild_rejects_alias_collisions(t *testing.T) {
	tests := []struct {
		name    string
		grammar any
		wantErr error
	}{
		{
			name: "alias collides with command name",
			grammar: &struct {
				J    struct{} `cmd:"" help:"Exact J."`
				Join struct{} `cmd:"" aliases:"j" help:"Join."`
			}{},
			wantErr: &AliasCollisionError{Alias: "j", Command: "join", ConflictsWith: "j"},
		},
		{
			name: "duplicate alias across commands",
			grammar: &struct {
				Join struct{} `cmd:"" aliases:"j" help:"Join."`
				Jump struct{} `cmd:"" aliases:"j" help:"Jump."`
			}{},
			wantErr: &AliasCollisionError{Alias: "j", Command: "jump", ConflictsWith: "join"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := build[testCtx](tt.grammar)
			require.Equal(t, tt.wantErr, err)
		})
	}
}

func TestNode_Aliases_are_populated(t *testing.T) {
	cmds := allCommands(t)
	join := cmds.Find("join")

	require.Equal(t, []string{"j", "jo"}, join.Aliases)
}

func TestSet_Find_resolves_alias(t *testing.T) {
	cmds := allCommands(t)

	tests := []struct {
		name     string
		lookup   string
		wantName string
	}{
		{name: "exact name", lookup: "join", wantName: "join"},
		{name: "first alias", lookup: "j", wantName: "join"},
		{name: "second alias", lookup: "jo", wantName: "join"},
		{name: "single alias", lookup: "q", wantName: "quit"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := cmds.Find(tt.lookup)
			require.Equal(t, tt.wantName, node.Name)
		})
	}
}

func TestNode_Find_resolves_child_alias(t *testing.T) {
	type subCmd struct{}
	type parentCmd struct {
		Sub subCmd `cmd:"" aliases:"s" help:"Sub."`
	}
	type grammar struct {
		Parent parentCmd `cmd:"" help:"Parent."`
	}

	cmds, err := Build[testCtx](&grammar{})
	require.NoError(t, err)

	child := cmds.Find("parent").Find("s")
	require.Equal(t, "sub", child.Name)
}

func TestNode_DisplayName(t *testing.T) {
	cmds := allCommands(t)

	join := cmds.Find("join")
	require.Equal(t, "/join (/j, /jo)", join.DisplayName())

	quit := cmds.Find("quit")
	require.Equal(t, "/quit (/q)", quit.DisplayName())

	part := cmds.Find("part")
	require.Equal(t, "/part", part.DisplayName())
}

func TestNode_FullUsage(t *testing.T) {
	cmds := allCommands(t)

	join := cmds.Find("join")
	require.Equal(t, "/join (/j, /jo) <channel>", join.FullUsage())

	quit := cmds.Find("quit")
	require.Equal(t, "/quit (/q)", quit.FullUsage())

	part := cmds.Find("part")
	require.Equal(t, "/part", part.FullUsage())
}

func TestParseValue_subcommand_alias(t *testing.T) {
	type subCmd struct {
		Key string `arg:"" help:"Key"`
	}
	type parentCmd struct {
		Get subCmd `cmd:"" aliases:"g" help:"Get."`
	}
	type grammar struct {
		Config parentCmd `cmd:"" help:"Config."`
	}

	cmds, err := Build[testCtx](&grammar{})
	require.NoError(t, err)
	parsed, err := cmds.ParseValue("/config g my-key")

	require.NoError(t, err)
	require.Equal(t, subCmd{Key: "my-key"}, parsed)
}

func TestNode_DisplayName_subcommand(t *testing.T) {
	type subCmd struct{}
	type parentCmd struct {
		Get subCmd `cmd:"" aliases:"g" help:"Get."`
		Set subCmd `cmd:"" help:"Set."`
	}
	type grammar struct {
		Config parentCmd `cmd:"" help:"Config."`
	}

	cmds, err := Build[testCtx](&grammar{})
	require.NoError(t, err)
	config := cmds.Find("config")

	get := config.Find("get")
	require.Equal(t, "/get (/g)", get.DisplayName())

	set := config.Find("set")
	require.Equal(t, "/set", set.DisplayName())
}

func TestSubcommandError_includes_aliases(t *testing.T) {
	type subCmd struct{}
	type parentCmd struct {
		Get subCmd `cmd:"" aliases:"g" help:"Get."`
		Set subCmd `cmd:"" help:"Set."`
	}
	type grammar struct {
		Config parentCmd `cmd:"" help:"Config."`
	}

	cmds, err := Build[testCtx](&grammar{})
	require.NoError(t, err)
	_, err = cmds.ParseValue("/config")

	var subErr *SubcommandError
	require.ErrorAs(t, err, &subErr)
	require.Equal(t, &SubcommandError{
		Path:     "config",
		Children: []string{"get", "g", "set"},
	}, subErr)
}
