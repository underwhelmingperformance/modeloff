package command

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"
)

// positionalMeta is Positional without the Source field, which is not
// comparable.
type positionalMeta struct {
	Name     string
	Help     string
	Optional bool
	Variadic bool
	Nargs    *int
}

func toPositionalMeta(positionals []Positional) []positionalMeta {
	if len(positionals) == 0 {
		return nil
	}

	out := make([]positionalMeta, len(positionals))

	for i, p := range positionals {
		out[i] = positionalMeta{
			Name:     p.Name,
			Help:     p.Help,
			Optional: p.Optional,
			Variadic: p.Variadic,
			Nargs:    p.Nargs,
		}
	}

	return out
}

// flagMeta is Flag without the Source field, which is not comparable.
type flagMeta struct {
	Name     string
	Help     string
	Optional bool
	Variadic bool
}

func toFlagMeta(flags []Flag) []flagMeta {
	if len(flags) == 0 {
		return nil
	}

	out := make([]flagMeta, len(flags))

	for i, f := range flags {
		out[i] = flagMeta{
			Name:     f.Name,
			Help:     f.Help,
			Optional: f.Optional,
			Variadic: f.Variadic,
		}
	}

	return out
}

// nodeMeta is Node without the non-comparable fields (Handler,
// factory, Source on positionals/flags), for structural test
// assertions.
type nodeMeta struct {
	Name        string
	Help        string
	Positionals []positionalMeta
	Flags       []flagMeta
}

func toNodeMeta(n *Node) nodeMeta {
	return nodeMeta{
		Name:        n.Name,
		Help:        n.Help,
		Positionals: toPositionalMeta(n.Positionals),
		Flags:       toFlagMeta(n.Flags),
	}
}

func intPtr(n int) *int { return &n }

// fieldMetaMeta is fieldMeta without the decoder, for test comparison.
type fieldMetaMeta struct {
	Name     string
	Help     string
	Index    int
	IsFlag   bool
	FlagName string
	Optional bool
	Variadic bool
	Nargs    *int
}

func toFieldMeta(fields []fieldMeta) []fieldMetaMeta {
	out := make([]fieldMetaMeta, len(fields))

	for i, f := range fields {
		out[i] = fieldMetaMeta{
			Name:     f.name,
			Help:     f.help,
			Index:    f.index,
			IsFlag:   f.isFlag,
			FlagName: f.flagName,
			Optional: f.optional,
			Variadic: f.variadic,
			Nargs:    f.nargs,
		}
	}

	return out
}

func TestResolveFieldMetas(t *testing.T) {
	tests := []struct {
		name string
		cmd  any
		want []fieldMetaMeta
	}{
		{
			name: "positional with explicit name",
			cmd:  JoinCommand{},
			want: []fieldMetaMeta{
				{Name: "channel", Help: "Channel to join or create", Index: 0},
			},
		},
		{
			name: "no fields",
			cmd:  PartCommand{},
			want: nil,
		},
		{
			name: "positional and flag",
			cmd:  InviteCommand{},
			want: []fieldMetaMeta{
				{Name: "model", Help: "Model to invite", Optional: true, Index: 0},
				{Name: "persona", Help: "Optional persona", Optional: true, Variadic: true, IsFlag: true, FlagName: "--persona", Index: 1},
			},
		},
		{
			name: "variadic positional with nargs",
			cmd:  MsgCommand{},
			want: []fieldMetaMeta{
				{Name: "nick", Help: "Nick to message", Index: 0},
				{Name: "body", Help: "Message text", Optional: true, Variadic: true, Nargs: intPtr(1), Index: 1},
			},
		},
		{
			name: "arg tag overrides field name",
			cmd:  NickCommand{},
			want: []fieldMetaMeta{
				{Name: "new-nick", Help: "New nickname", Index: 0},
			},
		},
		{
			name: "optional variadic without nargs",
			cmd:  TopicCommand{},
			want: []fieldMetaMeta{
				{Name: "topic", Help: "Topic text", Optional: true, Variadic: true, Index: 0},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveFieldMetas(tt.cmd)
			require.NoError(t, err)

			if tt.want == nil {
				require.Nil(t, got)
				return
			}

			require.Equal(t, tt.want, toFieldMeta(got))
		})
	}
}

func TestBuildPositionals(t *testing.T) {
	fields := []fieldMeta{
		{name: "channel", help: "Channel to join", index: 0},
	}

	sources := map[string]SuggestionSource{
		"channel": ChannelsSource(),
	}

	positionals := buildPositionals(fields, sources)

	require.Equal(t, []positionalMeta{
		{Name: "channel", Help: "Channel to join"},
	}, toPositionalMeta(positionals))
	require.NotNil(t, positionals[0].Source)
}

func TestBuildPositionals_unknown_source_ignored(t *testing.T) {
	fields := []fieldMeta{
		{name: "nick", help: "Nick", index: 0},
	}

	sources := map[string]SuggestionSource{
		"nonexistent": ChannelsSource(),
	}

	positionals := buildPositionals(fields, sources)

	require.Equal(t, []positionalMeta{
		{Name: "nick", Help: "Nick"},
	}, toPositionalMeta(positionals))
	require.Nil(t, positionals[0].Source)
}

func TestBuildPositionals_skips_flags(t *testing.T) {
	fields := []fieldMeta{
		{name: "model", help: "Model", index: 0},
		{name: "persona", help: "Persona", isFlag: true, flagName: "--persona", variadic: true, index: 1},
	}

	positionals := buildPositionals(fields, nil)

	require.Equal(t, []positionalMeta{
		{Name: "model", Help: "Model"},
	}, toPositionalMeta(positionals))
}

func TestBuildFlags(t *testing.T) {
	fields := []fieldMeta{
		{name: "persona", help: "Persona", isFlag: true, flagName: "--persona", variadic: true, index: 0},
	}

	flags := buildFlags(fields, nil)

	require.Equal(t, []flagMeta{
		{Name: "--persona", Help: "Persona", Variadic: true},
	}, toFlagMeta(flags))
}

func TestBuildFlags_skips_positionals(t *testing.T) {
	fields := []fieldMeta{
		{name: "channel", help: "Channel", index: 0},
		{name: "persona", help: "Persona", isFlag: true, flagName: "--persona", variadic: true, index: 1},
	}

	flags := buildFlags(fields, nil)

	require.Equal(t, []flagMeta{
		{Name: "--persona", Help: "Persona", Variadic: true},
	}, toFlagMeta(flags))
}

func TestToKebabCase(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Channel", "channel"},
		{"FooBar", "foo-bar"},
		{"Nick", "nick"},
		{"ModelID", "model-id"},
		{"HTMLParser", "html-parser"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			require.Equal(t, tt.want, toKebabCase(tt.input))
		})
	}
}

type buildJoinCommand struct {
	Channel string `arg:"channel" help:"Channel to join"`
}

type buildPartCommand struct{}

type buildGrammar struct {
	Join buildJoinCommand `cmd:"" help:"Join a channel."`
	Part buildPartCommand `cmd:"" help:"Part from the channel."`
}

func TestBuild_produces_nodes_from_grammar(t *testing.T) {
	nodes, err := build(&buildGrammar{})
	require.NoError(t, err)

	require.Len(t, nodes, 2)

	require.Equal(t, nodeMeta{
		Name:        "join",
		Help:        "Join a channel.",
		Positionals: []positionalMeta{{Name: "channel", Help: "Channel to join"}},
	}, toNodeMeta(nodes[0]))
	require.NotNil(t, nodes[0].factory)

	require.Equal(t, nodeMeta{
		Name: "part",
		Help: "Part from the channel.",
	}, toNodeMeta(nodes[1]))
	require.NotNil(t, nodes[1].factory)
}

func TestBuild_name_tag_overrides_field_name(t *testing.T) {
	type renameCommand struct{}
	grammar := &struct {
		Foo renameCommand `cmd:"" name:"bar" help:"A renamed command."`
	}{}

	nodes, err := build(grammar)
	require.NoError(t, err)

	require.Len(t, nodes, 1)
	require.Equal(t, "bar", nodes[0].Name)
}

func TestBuild_skips_non_cmd_fields(t *testing.T) {
	type someCommand struct{}
	grammar := &struct {
		Cmd    someCommand `cmd:"" help:"A command."`
		NotCmd string
	}{}

	nodes, err := build(grammar)
	require.NoError(t, err)

	require.Len(t, nodes, 1)
	require.Equal(t, "cmd", nodes[0].Name)
}

func TestBuild_rejects_non_pointer(t *testing.T) {
	_, err := build(buildGrammar{})
	require.Error(t, err)
}

func TestBuild_rejects_non_struct(t *testing.T) {
	s := "not a struct"

	_, err := build(&s)
	require.Error(t, err)
}

func TestBuild_factory_creates_pointer(t *testing.T) {
	nodes, err := build(&buildGrammar{})
	require.NoError(t, err)

	cmd := nodes[0].factory()
	_, ok := cmd.(*buildJoinCommand)
	require.True(t, ok, "expected *buildJoinCommand, got %T", cmd)
}

func TestBind_attaches_typed_handler(t *testing.T) {
	cmds := Build(&buildGrammar{})
	var received buildJoinCommand

	Bind(cmds, "join", func(cmd buildJoinCommand) tea.Cmd {
		received = cmd
		return nil
	})

	inv, err := cmds.Parse("/join test-channel")
	require.NoError(t, err)
	inv.Run()

	require.Equal(t, buildJoinCommand{Channel: "test-channel"}, received)
}

func TestBind_panics_on_unknown_command(t *testing.T) {
	cmds := Build(&buildGrammar{})

	require.Panics(t, func() {
		Bind(cmds, "nonexistent", func(_ buildJoinCommand) tea.Cmd { return nil })
	})
}

func TestParsed_extracts_typed_value(t *testing.T) {
	cmds := Build(&buildGrammar{})

	inv, err := cmds.Parse("/join test-channel")
	require.NoError(t, err)

	cmd := Parsed[buildJoinCommand](inv)
	require.Equal(t, buildJoinCommand{Channel: "test-channel"}, cmd)
}

func TestParsed_wrong_type_panics(t *testing.T) {
	cmds := Build(&buildGrammar{})

	inv, err := cmds.Parse("/join test-channel")
	require.NoError(t, err)

	require.Panics(t, func() {
		Parsed[buildPartCommand](inv)
	})
}

func TestBind_wrong_type_panics_on_run(t *testing.T) {
	cmds := Build(&buildGrammar{})

	Bind(cmds, "join", func(_ buildPartCommand) tea.Cmd { return nil })

	inv, err := cmds.Parse("/join test-channel")
	require.NoError(t, err)

	require.Panics(t, func() {
		inv.Run()
	})
}

func TestBuild_unexported_fields_are_skipped(t *testing.T) {
	type someCommand struct{}
	grammar := &struct {
		Pub someCommand `cmd:"" help:"Public."`
		prv someCommand `cmd:"" help:"Private."`
	}{}

	nodes, err := build(grammar)
	require.NoError(t, err)

	require.Len(t, nodes, 1)
	require.Equal(t, "pub", nodes[0].Name)
}

func TestBuild_empty_grammar(t *testing.T) {
	grammar := &struct{}{}

	nodes, err := build(grammar)
	require.NoError(t, err)

	require.Nil(t, nodes)
}

func TestBuild_panics_on_undecodable_field(t *testing.T) {
	type badCommand struct {
		Ch chan int `arg:"" help:"A channel"`
	}

	grammar := &struct {
		Bad badCommand `cmd:"" help:"Bad."`
	}{}

	require.Panics(t, func() {
		Build(grammar)
	})
}
