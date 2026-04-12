package command

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
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
		{
			name: "alias in higher-priority set shadows name in lower-priority set",
			sets: []Set{
				{Commands: []*Node{{Name: "join", Aliases: []string{"j"}}}},
				{Commands: []*Node{{Name: "j", Help: "bare j"}}},
			},
			wants: []string{"join"},
		},
		{
			name: "name in higher-priority set shadows alias in lower-priority set",
			sets: []Set{
				{Commands: []*Node{{Name: "j"}}},
				{Commands: []*Node{{Name: "join", Aliases: []string{"j"}}}},
			},
			wants: []string{"j"},
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
		name string
		raw  string
		want Completion
	}{
		{
			name: "partial match",
			raw:  "/j",
			want: Completion{
				Visible: true, ReplaceStart: 1, ReplaceEnd: 2, AppendSpace: true,
				Suggestions: []Suggestion{
					{Value: "join", Label: "/join", Detail: "Join channels", Usage: "/join <channel>"},
				},
			},
		},
		{
			name: "exact match is still a suggestion",
			raw:  "/quit",
			want: Completion{
				Visible: true, ReplaceStart: 1, ReplaceEnd: 5, AppendSpace: true,
				Suggestions: []Suggestion{
					{Value: "quit", Label: "/quit", Detail: "Exit.", Usage: "/quit"},
				},
			},
		},
		{
			name: "all commands",
			raw:  "/",
			want: Completion{
				Visible: true, ReplaceStart: 1, ReplaceEnd: 1, AppendSpace: true,
				Suggestions: []Suggestion{
					{Value: "join", Label: "/join", Detail: "Join channels", Usage: "/join <channel>"},
					{Value: "list", Label: "/list", Detail: "List channels", Usage: "/list"},
					{Value: "quit", Label: "/quit", Detail: "Exit.", Usage: "/quit"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Complete(cmds, tt.raw, len([]rune(tt.raw)), domain.KindChannel))
		})
	}
}

func TestComplete_filters_commands_by_channel_kind(t *testing.T) {
	channelOnly := domain.KindChannel

	cmds := Set{
		Commands: []*Node{
			{Name: "join", Help: "Join channels"},
			{Name: "topic", Help: "Set topic", RequiredKind: &channelOnly},
			{Name: "kick", Help: "Kick a nick", RequiredKind: &channelOnly},
			{Name: "quit", Help: "Exit"},
		},
	}

	tests := []struct {
		name string
		kind domain.ChannelKind
		want []string
	}{
		{
			name: "channel shows all commands",
			kind: domain.KindChannel,
			want: []string{"join", "topic", "kick", "quit"},
		},
		{
			name: "DM hides channel-only commands",
			kind: domain.KindDM,
			want: []string{"join", "quit"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completion := Complete(cmds, "/", 1, tt.kind)

			var names []string
			for _, s := range completion.Suggestions {
				names = append(names, s.Value)
			}

			require.Equal(t, tt.want, names)
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

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 6, ReplaceEnd: 7, AppendSpace: false,
		Suggestions: []Suggestion{{Value: "helper", Label: "helper"}},
	}, Complete(cmds, "/kick h", 7, domain.KindChannel))
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

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 11, ReplaceEnd: 16,
	}, Complete(cmds, "/msg botty hello", 16, domain.KindChannel))
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

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 8, ReplaceEnd: 8, AppendSpace: true,
		Suggestions: []Suggestion{
			{Value: "botty", Label: "botty", Detail: "test/model-a"},
			{Value: "anthropic/claude-3-haiku", Label: "anthropic/claude-3-haiku", Detail: "Claude Haiku"},
		},
	}, Complete(cmds, "/invite ", 8, domain.KindChannel))
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
			want: "",
		},
		{
			name: "required positional",
			node: Node{
				Name:        "join",
				Positionals: []Positional{{Name: "channel"}},
			},
			want: "<channel>",
		},
		{
			name: "optional positional",
			node: Node{
				Name:        "topic",
				Positionals: []Positional{{Name: "text", Optional: true}},
			},
			want: "[text]",
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
			want: "<nick> [message]",
		},
		{
			name: "with flag",
			node: Node{
				Name:        "invite",
				Positionals: []Positional{{Name: "model", Optional: true}},
				Flags:       []Flag{{Name: "--persona", Variadic: true}},
			},
			want: "[model] [--persona <persona>]",
		},
		{
			name: "with children",
			node: Node{
				Name:     "admin",
				Children: []*Node{{Name: "ban"}},
			},
			want: "<command>",
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
			want: "<key> [--format <format>]",
		},
		{
			name: "no positionals or flags",
			node: Node{
				Name: "help",
			},
			want: "",
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
		name   string
		raw    string
		cursor int
		want   Completion
	}{
		{
			name: "cursor at zero shows command suggestions", raw: "/k", cursor: 0,
			want: Completion{Visible: true, ReplaceStart: 1, ReplaceEnd: 2, AppendSpace: true, Suggestions: []Suggestion{
				{Value: "kick", Label: "/kick", Detail: "Kick a nick", Usage: "/kick <nick>"},
			}},
		},
		{
			name: "cursor at 1 shows command suggestions", raw: "/kick alice", cursor: 1,
			want: Completion{Visible: true, ReplaceStart: 1, ReplaceEnd: 5, AppendSpace: true, Suggestions: []Suggestion{
				{Value: "kick", Label: "/kick", Detail: "Kick a nick", Usage: "/kick <nick>"},
			}},
		},
		{
			name: "cursor after space shows argument suggestions", raw: "/kick ", cursor: 6,
			want: Completion{Visible: true, ReplaceStart: 6, ReplaceEnd: 6, Suggestions: allNicks},
		},
		{
			name: "cursor mid-argument filters", raw: "/kick al", cursor: 8,
			want: Completion{Visible: true, ReplaceStart: 6, ReplaceEnd: 8, Suggestions: []Suggestion{{Value: "alice", Label: "alice"}}},
		},
		{
			name: "cursor beyond length is clamped", raw: "/kick ", cursor: 100,
			want: Completion{Visible: true, ReplaceStart: 6, ReplaceEnd: 6, Suggestions: allNicks},
		},
		{
			name: "not a command", raw: "hello", cursor: 5,
			want: Completion{},
		},
		{
			name: "multiple spaces between tokens", raw: "/kick   a", cursor: 9,
			want: Completion{Visible: true, ReplaceStart: 8, ReplaceEnd: 9, Suggestions: []Suggestion{{Value: "alice", Label: "alice"}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Complete(cmds, tt.raw, tt.cursor, domain.KindChannel))
		})
	}
}

func TestComplete_unknown_command_has_no_suggestions(t *testing.T) {
	cmds := Set{
		Commands: []*Node{{Name: "quit", Help: "Exit."}},
	}

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 9, ReplaceEnd: 12,
	}, Complete(cmds, "/unknown arg", 12, domain.KindChannel))
}

func TestComplete_contains_match(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{Name: "claude-3-haiku", Help: "Haiku model"},
			{Name: "quit", Help: "Exit."},
		},
	}

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 1, ReplaceEnd: 5, AppendSpace: true,
		Suggestions: []Suggestion{
			{Value: "claude-3-haiku", Label: "/claude-3-haiku", Detail: "Haiku model", Usage: "/claude-3-haiku"},
		},
	}, Complete(cmds, "/aiku", 5, domain.KindChannel))
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

	var noFactory *NoFactoryError
	require.ErrorAs(t, err, &noFactory)
	require.Equal(t, &NoFactoryError{Node: cmds.Commands[0]}, noFactory)
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

	child, err := Build(&childGrammar{})
	require.NoError(t, err)

	parent, err := Build(&parentGrammar{})
	require.NoError(t, err)

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

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 1, ReplaceEnd: 2, AppendSpace: true,
		Suggestions: []Suggestion{},
	}, Complete(cmds, "/ ", 2, domain.KindChannel))
}

func TestComplete_cursor_mid_command_name(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{Name: "quit", Help: "Exit."},
			{Name: "query", Help: "Query."},
		},
	}

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 1, ReplaceEnd: 5, AppendSpace: true,
		Suggestions: []Suggestion{
			{Value: "quit", Label: "/quit", Detail: "Exit.", Usage: "/quit"},
		},
	}, Complete(cmds, "/quit", 3, domain.KindChannel))
}

func TestComplete_multiple_prefix_matches(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{Name: "quit", Help: "Exit."},
			{Name: "query", Help: "Query."},
			{Name: "queue", Help: "Queue."},
		},
	}

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 1, ReplaceEnd: 3, AppendSpace: true,
		Suggestions: []Suggestion{
			{Value: "quit", Label: "/quit", Detail: "Exit.", Usage: "/quit"},
			{Value: "query", Label: "/query", Detail: "Query.", Usage: "/query"},
			{Value: "queue", Label: "/queue", Detail: "Queue.", Usage: "/queue"},
		},
	}, Complete(cmds, "/qu", 3, domain.KindChannel))
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

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 12, ReplaceEnd: 12, AppendSpace: true,
		Suggestions: []Suggestion{
			{Value: "--reason", Label: "--reason", Detail: "Kick reason"},
		},
	}, Complete(cmds, "/kick botty ", 12, domain.KindChannel))
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

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 16, ReplaceEnd: 21, AppendSpace: true,
		Suggestions: []Suggestion{
			{Value: "--persona", Label: "--persona", Detail: "Persona text"},
		},
	}, Complete(cmds, "/invite model-a --per", 21, domain.KindChannel))
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

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 25, ReplaceEnd: 25, AppendSpace: true,
		Suggestions: []Suggestion{
			{Value: "json", Label: "json"},
			{Value: "yaml", Label: "yaml"},
		},
	}, Complete(cmds, "/config api-key --format ", 25, domain.KindChannel))
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

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 17, ReplaceEnd: 18, AppendSpace: true,
		Suggestions: []Suggestion{{Value: "json", Label: "json"}},
	}, Complete(cmds, "/config --format j", 18, domain.KindChannel))
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

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 27, ReplaceEnd: 27, AppendSpace: true,
		Suggestions: []Suggestion{{Value: "claude", Label: "claude"}},
	}, Complete(cmds, "/invite --persona friendly ", 27, domain.KindChannel))
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
		name string
		raw  string
		want Completion
	}{
		{
			name: "all subcommands", raw: "/admin ",
			want: Completion{Visible: true, ReplaceStart: 7, ReplaceEnd: 7, AppendSpace: true, Suggestions: []Suggestion{
				{Value: "ban", Label: "ban", Detail: "Ban a user"},
				{Value: "unban", Label: "unban", Detail: "Unban a user"},
				{Value: "mute", Label: "mute", Detail: "Mute a user"},
			}},
		},
		{
			name: "filtered by prefix", raw: "/admin mu",
			want: Completion{Visible: true, ReplaceStart: 7, ReplaceEnd: 9, AppendSpace: true, Suggestions: []Suggestion{
				{Value: "mute", Label: "mute", Detail: "Mute a user"},
			}},
		},
		{
			name: "no match", raw: "/admin x",
			want: Completion{Visible: true, ReplaceStart: 7, ReplaceEnd: 8, AppendSpace: true, Suggestions: []Suggestion{}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Complete(cmds, tt.raw, len([]rune(tt.raw)), domain.KindChannel))
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
		name string
		raw  string
		want Completion
	}{
		{
			name: "all flags offered", raw: "/config ",
			want: Completion{Visible: true, ReplaceStart: 8, ReplaceEnd: 8, AppendSpace: true, Suggestions: []Suggestion{
				{Value: "--api-key", Label: "--api-key", Detail: "API key"},
				{Value: "--theme", Label: "--theme", Detail: "Theme"},
			}},
		},
		{
			name: "used flag excluded", raw: "/config --api-key secret ",
			want: Completion{Visible: true, ReplaceStart: 25, ReplaceEnd: 25, AppendSpace: true, Suggestions: []Suggestion{
				{Value: "--theme", Label: "--theme", Detail: "Theme"},
			}},
		},
		{
			name: "all flags used", raw: "/config --api-key secret --theme dark ",
			want: Completion{Visible: true, ReplaceStart: 38, ReplaceEnd: 38},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Complete(cmds, tt.raw, len([]rune(tt.raw)), domain.KindChannel))
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
		name string
		raw  string
		want Completion
	}{
		{
			name: "subcommand names after parent", raw: "/config ",
			want: Completion{Visible: true, ReplaceStart: 8, ReplaceEnd: 8, AppendSpace: true, Suggestions: []Suggestion{
				{Value: "set", Label: "set", Detail: "Set a value"},
				{Value: "get", Label: "get", Detail: "Get a value"},
				{Value: "reset", Label: "reset", Detail: "Reset config"},
				{Value: "--format", Label: "--format", Detail: "Output format"},
			}},
		},
		{
			name: "subcommand names filtered", raw: "/config s",
			want: Completion{Visible: true, ReplaceStart: 8, ReplaceEnd: 9, AppendSpace: true, Suggestions: []Suggestion{
				{Value: "set", Label: "set", Detail: "Set a value"},
				{Value: "reset", Label: "reset", Detail: "Reset config"},
			}},
		},
		{
			name: "child positional after subcommand selected", raw: "/config set ",
			want: Completion{Visible: true, ReplaceStart: 12, ReplaceEnd: 12, AppendSpace: true, Suggestions: []Suggestion{
				{Value: "api-key", Label: "api-key"},
				{Value: "theme", Label: "theme"},
			}},
		},
		{
			name: "ancestor flag suggested on child", raw: "/config set --",
			want: Completion{Visible: true, ReplaceStart: 12, ReplaceEnd: 14, AppendSpace: true, Suggestions: []Suggestion{
				{Value: "--format", Label: "--format", Detail: "Output format"},
			}},
		},
		{
			name: "child positional filtered", raw: "/config set th",
			want: Completion{Visible: true, ReplaceStart: 12, ReplaceEnd: 14, AppendSpace: true, Suggestions: []Suggestion{
				{Value: "theme", Label: "theme"},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Complete(cmds, tt.raw, len([]rune(tt.raw)), domain.KindChannel))
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

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 8, ReplaceEnd: 8, AppendSpace: true,
		Suggestions: []Suggestion{
			{Value: "set", Label: "set", Detail: "Set a value"},
			{Value: "get", Label: "get", Detail: "Get a value"},
			{Value: "--format", Label: "--format", Detail: "Output format"},
		},
	}, Complete(cmds, "/config ", 8, domain.KindChannel))
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

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 21, ReplaceEnd: 21, AppendSpace: true,
		Suggestions: []Suggestion{
			{Value: "json", Label: "json"},
			{Value: "yaml", Label: "yaml"},
		},
	}, Complete(cmds, "/config set --format ", 21, domain.KindChannel))
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

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 26, ReplaceEnd: 28, AppendSpace: true,
		Suggestions: []Suggestion{},
	}, Complete(cmds, "/config set --format json --", 28, domain.KindChannel))
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

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 16, ReplaceEnd: 16, AppendSpace: true,
		Suggestions: []Suggestion{
			{Value: "api-key", Label: "api-key", Detail: "API key"},
			{Value: "poke-interval", Label: "poke-interval", Detail: "Poke interval"},
		},
	}, Complete(cmds, "/config --reset ", 16, domain.KindChannel))
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
		name string
		raw  string
		want Completion
	}{
		{
			name: "level 1: top children", raw: "/admin ",
			want: Completion{Visible: true, ReplaceStart: 7, ReplaceEnd: 7, AppendSpace: true, Suggestions: []Suggestion{
				{Value: "user", Label: "user", Detail: "User management"},
				{Value: "stats", Label: "stats", Detail: "Show stats"},
			}},
		},
		{
			name: "level 2: grandchildren", raw: "/admin user ",
			want: Completion{Visible: true, ReplaceStart: 12, ReplaceEnd: 12, AppendSpace: true, Suggestions: []Suggestion{
				{Value: "ban", Label: "ban", Detail: "Ban a user"},
				{Value: "unban", Label: "unban", Detail: "Unban a user"},
			}},
		},
		{
			name: "level 2: filtered grandchildren", raw: "/admin user b",
			want: Completion{Visible: true, ReplaceStart: 12, ReplaceEnd: 13, AppendSpace: true, Suggestions: []Suggestion{
				{Value: "ban", Label: "ban", Detail: "Ban a user"},
				{Value: "unban", Label: "unban", Detail: "Unban a user"},
			}},
		},
		{
			name: "level 3: leaf positional source", raw: "/admin user ban ",
			want: Completion{Visible: true, ReplaceStart: 16, ReplaceEnd: 16, Suggestions: []Suggestion{
				{Value: "alice", Label: "alice"},
				{Value: "bob", Label: "bob"},
			}},
		},
		{
			name: "level 3: leaf positional filtered", raw: "/admin user ban a",
			want: Completion{Visible: true, ReplaceStart: 16, ReplaceEnd: 17, Suggestions: []Suggestion{
				{Value: "alice", Label: "alice"},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Complete(cmds, tt.raw, len([]rune(tt.raw)), domain.KindChannel))
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

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 8, ReplaceEnd: 8,
		Suggestions: []Suggestion{
			{Value: "claude", Label: "claude"},
			{Value: "gemini", Label: "gemini"},
		},
	}, Complete(cmds, "/invite ", 8, domain.KindChannel))
}

func TestComplete_command_suggestions_include_aliases(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{Name: "join", Help: "Join a channel", Aliases: []string{"j", "jo"}, Positionals: []Positional{{Name: "channel"}}},
			{Name: "quit", Help: "Exit.", Aliases: []string{"q"}},
		},
	}

	tests := []struct {
		name string
		raw  string
		want Completion
	}{
		{
			name: "all suggestions include aliases",
			raw:  "/",
			want: Completion{
				Visible:      true,
				ReplaceStart: 1,
				ReplaceEnd:   1,
				AppendSpace:  true,
				Suggestions: []Suggestion{
					{Value: "join", Label: "/join", Detail: "Join a channel", Usage: "/join (/j, /jo) <channel>"},
					{Value: "j", Label: "/j", Detail: "Join a channel", Usage: "/join (/j, /jo) <channel>"},
					{Value: "jo", Label: "/jo", Detail: "Join a channel", Usage: "/join (/j, /jo) <channel>"},
					{Value: "quit", Label: "/quit", Detail: "Exit.", Usage: "/quit (/q)"},
					{Value: "q", Label: "/q", Detail: "Exit.", Usage: "/quit (/q)"},
				},
			},
		},
		{
			name: "alias prefix filters",
			raw:  "/j",
			want: Completion{
				Visible:      true,
				ReplaceStart: 1,
				ReplaceEnd:   2,
				AppendSpace:  true,
				Suggestions: []Suggestion{
					{Value: "join", Label: "/join", Detail: "Join a channel", Usage: "/join (/j, /jo) <channel>"},
					{Value: "j", Label: "/j", Detail: "Join a channel", Usage: "/join (/j, /jo) <channel>"},
					{Value: "jo", Label: "/jo", Detail: "Join a channel", Usage: "/join (/j, /jo) <channel>"},
				},
			},
		},
		{
			name: "alias exact match",
			raw:  "/q",
			want: Completion{
				Visible:      true,
				ReplaceStart: 1,
				ReplaceEnd:   2,
				AppendSpace:  true,
				Suggestions: []Suggestion{
					{Value: "quit", Label: "/quit", Detail: "Exit.", Usage: "/quit (/q)"},
					{Value: "q", Label: "/q", Detail: "Exit.", Usage: "/quit (/q)"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Complete(cmds, tt.raw, len([]rune(tt.raw)), domain.KindChannel))
		})
	}
}

func TestComplete_child_suggestions_include_aliases(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{
				Name: "config",
				Help: "Configuration",
				Children: []*Node{
					{Name: "set", Help: "Set a value", Aliases: []string{"s"}},
					{Name: "get", Help: "Get a value"},
				},
			},
		},
	}

	raw := "/config "
	require.Equal(t, Completion{
		Visible:      true,
		ReplaceStart: 8,
		ReplaceEnd:   8,
		AppendSpace:  true,
		Suggestions: []Suggestion{
			{Value: "set", Label: "set", Detail: "Set a value"},
			{Value: "s", Label: "s", Detail: "Set a value"},
			{Value: "get", Label: "get", Detail: "Get a value"},
		},
	}, Complete(cmds, raw, len([]rune(raw)), domain.KindChannel))
}

func TestComplete_alias_resolves_to_positional_suggestions(t *testing.T) {
	channels := LiteralSource(
		Suggestion{Value: "#general", Label: "#general"},
		Suggestion{Value: "#random", Label: "#random"},
	)

	cmds := Set{
		Commands: []*Node{
			{
				Name:    "join",
				Help:    "Join a channel",
				Aliases: []string{"j"},
				Positionals: []Positional{
					{Name: "channel", Source: channels},
				},
			},
		},
	}

	raw := "/j "
	require.Equal(t, Completion{
		Visible:      true,
		ReplaceStart: 3,
		ReplaceEnd:   3,
		AppendSpace:  false,
		Suggestions: []Suggestion{
			{Value: "#general", Label: "#general"},
			{Value: "#random", Label: "#random"},
		},
	}, Complete(cmds, raw, len([]rune(raw)), domain.KindChannel))
}

// --- Tool schema tests ---

type toolJoinCmd struct {
	Channel string `arg:"channel" help:"Channel to join"`
}

type toolTopicCmd struct {
	Topic []string `arg:"" optional:"" help:"Topic text"`
}

type toolKickCmd struct {
	Nick   string `arg:"" help:"Nick to kick"`
	Reason string `arg:"" optional:"" help:"Kick reason"`
}

type toolCountCmd struct {
	Count int  `arg:"" help:"Number of items"`
	Force bool `arg:"" optional:"" help:"Force operation"`
}

type toolSliceCmd struct {
	Tags []string `arg:"" optional:"" help:"Tags to apply"`
}

type toolNoToolCmd struct{}

type toolDescriberCmd struct{}

func (toolDescriberCmd) ToolDescription() string {
	return "rich multi-line description from method"
}

type toolGrammar struct {
	Join   toolJoinCmd      `cmd:"" tool:"" help:"Join a channel."`
	Topic  toolTopicCmd     `cmd:"" tool:"" help:"Set topic."`
	Kick   toolKickCmd      `cmd:"" tool:"" help:"Kick a nick."`
	NoTool toolNoToolCmd    `cmd:"" help:"Not a tool."`
	Count  toolCountCmd     `cmd:"" tool:"" help:"Count items."`
	Slice  toolSliceCmd     `cmd:"" tool:"" help:"Tag things."`
	Quit   toolDescriberCmd `cmd:"" tool:"Exit the application." help:"Quit."`
}

func toolSet(t *testing.T) Set {
	t.Helper()

	set, err := Build(&toolGrammar{})
	require.NoError(t, err)

	return set
}

func TestToolNodes_returns_only_tool_tagged_leaves(t *testing.T) {
	s := toolSet(t)
	nodes := s.ToolNodes()

	var names []string
	for _, n := range nodes {
		names = append(names, n.Name)
	}

	require.Equal(t, []string{"join", "topic", "kick", "count", "slice", "quit"}, names)
}

func TestToolName(t *testing.T) {
	tests := []struct {
		name string
		node Node
		want string
	}{
		{name: "simple", node: Node{Name: "join"}, want: "join"},
		{name: "with parent", node: func() Node {
			parent := &Node{Name: "config"}
			child := &Node{Name: "set", Parent: parent}
			return *child
		}(), want: "config set"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.node.ToolName())
		})
	}
}

func TestToolParameters(t *testing.T) {
	s := toolSet(t)

	t.Run("required positional", func(t *testing.T) {
		node := s.Find("join")
		params := node.ToolParameters()

		require.Equal(t, map[string]any{
			"type": "object",
			"properties": map[string]any{
				"channel": map[string]any{"type": "string", "description": "Channel to join"},
			},
			"required":             []string{"channel"},
			"additionalProperties": false,
		}, params)
	})

	t.Run("optional variadic", func(t *testing.T) {
		node := s.Find("topic")
		params := node.ToolParameters()

		require.Equal(t, map[string]any{
			"type": "object",
			"properties": map[string]any{
				"topic": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Topic text",
				},
			},
			"additionalProperties": false,
		}, params)
	})

	t.Run("mixed required and optional", func(t *testing.T) {
		node := s.Find("kick")
		require.Equal(t, map[string]any{
			"type": "object",
			"properties": map[string]any{
				"nick":   map[string]any{"type": "string", "description": "Nick to kick"},
				"reason": map[string]any{"type": "string", "description": "Kick reason"},
			},
			"required":             []string{"nick"},
			"additionalProperties": false,
		}, node.ToolParameters())
	})

	t.Run("int and bool types", func(t *testing.T) {
		node := s.Find("count")
		require.Equal(t, map[string]any{
			"type": "object",
			"properties": map[string]any{
				"count": map[string]any{"type": "integer", "description": "Number of items"},
				"force": map[string]any{"type": "boolean", "description": "Force operation"},
			},
			"required":             []string{"count"},
			"additionalProperties": false,
		}, node.ToolParameters())
	})

	t.Run("slice type", func(t *testing.T) {
		node := s.Find("slice")
		params := node.ToolParameters()

		props := params["properties"].(map[string]any)
		require.Equal(t, map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "Tags to apply",
		}, props["tags"])
	})
}

func TestToolValue(t *testing.T) {
	s := toolSet(t)

	t.Run("valid args", func(t *testing.T) {
		node := s.Find("join")
		raw := json.RawMessage(`{"channel": "#general"}`)

		val, err := node.ToolValue(raw)
		require.NoError(t, err)
		require.Equal(t, toolJoinCmd{Channel: "#general"}, val)
	})

	t.Run("empty args uses defaults", func(t *testing.T) {
		node := s.Find("topic")

		val, err := node.ToolValue(nil)
		require.NoError(t, err)
		require.Equal(t, toolTopicCmd{}, val)
	})

	t.Run("missing required arg", func(t *testing.T) {
		node := s.Find("join")
		raw := json.RawMessage(`{}`)

		_, err := node.ToolValue(raw)

		var me *MissingArgError
		require.ErrorAs(t, err, &me)
		require.Equal(t, "channel", me.Name)
	})

	t.Run("malformed JSON", func(t *testing.T) {
		node := s.Find("join")

		_, err := node.ToolValue(json.RawMessage(`{not json}`))
		require.Error(t, err)
	})

	t.Run("no factory", func(t *testing.T) {
		node := &Node{Name: "broken"}

		_, err := node.ToolValue(json.RawMessage(`{}`))
		require.ErrorContains(t, err, "no factory")
	})

	t.Run("non-leaf", func(t *testing.T) {
		node := &Node{Name: "parent", Children: []*Node{{Name: "child"}}}

		_, err := node.ToolValue(json.RawMessage(`{}`))
		require.ErrorContains(t, err, "not a tool leaf")
	})
}

func TestToolSchemaForType(t *testing.T) {
	tests := []struct {
		name string
		typ  reflect.Type
		want map[string]any
	}{
		{name: "string", typ: reflect.TypeFor[string](), want: map[string]any{"type": "string"}},
		{name: "int", typ: reflect.TypeFor[int](), want: map[string]any{"type": "integer"}},
		{name: "bool", typ: reflect.TypeFor[bool](), want: map[string]any{"type": "boolean"}},
		{name: "float64", typ: reflect.TypeFor[float64](), want: map[string]any{"type": "number"}},
		{
			name: "string slice",
			typ:  reflect.TypeFor[[]string](),
			want: map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
		},
		{name: "pointer to string", typ: reflect.TypeFor[*string](), want: map[string]any{"type": "string"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, toolSchemaForType(tt.typ))
		})
	}
}

func TestToolDescription_three_tiers(t *testing.T) {
	t.Run("tier 3: falls back to help", func(t *testing.T) {
		node := &Node{Help: "Join a channel.", ToolDesc: ""}

		require.Equal(t, "Join a channel.", node.ToolDescription(struct{}{}))
	})

	t.Run("tier 2: non-empty tool tag", func(t *testing.T) {
		node := &Node{Help: "Exit modeloff.", ToolDesc: "Shut down your instance."}

		require.Equal(t, "Shut down your instance.", node.ToolDescription(struct{}{}))
	})

	t.Run("tier 1: ToolDescriber interface", func(t *testing.T) {
		node := &Node{Help: "Quit.", ToolDesc: "Should be overridden."}

		require.Equal(t, "rich multi-line description from method", node.ToolDescription(toolDescriberCmd{}))
	})
}

func TestToolDescription_from_grammar(t *testing.T) {
	s := toolSet(t)

	t.Run("quit uses tier 1 ToolDescriber", func(t *testing.T) {
		node := s.Find("quit")
		desc := node.ToolDescription(node.NewZero())

		// toolDescriberCmd implements ToolDescriber, so tier 1 wins.
		require.Equal(t, "rich multi-line description from method", desc)
	})

	t.Run("join uses tier 3 help text", func(t *testing.T) {
		node := s.Find("join")
		desc := node.ToolDescription(node.NewZero())

		require.Equal(t, "Join a channel.", desc)
	})
}

func TestNewZero(t *testing.T) {
	s := toolSet(t)

	t.Run("returns zero-valued pointer", func(t *testing.T) {
		node := s.Find("join")
		require.Equal(t, &toolJoinCmd{}, node.NewZero())
	})

	t.Run("nil factory returns nil", func(t *testing.T) {
		node := &Node{Name: "broken"}
		require.Nil(t, node.NewZero())
	})
}
