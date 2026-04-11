package command

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMerge(t *testing.T) {
	tests := []struct {
		name  string
		sets  []Set
		wants []string
	}{
		{
			name: "nearest wins on duplicate name",
			sets: []Set{
				{Commands: []*Node{{Name: "join", Help: "child"}}},
				{Commands: []*Node{{Name: "join", Help: "parent"}, {Name: "list", Help: "list"}}},
			},
			wants: []string{"join", "list"},
		},
		{
			name:  "no sets",
			sets:  nil,
			wants: nil,
		},
		{
			name:  "single empty set",
			sets:  []Set{{}},
			wants: nil,
		},
		{
			name: "single non-empty set",
			sets: []Set{
				{Commands: []*Node{{Name: "quit"}}},
			},
			wants: []string{"quit"},
		},
		{
			name:  "two empty sets",
			sets:  []Set{{}, {}},
			wants: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merged := Merge(tt.sets...)

			var names []string
			for _, n := range merged.Commands {
				names = append(names, n.Name)
			}

			require.Equal(t, tt.wants, names)
		})
	}

	t.Run("nearest wins preserves help from child", func(t *testing.T) {
		child := Set{Commands: []*Node{{Name: "join", Help: "child"}}}
		parent := Set{Commands: []*Node{{Name: "join", Help: "parent"}}}

		merged := Merge(child, parent)

		require.Equal(t, []nodeMeta{
			{Name: "join", Help: "child"},
		}, toNodeMetas(merged.Commands))
	})
}

func TestComplete_command_suggestions_carry_usage(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{Name: "join", Help: "Join channels", Positionals: []Positional{{Name: "channel"}}},
			{Name: "list", Help: "List channels"},
			{Name: "quit", Help: "Exit."},
		},
	}

	tests := []struct {
		name        string
		raw         string
		suggestions []Suggestion
	}{
		{
			name: "partial match",
			raw:  "/j",
			suggestions: []Suggestion{
				{Value: "join", Label: "/join", Detail: "Join channels", Usage: "/join <channel>"},
			},
		},
		{
			name: "exact match is still a suggestion",
			raw:  "/quit",
			suggestions: []Suggestion{
				{Value: "quit", Label: "/quit", Detail: "Exit.", Usage: "/quit"},
			},
		},
		{
			name: "all commands",
			raw:  "/",
			suggestions: []Suggestion{
				{Value: "join", Label: "/join", Detail: "Join channels", Usage: "/join <channel>"},
				{Value: "list", Label: "/list", Detail: "List channels", Usage: "/list"},
				{Value: "quit", Label: "/quit", Detail: "Exit.", Usage: "/quit"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completion := Complete(cmds, tt.raw, len([]rune(tt.raw)))

			require.True(t, completion.Visible)
			require.Equal(t, tt.suggestions, completion.Suggestions)
		})
	}
}

func TestComplete_argument_sources_are_contextual(t *testing.T) {
	nickSource := func(_ InvocationState) []Suggestion {
		return []Suggestion{
			{Value: "botty", Label: "botty"},
			{Value: "helper", Label: "helper"},
		}
	}

	cmds := Set{
		Commands: []*Node{
			{
				Name: "kick",
				Help: "Kick a nick",
				Positionals: []Positional{
					{Name: "nick", Source: nickSource},
				},
			},
		},
	}

	completion := Complete(cmds, "/kick h", 7)

	require.Equal(t, []Suggestion{{Value: "helper", Label: "helper"}}, completion.Suggestions)
	require.False(t, completion.AppendSpace)
}

func TestComplete_free_form_arguments_have_no_suggestions(t *testing.T) {
	nickSource := func(_ InvocationState) []Suggestion {
		return []Suggestion{{Value: "botty", Label: "botty"}}
	}

	cmds := Set{
		Commands: []*Node{
			{
				Name: "msg",
				Help: "Direct message",
				Positionals: []Positional{
					{Name: "nick", Source: nickSource},
					{Name: "message", Variadic: true, Optional: true, Help: "Message body"},
				},
			},
		},
	}

	completion := Complete(cmds, "/msg botty hello", len([]rune("/msg botty hello")))

	require.True(t, completion.Visible)
	require.Empty(t, completion.Suggestions)
}

func TestComplete_composes_sources(t *testing.T) {
	localSource := func(_ InvocationState) []Suggestion {
		return []Suggestion{{Value: "botty", Label: "botty", Detail: "test/model-a"}}
	}

	liveSource := func(_ InvocationState) []Suggestion {
		return []Suggestion{{Value: "anthropic/claude-3-haiku", Label: "anthropic/claude-3-haiku", Detail: "Claude Haiku"}}
	}

	cmds := Set{
		Commands: []*Node{
			{
				Name: "invite",
				Help: "Invite a model",
				Positionals: []Positional{
					{
						Name:   "model",
						Source: ComposeSources(localSource, liveSource),
					},
				},
				Flags: []Flag{
					{
						Name:     "--persona",
						Optional: true,
						Source:   LiteralSource(Suggestion{Value: "--persona", Label: "--persona"}),
					},
				},
			},
		},
	}

	completion := Complete(cmds, "/invite ", len([]rune("/invite ")))

	require.Equal(t, []Suggestion{
		{Value: "botty", Label: "botty", Detail: "test/model-a"},
		{Value: "anthropic/claude-3-haiku", Label: "anthropic/claude-3-haiku", Detail: "Claude Haiku"},
	}, completion.Suggestions)
	require.True(t, completion.AppendSpace)
}

func TestNode_Usage(t *testing.T) {
	tests := []struct {
		name string
		node Node
		want string
	}{
		{
			name: "no args",
			node: Node{Name: "quit"},
			want: "/quit",
		},
		{
			name: "required positional",
			node: Node{
				Name:        "join",
				Positionals: []Positional{{Name: "channel"}},
			},
			want: "/join <channel>",
		},
		{
			name: "optional positional",
			node: Node{
				Name:        "topic",
				Positionals: []Positional{{Name: "text", Optional: true}},
			},
			want: "/topic [text]",
		},
		{
			name: "mixed positionals",
			node: Node{
				Name: "msg",
				Positionals: []Positional{
					{Name: "nick"},
					{Name: "message", Optional: true},
				},
			},
			want: "/msg <nick> [message]",
		},
		{
			name: "with flag",
			node: Node{
				Name:        "invite",
				Positionals: []Positional{{Name: "model", Optional: true}},
				Flags:       []Flag{{Name: "--persona", Variadic: true}},
			},
			want: "/invite [model] [--persona <persona>]",
		},
		{
			name: "with children",
			node: Node{
				Name:     "admin",
				Children: []*Node{{Name: "ban"}},
			},
			want: "/admin <command>",
		},
		{
			name: "inherits ancestor flags",
			node: func() Node {
				parent := &Node{
					Name:  "config",
					Flags: []Flag{{Name: "--format", Variadic: true}},
				}

				child := &Node{
					Parent:      parent,
					Name:        "set",
					Positionals: []Positional{{Name: "key"}},
				}

				return *child
			}(),
			want: "/config set <key> [--format <format>]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.node.Usage())
		})
	}
}

func TestComplete_token_boundaries(t *testing.T) {
	nickSource := func(_ InvocationState) []Suggestion {
		return []Suggestion{
			{Value: "alice", Label: "alice"},
			{Value: "bob", Label: "bob"},
		}
	}

	cmds := Set{
		Commands: []*Node{
			{
				Name: "kick",
				Help: "Kick a nick",
				Positionals: []Positional{
					{Name: "nick", Source: nickSource},
				},
			},
			{Name: "quit", Help: "Exit."},
		},
	}

	allNicks := []Suggestion{
		{Value: "alice", Label: "alice"},
		{Value: "bob", Label: "bob"},
	}

	tests := []struct {
		name            string
		raw             string
		cursor          int
		wantCmd         bool
		wantSuggestions []Suggestion
	}{
		{
			name:    "cursor at zero shows command suggestions",
			raw:     "/k",
			cursor:  0,
			wantCmd: true,
		},
		{
			name:    "cursor at 1 shows command suggestions",
			raw:     "/kick alice",
			cursor:  1,
			wantCmd: true,
		},
		{
			name:            "cursor after space shows argument suggestions",
			raw:             "/kick ",
			cursor:          6,
			wantSuggestions: allNicks,
		},
		{
			name:            "cursor mid-argument filters",
			raw:             "/kick al",
			cursor:          8,
			wantSuggestions: []Suggestion{{Value: "alice", Label: "alice"}},
		},
		{
			name:            "cursor beyond length is clamped",
			raw:             "/kick ",
			cursor:          100,
			wantSuggestions: allNicks,
		},
		{
			name:   "not a command",
			raw:    "hello",
			cursor: 5,
		},
		{
			name:            "multiple spaces between tokens",
			raw:             "/kick   a",
			cursor:          9,
			wantSuggestions: []Suggestion{{Value: "alice", Label: "alice"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completion := Complete(cmds, tt.raw, tt.cursor)

			if !tt.wantCmd && tt.wantSuggestions == nil {
				require.False(t, completion.Visible)
				return
			}

			require.True(t, completion.Visible)

			if tt.wantSuggestions != nil {
				require.Equal(t, tt.wantSuggestions, completion.Suggestions)
			}
		})
	}
}

func TestComplete_unknown_command_has_no_suggestions(t *testing.T) {
	cmds := Set{
		Commands: []*Node{{Name: "quit", Help: "Exit."}},
	}

	completion := Complete(cmds, "/unknown arg", len([]rune("/unknown arg")))

	require.True(t, completion.Visible)
	require.Empty(t, completion.Suggestions)
}

func TestComplete_contains_match(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{Name: "claude-3-haiku", Help: "Haiku model"},
			{Name: "quit", Help: "Exit."},
		},
	}

	raw := "/aiku"
	completion := Complete(cmds, raw, len([]rune(raw)))

	require.True(t, completion.Visible)
	require.Equal(t, []Suggestion{
		{Value: "claude-3-haiku", Label: "/claude-3-haiku", Detail: "Haiku model", Usage: "/claude-3-haiku"},
	}, completion.Suggestions)
}

func TestNode_Find(t *testing.T) {
	child := &Node{Name: "ban"}
	parent := &Node{
		Name:     "admin",
		Children: []*Node{child, {Name: "unban"}},
	}

	require.Equal(t, child, parent.Find("ban"))
	require.Nil(t, parent.Find("nonexistent"))
	require.Nil(t, (&Node{Name: "empty"}).Find("anything"))
}

func TestNode_Leaf(t *testing.T) {
	require.True(t, (&Node{Name: "quit"}).Leaf())
	require.False(t, (&Node{Name: "admin", Children: []*Node{{Name: "ban"}}}).Leaf())
}

func TestParseValue_no_factory(t *testing.T) {
	cmds := Set{
		Commands: []*Node{{Name: "broken"}},
	}

	_, err := cmds.ParseValue("/broken")

	require.ErrorContains(t, err, "no factory")
}

func TestParseValue_after_merge(t *testing.T) {
	type mergeJoinCmd struct {
		Channel string `arg:"channel" help:"Channel"`
	}
	type mergeQuitCmd struct{}

	type childGrammar struct {
		Join mergeJoinCmd `cmd:"" help:"Child join."`
	}

	type parentGrammar struct {
		Join mergeJoinCmd `cmd:"" help:"Parent join."`
		Quit mergeQuitCmd `cmd:"" help:"Quit."`
	}

	child := Build(&childGrammar{})
	parent := Build(&parentGrammar{})
	merged := Merge(child, parent)

	t.Run("child command wins", func(t *testing.T) {
		parsed, err := merged.ParseValue("/join test")
		require.NoError(t, err)
		require.Equal(t, mergeJoinCmd{Channel: "test"}, parsed)
	})

	t.Run("parent command accessible", func(t *testing.T) {
		parsed, err := merged.ParseValue("/quit")
		require.NoError(t, err)
		require.Equal(t, mergeQuitCmd{}, parsed)
	})

	t.Run("unknown command errors", func(t *testing.T) {
		_, err := merged.ParseValue("/unknown")
		require.Error(t, err)
	})
}

func TestComplete_whitespace_after_slash(t *testing.T) {
	cmds := Set{
		Commands: []*Node{{Name: "quit", Help: "Exit."}},
	}

	completion := Complete(cmds, "/ ", len([]rune("/ ")))

	require.True(t, completion.Visible)
	require.Empty(t, completion.Suggestions)
}

func TestComplete_cursor_mid_command_name(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{Name: "quit", Help: "Exit."},
			{Name: "query", Help: "Query."},
		},
	}

	raw := "/quit"
	completion := Complete(cmds, raw, 3)

	require.True(t, completion.Visible)

	var names []string
	for _, s := range completion.Suggestions {
		names = append(names, s.Value)
	}

	// Cursor mid-token still filters with the full token text.
	require.Equal(t, []string{"quit"}, names)
}

func TestComplete_multiple_prefix_matches(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{Name: "quit", Help: "Exit."},
			{Name: "query", Help: "Query."},
			{Name: "queue", Help: "Queue."},
		},
	}

	raw := "/qu"
	completion := Complete(cmds, raw, len([]rune(raw)))

	require.True(t, completion.Visible)

	var names []string
	for _, s := range completion.Suggestions {
		names = append(names, s.Value)
	}

	require.Equal(t, []string{"quit", "query", "queue"}, names)
}

func TestComplete_flag_name_after_positionals(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{
				Name:        "kick",
				Help:        "Kick a nick",
				Positionals: []Positional{{Name: "nick"}},
				Flags: []Flag{
					{Name: "--reason", Optional: true, Help: "Kick reason"},
				},
			},
		},
	}

	raw := "/kick botty "
	completion := Complete(cmds, raw, len([]rune(raw)))

	require.True(t, completion.Visible)
	require.Equal(t, []Suggestion{
		{Value: "--reason", Label: "--reason", Detail: "Kick reason"},
	}, completion.Suggestions)
}

func TestComplete_flag_name_prefix_filters(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{
				Name: "invite",
				Help: "Invite a model",
				Positionals: []Positional{
					{Name: "model", Optional: true},
				},
				Flags: []Flag{
					{Name: "--persona", Optional: true, Help: "Persona text"},
					{Name: "--priority", Optional: true, Help: "Priority level"},
				},
			},
		},
	}

	raw := "/invite model-a --per"
	completion := Complete(cmds, raw, len([]rune(raw)))

	require.True(t, completion.Visible)
	require.Equal(t, []Suggestion{
		{Value: "--persona", Label: "--persona", Detail: "Persona text"},
	}, completion.Suggestions)
}

func TestComplete_flag_value_uses_source(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{
				Name: "config",
				Help: "Configure",
				Positionals: []Positional{
					{Name: "key", Source: LiteralSource(
						Suggestion{Value: "api-key", Label: "api-key"},
						Suggestion{Value: "theme", Label: "theme"},
					)},
				},
				Flags: []Flag{
					{
						Name:     "--format",
						Optional: true,
						Help:     "Output format",
						Source: LiteralSource(
							Suggestion{Value: "json", Label: "json"},
							Suggestion{Value: "yaml", Label: "yaml"},
						),
					},
				},
			},
		},
	}

	raw := "/config api-key --format "
	completion := Complete(cmds, raw, len([]rune(raw)))

	require.True(t, completion.Visible)
	require.Equal(t, []Suggestion{
		{Value: "json", Label: "json"},
		{Value: "yaml", Label: "yaml"},
	}, completion.Suggestions)
}

func TestComplete_flag_value_filters_by_prefix(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{
				Name: "config",
				Help: "Configure",
				Flags: []Flag{
					{
						Name:     "--format",
						Optional: true,
						Source: LiteralSource(
							Suggestion{Value: "json", Label: "json"},
							Suggestion{Value: "yaml", Label: "yaml"},
						),
					},
				},
			},
		},
	}

	raw := "/config --format j"
	completion := Complete(cmds, raw, len([]rune(raw)))

	require.True(t, completion.Visible)
	require.Equal(t, []Suggestion{
		{Value: "json", Label: "json"},
	}, completion.Suggestions)
}

func TestComplete_flags_interleaved_with_positionals(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{
				Name: "invite",
				Help: "Invite a model",
				Positionals: []Positional{
					{Name: "model", Optional: true, Source: LiteralSource(
						Suggestion{Value: "claude", Label: "claude"},
					)},
				},
				Flags: []Flag{
					{Name: "--persona", Optional: true, Help: "Persona"},
				},
			},
		},
	}

	// Flag before positional: after --persona value, should offer model suggestions.
	raw := "/invite --persona friendly "
	completion := Complete(cmds, raw, len([]rune(raw)))

	require.True(t, completion.Visible)
	require.Equal(t, []Suggestion{
		{Value: "claude", Label: "claude"},
	}, completion.Suggestions)
}

func TestComplete_subcommand_names(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{
				Name: "admin",
				Help: "Admin commands",
				Children: []*Node{
					{Name: "ban", Help: "Ban a user"},
					{Name: "unban", Help: "Unban a user"},
					{Name: "mute", Help: "Mute a user"},
				},
			},
		},
	}

	tests := []struct {
		name  string
		raw   string
		wants []string
	}{
		{
			name:  "all subcommands",
			raw:   "/admin ",
			wants: []string{"ban", "unban", "mute"},
		},
		{
			name:  "filtered by prefix",
			raw:   "/admin mu",
			wants: []string{"mute"},
		},
		{
			name:  "no match",
			raw:   "/admin x",
			wants: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completion := Complete(cmds, tt.raw, len([]rune(tt.raw)))

			require.True(t, completion.Visible)

			var names []string
			for _, s := range completion.Suggestions {
				names = append(names, s.Value)
			}

			require.Equal(t, tt.wants, names)
		})
	}
}

func TestComplete_flag_only_command(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{
				Name: "config",
				Help: "Configure",
				Flags: []Flag{
					{Name: "--api-key", Optional: true, Help: "API key"},
					{Name: "--theme", Optional: true, Help: "Theme"},
				},
			},
		},
	}

	tests := []struct {
		name  string
		raw   string
		wants []string
	}{
		{
			name:  "all flags offered",
			raw:   "/config ",
			wants: []string{"--api-key", "--theme"},
		},
		{
			name:  "used flag excluded",
			raw:   "/config --api-key secret ",
			wants: []string{"--theme"},
		},
		{
			name:  "all flags used",
			raw:   "/config --api-key secret --theme dark ",
			wants: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completion := Complete(cmds, tt.raw, len([]rune(tt.raw)))

			require.True(t, completion.Visible)

			var names []string
			for _, s := range completion.Suggestions {
				names = append(names, s.Value)
			}

			require.Equal(t, tt.wants, names)
		})
	}
}

func TestComplete_subcommand_recurses_into_child(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{
				Name: "config",
				Help: "Configuration",
				Flags: []Flag{
					{Name: "--format", Optional: true, Help: "Output format"},
				},
				Children: []*Node{
					{
						Name: "set",
						Help: "Set a value",
						Positionals: []Positional{
							{
								Name: "key",
								Source: LiteralSource(
									Suggestion{Value: "api-key", Label: "api-key"},
									Suggestion{Value: "theme", Label: "theme"},
								),
							},
						},
					},
					{Name: "get", Help: "Get a value"},
					{Name: "reset", Help: "Reset config"},
				},
			},
		},
	}

	tests := []struct {
		name  string
		raw   string
		wants []string
	}{
		{
			name:  "subcommand names after parent",
			raw:   "/config ",
			wants: []string{"set", "get", "reset", "--format"},
		},
		{
			name:  "subcommand names filtered",
			raw:   "/config s",
			wants: []string{"set", "reset"},
		},
		{
			name:  "child positional after subcommand selected",
			raw:   "/config set ",
			wants: []string{"api-key", "theme"},
		},
		{
			name:  "ancestor flag suggested on child",
			raw:   "/config set --",
			wants: []string{"--format"},
		},
		{
			name:  "child positional filtered",
			raw:   "/config set th",
			wants: []string{"theme"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completion := Complete(cmds, tt.raw, len([]rune(tt.raw)))

			require.True(t, completion.Visible)

			var names []string
			for _, s := range completion.Suggestions {
				names = append(names, s.Value)
			}

			require.Equal(t, tt.wants, names)
		})
	}
}

func TestComplete_group_node_combines_child_and_flag_suggestions(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{
				Name: "config",
				Flags: []Flag{
					{Name: "--format", Optional: true, Help: "Output format"},
				},
				Children: []*Node{
					{Name: "set", Help: "Set a value"},
					{Name: "get", Help: "Get a value"},
				},
			},
		},
	}

	completion := Complete(cmds, "/config ", len([]rune("/config ")))

	require.True(t, completion.Visible)
	require.Equal(t, []Suggestion{
		{Value: "set", Label: "set", Detail: "Set a value"},
		{Value: "get", Label: "get", Detail: "Get a value"},
		{Value: "--format", Label: "--format", Detail: "Output format"},
	}, completion.Suggestions)
}

func TestComplete_ancestor_flag_value_uses_source(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{
				Name: "config",
				Flags: []Flag{
					{
						Name:     "--format",
						Optional: true,
						Source: LiteralSource(
							Suggestion{Value: "json", Label: "json"},
							Suggestion{Value: "yaml", Label: "yaml"},
						),
					},
				},
				Children: []*Node{
					{
						Name: "set",
						Positionals: []Positional{
							{Name: "key"},
						},
					},
				},
			},
		},
	}

	completion := Complete(cmds, "/config set --format ", len([]rune("/config set --format ")))

	require.True(t, completion.Visible)
	require.Equal(t, []Suggestion{
		{Value: "json", Label: "json"},
		{Value: "yaml", Label: "yaml"},
	}, completion.Suggestions)
}

func TestComplete_used_ancestor_flags_are_excluded(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{
				Name: "config",
				Flags: []Flag{
					{Name: "--format", Optional: true, Help: "Output format"},
				},
				Children: []*Node{
					{Name: "set", Help: "Set"},
				},
			},
		},
	}

	completion := Complete(cmds, "/config set --format json --", len([]rune("/config set --format json --")))

	require.True(t, completion.Visible)
	require.Empty(t, completion.Suggestions)
}

func TestComplete_bool_flag_does_not_expect_a_value(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{
				Name: "config",
				Flags: []Flag{
					{Name: "--reset", Boolean: true, Optional: true, Help: "Reset"},
				},
				Children: []*Node{
					{Name: "api-key", Help: "API key"},
					{Name: "poke-interval", Help: "Poke interval"},
				},
			},
		},
	}

	completion := Complete(cmds, "/config --reset ", len([]rune("/config --reset ")))

	require.True(t, completion.Visible)
	require.Equal(t, []Suggestion{
		{Value: "api-key", Label: "api-key", Detail: "API key"},
		{Value: "poke-interval", Label: "poke-interval", Detail: "Poke interval"},
	}, completion.Suggestions)
}

func TestComplete_deep_nesting_walks_into_grandchildren(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{
				Name: "admin",
				Help: "Admin commands",
				Children: []*Node{
					{
						Name: "user",
						Help: "User management",
						Children: []*Node{
							{
								Name: "ban",
								Help: "Ban a user",
								Positionals: []Positional{
									{
										Name: "nick",
										Source: LiteralSource(
											Suggestion{Value: "alice", Label: "alice"},
											Suggestion{Value: "bob", Label: "bob"},
										),
									},
								},
							},
							{Name: "unban", Help: "Unban a user"},
						},
					},
					{Name: "stats", Help: "Show stats"},
				},
			},
		},
	}

	tests := []struct {
		name  string
		raw   string
		wants []string
	}{
		{
			name:  "level 1: top children",
			raw:   "/admin ",
			wants: []string{"user", "stats"},
		},
		{
			name:  "level 2: grandchildren",
			raw:   "/admin user ",
			wants: []string{"ban", "unban"},
		},
		{
			name:  "level 2: filtered grandchildren",
			raw:   "/admin user b",
			wants: []string{"ban", "unban"},
		},
		{
			name:  "level 3: leaf positional source",
			raw:   "/admin user ban ",
			wants: []string{"alice", "bob"},
		},
		{
			name:  "level 3: leaf positional filtered",
			raw:   "/admin user ban a",
			wants: []string{"alice"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completion := Complete(cmds, tt.raw, len([]rune(tt.raw)))

			require.True(t, completion.Visible)

			var names []string
			for _, s := range completion.Suggestions {
				names = append(names, s.Value)
			}

			require.Equal(t, tt.wants, names)
		})
	}
}

func TestComplete_optional_positional_with_source(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{
				Name: "invite",
				Help: "Invite a model",
				Positionals: []Positional{
					{
						Name:     "model",
						Optional: true,
						Source: LiteralSource(
							Suggestion{Value: "claude", Label: "claude"},
							Suggestion{Value: "gemini", Label: "gemini"},
						),
					},
				},
			},
		},
	}

	raw := "/invite "
	completion := Complete(cmds, raw, len([]rune(raw)))

	require.True(t, completion.Visible)
	require.Equal(t, []Suggestion{
		{Value: "claude", Label: "claude"},
		{Value: "gemini", Label: "gemini"},
	}, completion.Suggestions)
}
