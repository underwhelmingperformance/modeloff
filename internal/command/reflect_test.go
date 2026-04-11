package command

import (
	"testing"

	"github.com/stretchr/testify/require"
)

var nargs1 = 1

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
	Boolean  bool
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
			Boolean:  f.Boolean,
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
	Children    []nodeMeta
}

func toNodeMetas(nodes []*Node) []nodeMeta {
	metas := make([]nodeMeta, len(nodes))
	for i, n := range nodes {
		metas[i] = toNodeMeta(n)
	}

	return metas
}

func toNodeMeta(n *Node) nodeMeta {
	var children []nodeMeta
	for _, child := range n.Children {
		children = append(children, toNodeMeta(child))
	}

	return nodeMeta{
		Name:        n.Name,
		Help:        n.Help,
		Positionals: toPositionalMeta(n.Positionals),
		Flags:       toFlagMeta(n.Flags),
		Children:    children,
	}
}

// fieldMetaMeta is fieldMeta without the decoder, for test comparison.
type fieldMetaMeta struct {
	Name     string
	Help     string
	Index    int
	IsFlag   bool
	BoolFlag bool
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
			BoolFlag: f.boolFlag,
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
			cmd:  testJoinCommand{},
			want: []fieldMetaMeta{
				{Name: "channel", Help: "Channel to join", Index: 0},
			},
		},
		{
			name: "no fields",
			cmd:  testPartCommand{},
			want: nil,
		},
		{
			name: "positional and flag",
			cmd:  testInviteCommand{},
			want: []fieldMetaMeta{
				{Name: "model", Help: "Model to invite", Optional: true, Index: 0},
				{Name: "persona", Help: "Optional persona", Optional: true, Variadic: true, IsFlag: true, FlagName: "--persona", Index: 1},
			},
		},
		{
			name: "variadic positional with nargs",
			cmd:  testMsgCommand{},
			want: []fieldMetaMeta{
				{Name: "nick", Help: "Nick to message", Index: 0},
				{Name: "body", Help: "Message text", Optional: true, Variadic: true, Nargs: &nargs1, Index: 1},
			},
		},
		{
			name: "arg tag overrides field name",
			cmd:  testNickCommand{},
			want: []fieldMetaMeta{
				{Name: "new-nick", Help: "New nickname", Index: 0},
			},
		},
		{
			name: "optional variadic without nargs",
			cmd:  testTopicCommand{},
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

	stubSource := LiteralSource(Suggestion{Value: "a", Label: "a"})

	sources := map[string]SuggestionSource{
		"channel": stubSource,
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
		"nonexistent": LiteralSource(Suggestion{Value: "x", Label: "x"}),
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

func TestBuildFlags_marks_bool_flags(t *testing.T) {
	fields := []fieldMeta{
		{name: "reset", help: "Reset", isFlag: true, boolFlag: true, flagName: "--reset", index: 0},
	}

	flags := buildFlags(fields, nil)

	require.Equal(t, []flagMeta{
		{Name: "--reset", Help: "Reset", Boolean: true},
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

	require.Equal(t, []nodeMeta{
		{
			Name:        "join",
			Help:        "Join a channel.",
			Positionals: []positionalMeta{{Name: "channel", Help: "Channel to join"}},
		},
		{
			Name: "part",
			Help: "Part from the channel.",
		},
	}, toNodeMetas(nodes))
	require.NotNil(t, nodes[0].factory)
	require.NotNil(t, nodes[1].factory)
}

func TestBuild_name_tag_overrides_field_name(t *testing.T) {
	type renameCommand struct{}
	grammar := &struct {
		Foo renameCommand `cmd:"" name:"bar" help:"A renamed command."`
	}{}

	nodes, err := build(grammar)
	require.NoError(t, err)

	require.Equal(t, []nodeMeta{
		{Name: "bar", Help: "A renamed command."},
	}, toNodeMetas(nodes))
}

func TestBuild_skips_non_cmd_fields(t *testing.T) {
	type someCommand struct{}
	grammar := &struct {
		Cmd    someCommand `cmd:"" help:"A command."`
		NotCmd string
	}{}

	nodes, err := build(grammar)
	require.NoError(t, err)

	require.Equal(t, []nodeMeta{
		{Name: "cmd", Help: "A command."},
	}, toNodeMetas(nodes))
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

	require.Equal(t, []nodeMeta{
		{
			Name:        "join",
			Help:        "Join a channel.",
			Positionals: []positionalMeta{{Name: "channel", Help: "Channel to join"}},
		},
		{
			Name: "part",
			Help: "Part from the channel.",
		},
	}, toNodeMetas(nodes))

	cmd := nodes[0].factory()
	require.IsType(t, &buildJoinCommand{}, cmd)
}

func TestBuild_unexported_fields_are_skipped(t *testing.T) {
	type someCommand struct{}
	grammar := &struct {
		Pub someCommand `cmd:"" help:"Public."`
		prv someCommand `cmd:"" help:"Private."`
	}{}

	nodes, err := build(grammar)
	require.NoError(t, err)

	require.Equal(t, []nodeMeta{
		{Name: "pub", Help: "Public."},
	}, toNodeMetas(nodes))
}

func TestBuild_empty_grammar(t *testing.T) {
	grammar := &struct{}{}

	nodes, err := build(grammar)
	require.NoError(t, err)

	require.Nil(t, nodes)
}

type completerCommand struct {
	Target string `arg:"" help:"Target"`
}

func (completerCommand) Sources() map[string]SuggestionSource {
	return map[string]SuggestionSource{
		"target": LiteralSource(Suggestion{Value: "a", Label: "a"}),
	}
}

func TestBuild_picks_up_completer_sources(t *testing.T) {
	grammar := &struct {
		Do completerCommand `cmd:"" help:"Do something."`
	}{}

	nodes, err := build(grammar)
	require.NoError(t, err)

	require.Equal(t, []nodeMeta{
		{Name: "do", Help: "Do something.", Positionals: []positionalMeta{{Name: "target", Help: "Target"}}},
	}, toNodeMetas(nodes))
	require.NotNil(t, nodes[0].Positionals[0].Source, "Source should be wired from Completer")
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

// --- Subcommand (recursive build) tests ---

type buildSubGet struct {
	Key string `arg:"" help:"Key to get"`
}

type buildSubSet struct {
	Key   string `arg:"" help:"Key to set"`
	Value string `arg:"" help:"Value to set"`
}

type buildSubReset struct{}

type buildConfigGroup struct {
	Format string `optional:"" help:"Output format"`

	Get   buildSubGet   `cmd:"" help:"Get a config value."`
	Set   buildSubSet   `cmd:"" help:"Set a config value."`
	Reset buildSubReset `cmd:"" help:"Reset all config."`
}

type buildSubcommandGrammar struct {
	Config buildConfigGroup `cmd:"" help:"Manage configuration."`
	Quit   buildPartCommand `cmd:"" help:"Quit."`
}

func TestBuild_recursive_subcommands(t *testing.T) {
	nodes, err := build(&buildSubcommandGrammar{})
	require.NoError(t, err)

	require.Equal(t, []nodeMeta{
		{
			Name: "config",
			Help: "Manage configuration.",
			Flags: []flagMeta{
				{Name: "--format", Help: "Output format", Optional: true},
			},
			Children: []nodeMeta{
				{
					Name:        "get",
					Help:        "Get a config value.",
					Positionals: []positionalMeta{{Name: "key", Help: "Key to get"}},
				},
				{
					Name: "set",
					Help: "Set a config value.",
					Positionals: []positionalMeta{
						{Name: "key", Help: "Key to set"},
						{Name: "value", Help: "Value to set"},
					},
				},
				{
					Name: "reset",
					Help: "Reset all config.",
				},
			},
		},
		{
			Name: "quit",
			Help: "Quit.",
		},
	}, toNodeMetas(nodes))
}

func TestBuild_group_nodes_have_factories_and_parent_links(t *testing.T) {
	nodes, err := build(&buildSubcommandGrammar{})
	require.NoError(t, err)

	require.Equal(t, []nodeMeta{
		{
			Name: "config",
			Help: "Manage configuration.",
			Flags: []flagMeta{
				{Name: "--format", Help: "Output format", Optional: true},
			},
			Children: []nodeMeta{
				{
					Name:        "get",
					Help:        "Get a config value.",
					Positionals: []positionalMeta{{Name: "key", Help: "Key to get"}},
				},
				{
					Name: "set",
					Help: "Set a config value.",
					Positionals: []positionalMeta{
						{Name: "key", Help: "Key to set"},
						{Name: "value", Help: "Value to set"},
					},
				},
				{
					Name: "reset",
					Help: "Reset all config.",
				},
			},
		},
		{
			Name: "quit",
			Help: "Quit.",
		},
	}, toNodeMetas(nodes))

	configNode := nodes[0]
	require.NotNil(t, configNode.factory, "group node should keep a factory")
	require.Nil(t, configNode.Parent)

	for _, child := range configNode.Children {
		require.NotNil(t, child.factory, "leaf child %q should have a factory", child.Name)
		require.Equal(t, configNode, child.Parent, "child %q should link back to parent", child.Name)
	}

	require.NotNil(t, nodes[1].factory, "top-level leaf should have a factory")
}

func TestNode_AllFlags_includes_ancestor_flags(t *testing.T) {
	nodes, err := build(&buildDeepGrammar{})
	require.NoError(t, err)

	require.Equal(t, []nodeMeta{
		{
			Name: "top",
			Help: "Top level.",
			Children: []nodeMeta{
				{
					Name: "mid",
					Help: "Middle level.",
					Children: []nodeMeta{
						{
							Name:        "leaf",
							Help:        "A leaf command.",
							Positionals: []positionalMeta{{Name: "name", Help: "Name"}},
						},
					},
				},
			},
		},
	}, toNodeMetas(nodes))

	top := nodes[0]
	top.Flags = []Flag{{Name: "--top-flag", Help: "Top", Optional: true}}

	mid := top.Children[0]
	mid.Flags = []Flag{{Name: "--mid-flag", Help: "Mid", Optional: true}}

	leaf := mid.Children[0]
	leaf.Flags = []Flag{{Name: "--leaf-flag", Help: "Leaf", Optional: true}}

	require.Equal(t, []flagMeta{
		{Name: "--top-flag", Help: "Top", Optional: true},
		{Name: "--mid-flag", Help: "Mid", Optional: true},
		{Name: "--leaf-flag", Help: "Leaf", Optional: true},
	}, toFlagMeta(leaf.AllFlags()))
}

type buildDeepLeaf struct {
	Name string `arg:"" help:"Name"`
}

type buildDeepMid struct {
	Leaf buildDeepLeaf `cmd:"" help:"A leaf command."`
}

type buildDeepTop struct {
	Mid buildDeepMid `cmd:"" help:"Middle level."`
}

type buildDeepGrammar struct {
	Top buildDeepTop `cmd:"" help:"Top level."`
}

func TestBuild_three_level_nesting(t *testing.T) {
	nodes, err := build(&buildDeepGrammar{})
	require.NoError(t, err)

	require.Equal(t, []nodeMeta{
		{
			Name: "top",
			Help: "Top level.",
			Children: []nodeMeta{
				{
					Name: "mid",
					Help: "Middle level.",
					Children: []nodeMeta{
						{
							Name:        "leaf",
							Help:        "A leaf command.",
							Positionals: []positionalMeta{{Name: "name", Help: "Name"}},
						},
					},
				},
			},
		},
	}, toNodeMetas(nodes))

	topNode := nodes[0]
	require.NotNil(t, topNode.factory)
	require.NotNil(t, topNode.Children[0].factory)
	require.NotNil(t, topNode.Children[0].Children[0].factory)
}
