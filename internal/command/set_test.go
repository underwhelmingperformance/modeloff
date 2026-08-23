package command

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	invjsonschema "github.com/invopop/jsonschema"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

// testCtx is a minimal [KindProvider] for engine-level tests. It
// returns `KindChannel` so kind-filtered commands are visible by
// default; tests that need to exercise kind filtering pass a real
// `domain.ChannelKind` to `complete` directly.
type testCtx struct{}

func (testCtx) ChannelKind() domain.ChannelKind { return domain.KindChannel }

var testCtxValue = testCtx{}

func TestMerge(t *testing.T) {
	tests := []struct {
		name  string
		sets  []Set[testCtx]
		wants []string
	}{
		{
			name: "nearest wins on duplicate name",
			sets: []Set[testCtx]{
				{Commands: []*Node[testCtx]{{Name: "join", Help: "child"}}},
				{Commands: []*Node[testCtx]{{Name: "join", Help: "parent"}, {Name: "list", Help: "list"}}},
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
			sets:  []Set[testCtx]{{}},
			wants: nil,
		},
		{
			name: "single non-empty set",
			sets: []Set[testCtx]{
				{Commands: []*Node[testCtx]{{Name: "quit"}}},
			},
			wants: []string{"quit"},
		},
		{
			name:  "two empty sets",
			sets:  []Set[testCtx]{{}, {}},
			wants: nil,
		},
		{
			name: "alias in higher-priority set shadows name in lower-priority set",
			sets: []Set[testCtx]{
				{Commands: []*Node[testCtx]{{Name: "join", Aliases: []string{"j"}}}},
				{Commands: []*Node[testCtx]{{Name: "j", Help: "bare j"}}},
			},
			wants: []string{"join"},
		},
		{
			name: "name in higher-priority set shadows alias in lower-priority set",
			sets: []Set[testCtx]{
				{Commands: []*Node[testCtx]{{Name: "j"}}},
				{Commands: []*Node[testCtx]{{Name: "join", Aliases: []string{"j"}}}},
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
		child := Set[testCtx]{Commands: []*Node[testCtx]{{Name: "join", Help: "child"}}}
		parent := Set[testCtx]{Commands: []*Node[testCtx]{{Name: "join", Help: "parent"}}}

		merged := Merge(child, parent)

		require.Equal(t, []nodeMeta{
			{Name: "join", Help: "child"},
		}, toNodeMetas(merged.Commands))
	})
}

func TestComplete_command_suggestions_carry_usage(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{Name: "join", Help: "Join channels", Positionals: []Positional[testCtx]{{Name: "channel"}}},
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
				Visible: true, ReplaceStart: 1, ReplaceEnd: 2, AppendSpace: true, TypedPrefix: "j",
				Suggestions: []Suggestion{
					{Value: "join", Label: "/join", Detail: "Join channels", Usage: "/join <channel>"},
				},
			},
		},
		{
			name: "exact match is still a suggestion",
			raw:  "/quit",
			want: Completion{
				Visible: true, ReplaceStart: 1, ReplaceEnd: 5, AppendSpace: true, TypedPrefix: "quit",
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
			require.Equal(t, tt.want, complete(cmds, testCtxValue, tt.raw, len([]rune(tt.raw)), domain.KindChannel, nil))
		})
	}
}

func TestComplete_filters_commands_by_channel_kind(t *testing.T) {
	channelOnly := domain.KindChannel

	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
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
			completion := complete(cmds, testCtxValue, "/", 1, tt.kind, nil)

			var names []string
			for _, s := range completion.Suggestions {
				names = append(names, s.Value)
			}

			require.Equal(t, tt.want, names)
		})
	}
}

func TestComplete_argument_sources_are_contextual(t *testing.T) {
	nickSource := SuggestionSource[testCtx](func(_ testCtx, _ InvocationState[testCtx]) SuggestionResult {
		return SuggestionResult{Suggestions: []Suggestion{
			{Value: "botty", Label: "botty"},
			{Value: "helper", Label: "helper"},
		}}
	})

	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "kick",
				Help: "Kick a nick",
				Positionals: []Positional[testCtx]{
					{Name: "nick", Source: nickSource},
				},
			},
		},
	}

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 6, ReplaceEnd: 7, AppendSpace: false, TypedPrefix: "h",
		Suggestions: []Suggestion{{Value: "helper", Label: "helper"}},
	}, complete(cmds, testCtxValue, "/kick h", 7, domain.KindChannel, nil))
}

func TestComplete_free_form_arguments_have_no_suggestions(t *testing.T) {
	nickSource := SuggestionSource[testCtx](func(_ testCtx, _ InvocationState[testCtx]) SuggestionResult {
		return SuggestionResult{Suggestions: []Suggestion{{Value: "botty", Label: "botty"}}}
	})

	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "msg",
				Help: "Direct message",
				Positionals: []Positional[testCtx]{
					{Name: "nick", Source: nickSource},
					{Name: "message", Variadic: true, Optional: true, Help: "Message body"},
				},
			},
		},
	}

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 11, ReplaceEnd: 16, TypedPrefix: "hello",
	}, complete(cmds, testCtxValue, "/msg botty hello", 16, domain.KindChannel, nil))
}

func TestComplete_passthrough_keeps_flags_available_until_the_first_value(t *testing.T) {
	for _, mode := range []PassthroughMode{PassthroughModeAll, PassthroughModePartial} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			cmds := Set[testCtx]{Commands: []*Node[testCtx]{
				{
					Name: "exec",
					Positionals: []Positional[testCtx]{
						{Name: "args", Optional: true, Variadic: true, Passthrough: mode},
					},
					Flags: []Flag[testCtx]{{Name: "--force", Optional: true, Boolean: true}},
				},
			}}

			beforeValue := complete(cmds, testCtxValue, "/exec --fo", 10, domain.KindChannel, nil)
			require.Equal(t, []Suggestion{{Value: "--force", Label: "--force"}}, beforeValue.Suggestions)

			afterValue := complete(cmds, testCtxValue, "/exec command --fo", 18, domain.KindChannel, nil)
			require.Equal(t, Completion{
				Visible:      true,
				ReplaceStart: 14,
				ReplaceEnd:   18,
				TypedPrefix:  "--fo",
			}, afterValue)
		})
	}
}

func TestComplete_attached_flag_values_do_not_start_passthrough(t *testing.T) {
	for _, mode := range []PassthroughMode{PassthroughModeAll, PassthroughModePartial} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			cmds := Set[testCtx]{Commands: []*Node[testCtx]{
				{
					Name: "exec",
					Positionals: []Positional[testCtx]{
						{Name: "args", Optional: true, Variadic: true, Passthrough: mode},
					},
					Flags: []Flag[testCtx]{
						{Name: "--force", Optional: true, Boolean: true},
						{Name: "--format", Optional: true},
					},
				},
			}}

			require.Equal(t, Completion{
				Visible:      true,
				ReplaceStart: 20,
				ReplaceEnd:   24,
				AppendSpace:  true,
				TypedPrefix:  "--fo",
				Suggestions: []Suggestion{
					{Value: "--force", Label: "--force"},
				},
			}, complete(cmds, testCtxValue, "/exec --format=json --fo", 24, domain.KindChannel, nil))
			require.Equal(t, Completion{
				Visible:      true,
				ReplaceStart: 6,
				ReplaceEnd:   19,
				AppendSpace:  true,
				TypedPrefix:  "--format=json",
				Suggestions:  []Suggestion{},
			}, complete(cmds, testCtxValue, "/exec --format=json", 19, domain.KindChannel, nil))
		})
	}
}

func TestComplete_composes_sources(t *testing.T) {
	localSource := SuggestionSource[testCtx](func(_ testCtx, _ InvocationState[testCtx]) SuggestionResult {
		return SuggestionResult{Suggestions: []Suggestion{{Value: "botty", Label: "botty", Detail: "test/model-a"}}}
	})

	liveSource := SuggestionSource[testCtx](func(_ testCtx, _ InvocationState[testCtx]) SuggestionResult {
		return SuggestionResult{Suggestions: []Suggestion{{Value: "anthropic/claude-3-haiku", Label: "anthropic/claude-3-haiku", Detail: "Claude Haiku"}}}
	})

	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "invite",
				Help: "Invite a model",
				Positionals: []Positional[testCtx]{
					{
						Name:   "model",
						Source: ComposeSources(localSource, liveSource),
					},
				},
				Flags: []Flag[testCtx]{
					{
						Name:     "--persona",
						Optional: true,
						Source:   LiteralSource[testCtx](Suggestion{Value: "--persona", Label: "--persona"}),
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
	}, complete(cmds, testCtxValue, "/invite ", 8, domain.KindChannel, nil))
}

func TestComplete_hides_completion_when_source_errors(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "add-model",
				Help: "Add a model",
				Positionals: []Positional[testCtx]{
					{
						Name: "model",
						Source: func(_ testCtx, _ InvocationState[testCtx]) SuggestionResult {
							return SuggestionResult{State: SuggestionStateError}
						},
					},
				},
			},
		},
	}

	require.Equal(t, Completion{}, complete(cmds, testCtxValue, "/add-model ", 11, domain.KindChannel, nil))
}

func TestComplete_composed_sources_hide_completion_only_when_all_sources_error(t *testing.T) {
	errored := SuggestionSource[testCtx](func(_ testCtx, _ InvocationState[testCtx]) SuggestionResult {
		return SuggestionResult{State: SuggestionStateError}
	})
	healthy := SuggestionSource[testCtx](func(_ testCtx, _ InvocationState[testCtx]) SuggestionResult {
		return SuggestionResult{Suggestions: []Suggestion{{Value: "botty", Label: "botty"}}}
	})

	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "invite",
				Help: "Invite a model",
				Positionals: []Positional[testCtx]{
					{Name: "model", Source: ComposeSources(errored, healthy)},
				},
			},
		},
	}

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 8, ReplaceEnd: 8, AppendSpace: false,
		Suggestions: []Suggestion{{Value: "botty", Label: "botty"}},
	}, complete(cmds, testCtxValue, "/invite ", 8, domain.KindChannel, nil))

	cmds.Commands[0].Positionals[0].Source = ComposeSources(errored, errored)

	require.Equal(t, Completion{}, complete(cmds, testCtxValue, "/invite ", 8, domain.KindChannel, nil))
}

func TestNode_Usage(t *testing.T) {
	tests := []struct {
		name string
		node Node[testCtx]
		want string
	}{
		{
			name: "no args",
			node: Node[testCtx]{Name: "quit"},
			want: "",
		},
		{
			name: "required positional",
			node: Node[testCtx]{
				Name:        "join",
				Positionals: []Positional[testCtx]{{Name: "channel"}},
			},
			want: "<channel>",
		},
		{
			name: "optional positional",
			node: Node[testCtx]{
				Name:        "topic",
				Positionals: []Positional[testCtx]{{Name: "text", Optional: true}},
			},
			want: "[text]",
		},
		{
			name: "mixed positionals",
			node: Node[testCtx]{
				Name: "msg",
				Positionals: []Positional[testCtx]{
					{Name: "nick"},
					{Name: "message", Optional: true},
				},
			},
			want: "<nick> [message]",
		},
		{
			name: "with flag",
			node: Node[testCtx]{
				Name:        "invite",
				Positionals: []Positional[testCtx]{{Name: "model", Optional: true}},
				Flags:       []Flag[testCtx]{{Name: "--persona", Variadic: true}},
			},
			want: "[model] [--persona <persona>]",
		},
		{
			name: "with children",
			node: Node[testCtx]{
				Name:     "admin",
				Children: []*Node[testCtx]{{Name: "ban"}},
			},
			want: "<command>",
		},
		{
			name: "inherits ancestor flags",
			node: func() Node[testCtx] {
				parent := &Node[testCtx]{
					Name:  "config",
					Flags: []Flag[testCtx]{{Name: "--format", Variadic: true}},
				}

				child := &Node[testCtx]{
					Parent:      parent,
					Name:        "set",
					Positionals: []Positional[testCtx]{{Name: "key"}},
				}

				return *child
			}(),
			want: "<key> [--format <format>]",
		},
		{
			name: "no positionals or flags",
			node: Node[testCtx]{
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
	nickSource := SuggestionSource[testCtx](func(_ testCtx, _ InvocationState[testCtx]) SuggestionResult {
		return SuggestionResult{Suggestions: []Suggestion{
			{Value: "alice", Label: "alice"},
			{Value: "bob", Label: "bob"},
		}}
	})

	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "kick",
				Help: "Kick a nick",
				Positionals: []Positional[testCtx]{
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
			want: Completion{Visible: true, ReplaceStart: 1, ReplaceEnd: 2, AppendSpace: true, TypedPrefix: "k", Suggestions: []Suggestion{
				{Value: "kick", Label: "/kick", Detail: "Kick a nick", Usage: "/kick <nick>"},
			}},
		},
		{
			name: "cursor at 1 shows command suggestions", raw: "/kick alice", cursor: 1,
			want: Completion{Visible: true, ReplaceStart: 1, ReplaceEnd: 5, AppendSpace: true, TypedPrefix: "kick", Suggestions: []Suggestion{
				{Value: "kick", Label: "/kick", Detail: "Kick a nick", Usage: "/kick <nick>"},
			}},
		},
		{
			name: "cursor after space shows argument suggestions", raw: "/kick ", cursor: 6,
			want: Completion{Visible: true, ReplaceStart: 6, ReplaceEnd: 6, Suggestions: allNicks},
		},
		{
			name: "cursor mid-argument filters", raw: "/kick al", cursor: 8,
			want: Completion{Visible: true, ReplaceStart: 6, ReplaceEnd: 8, TypedPrefix: "al", Suggestions: []Suggestion{{Value: "alice", Label: "alice"}}},
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
			want: Completion{Visible: true, ReplaceStart: 8, ReplaceEnd: 9, TypedPrefix: "a", Suggestions: []Suggestion{{Value: "alice", Label: "alice"}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, complete(cmds, testCtxValue, tt.raw, tt.cursor, domain.KindChannel, nil))
		})
	}
}

func TestComplete_unknown_command_has_no_suggestions(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{{Name: "quit", Help: "Exit."}},
	}

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 9, ReplaceEnd: 12, TypedPrefix: "arg",
	}, complete(cmds, testCtxValue, "/unknown arg", 12, domain.KindChannel, nil))
}

func TestComplete_contains_match(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{Name: "claude-3-haiku", Help: "Haiku model"},
			{Name: "quit", Help: "Exit."},
		},
	}

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 1, ReplaceEnd: 5, AppendSpace: true, TypedPrefix: "aiku",
		Suggestions: []Suggestion{
			{Value: "claude-3-haiku", Label: "/claude-3-haiku", Detail: "Haiku model", Usage: "/claude-3-haiku"},
		},
	}, complete(cmds, testCtxValue, "/aiku", 5, domain.KindChannel, nil))
}

func TestNode_Find(t *testing.T) {
	child := &Node[testCtx]{Name: "ban"}
	parent := &Node[testCtx]{
		Name:     "admin",
		Children: []*Node[testCtx]{child, {Name: "unban"}},
	}

	require.Equal(t, child, parent.Find("ban"))
	require.Nil(t, parent.Find("nonexistent"))
	require.Nil(t, (&Node[testCtx]{Name: "empty"}).Find("anything"))
}

func TestNode_Leaf(t *testing.T) {
	require.True(t, (&Node[testCtx]{Name: "quit"}).Leaf())
	require.False(t, (&Node[testCtx]{Name: "admin", Children: []*Node[testCtx]{{Name: "ban"}}}).Leaf())
}

func TestParseValue_no_factory(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{{Name: "broken"}},
	}

	_, err := cmds.ParseValue("/broken")

	var noFactory *NoFactoryError
	require.ErrorAs(t, err, &noFactory)
	require.Equal(t, &NoFactoryError{Path: cmds.Commands[0].Path()}, noFactory)
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

	child, err := Build[testCtx](&childGrammar{})
	require.NoError(t, err)

	parent, err := Build[testCtx](&parentGrammar{})
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
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{{Name: "quit", Help: "Exit."}},
	}

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 1, ReplaceEnd: 2, AppendSpace: true, TypedPrefix: " ",
		Suggestions: []Suggestion{},
	}, complete(cmds, testCtxValue, "/ ", 2, domain.KindChannel, nil))
}

func TestComplete_cursor_mid_command_name(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{Name: "quit", Help: "Exit."},
			{Name: "query", Help: "Query."},
		},
	}

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 1, ReplaceEnd: 5, AppendSpace: true, TypedPrefix: "quit",
		Suggestions: []Suggestion{
			{Value: "quit", Label: "/quit", Detail: "Exit.", Usage: "/quit"},
		},
	}, complete(cmds, testCtxValue, "/quit", 3, domain.KindChannel, nil))
}

func TestComplete_multiple_prefix_matches(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{Name: "quit", Help: "Exit."},
			{Name: "query", Help: "Query."},
			{Name: "queue", Help: "Queue."},
		},
	}

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 1, ReplaceEnd: 3, AppendSpace: true, TypedPrefix: "qu",
		Suggestions: []Suggestion{
			{Value: "quit", Label: "/quit", Detail: "Exit.", Usage: "/quit"},
			{Value: "query", Label: "/query", Detail: "Query.", Usage: "/query"},
			{Value: "queue", Label: "/queue", Detail: "Queue.", Usage: "/queue"},
		},
	}, complete(cmds, testCtxValue, "/qu", 3, domain.KindChannel, nil))
}

func TestComplete_flag_name_after_positionals(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name:        "kick",
				Help:        "Kick a nick",
				Positionals: []Positional[testCtx]{{Name: "nick"}},
				Flags: []Flag[testCtx]{
					{Name: "--reason", Optional: true, Help: "Kick reason"},
				},
			},
		},
	}

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 12, ReplaceEnd: 12, AppendSpace: true, EnterSubmits: true,
		Suggestions: []Suggestion{
			{Value: "--reason", Label: "--reason", Detail: "Kick reason"},
		},
	}, complete(cmds, testCtxValue, "/kick botty ", 12, domain.KindChannel, nil))
}

func TestComplete_flag_name_prefix_filters(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "invite",
				Help: "Invite a model",
				Positionals: []Positional[testCtx]{
					{Name: "model", Optional: true},
				},
				Flags: []Flag[testCtx]{
					{Name: "--persona", Optional: true, Help: "Persona text"},
					{Name: "--priority", Optional: true, Help: "Priority level"},
				},
			},
		},
	}

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 16, ReplaceEnd: 21, AppendSpace: true, TypedPrefix: "--per",
		Suggestions: []Suggestion{
			{Value: "--persona", Label: "--persona", Detail: "Persona text"},
		},
	}, complete(cmds, testCtxValue, "/invite model-a --per", 21, domain.KindChannel, nil))
}

func TestComplete_flag_value_uses_source(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "config",
				Help: "Configure",
				Positionals: []Positional[testCtx]{
					{Name: "key", Source: LiteralSource[testCtx](
						Suggestion{Value: "api-key", Label: "api-key"},
						Suggestion{Value: "theme", Label: "theme"},
					)},
				},
				Flags: []Flag[testCtx]{
					{
						Name:     "--format",
						Optional: true,
						Help:     "Output format",
						Source: LiteralSource[testCtx](
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
	}, complete(cmds, testCtxValue, "/config api-key --format ", 25, domain.KindChannel, nil))
}

func TestComplete_flag_value_filters_by_prefix(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "config",
				Help: "Configure",
				Flags: []Flag[testCtx]{
					{
						Name:     "--format",
						Optional: true,
						Source: LiteralSource[testCtx](
							Suggestion{Value: "json", Label: "json"},
							Suggestion{Value: "yaml", Label: "yaml"},
						),
					},
				},
			},
		},
	}

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 17, ReplaceEnd: 18, AppendSpace: true, TypedPrefix: "j",
		Suggestions: []Suggestion{{Value: "json", Label: "json"}},
	}, complete(cmds, testCtxValue, "/config --format j", 18, domain.KindChannel, nil))
}

func TestComplete_flags_interleaved_with_positionals(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "invite",
				Help: "Invite a model",
				Positionals: []Positional[testCtx]{
					{Name: "model", Optional: true, Source: LiteralSource[testCtx](
						Suggestion{Value: "claude", Label: "claude"},
					)},
				},
				Flags: []Flag[testCtx]{
					{Name: "--persona", Optional: true, Help: "Persona"},
				},
			},
		},
	}

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 27, ReplaceEnd: 27, AppendSpace: true,
		Suggestions: []Suggestion{{Value: "claude", Label: "claude"}},
	}, complete(cmds, testCtxValue, "/invite --persona friendly ", 27, domain.KindChannel, nil))
}

func TestComplete_subcommand_names(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "admin",
				Help: "Admin commands",
				Children: []*Node[testCtx]{
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
				{Value: "ban", Label: "ban", Detail: "Ban a user", Usage: "ban"},
				{Value: "unban", Label: "unban", Detail: "Unban a user", Usage: "unban"},
				{Value: "mute", Label: "mute", Detail: "Mute a user", Usage: "mute"},
			}},
		},
		{
			name: "filtered by prefix", raw: "/admin mu",
			want: Completion{Visible: true, ReplaceStart: 7, ReplaceEnd: 9, AppendSpace: true, TypedPrefix: "mu", Suggestions: []Suggestion{
				{Value: "mute", Label: "mute", Detail: "Mute a user", Usage: "mute"},
			}},
		},
		{
			name: "no match", raw: "/admin x",
			want: Completion{Visible: true, ReplaceStart: 7, ReplaceEnd: 8, AppendSpace: true, TypedPrefix: "x", Suggestions: []Suggestion{}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, complete(cmds, testCtxValue, tt.raw, len([]rune(tt.raw)), domain.KindChannel, nil))
		})
	}
}

func TestComplete_flag_only_command(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "config",
				Help: "Configure",
				Flags: []Flag[testCtx]{
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
			want: Completion{Visible: true, ReplaceStart: 8, ReplaceEnd: 8, AppendSpace: true, EnterSubmits: true, Suggestions: []Suggestion{
				{Value: "--api-key", Label: "--api-key", Detail: "API key"},
				{Value: "--theme", Label: "--theme", Detail: "Theme"},
			}},
		},
		{
			name: "used flag excluded", raw: "/config --api-key secret ",
			want: Completion{Visible: true, ReplaceStart: 25, ReplaceEnd: 25, AppendSpace: true, EnterSubmits: true, Suggestions: []Suggestion{
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
			require.Equal(t, tt.want, complete(cmds, testCtxValue, tt.raw, len([]rune(tt.raw)), domain.KindChannel, nil))
		})
	}
}

func TestComplete_subcommand_recurses_into_child(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "config",
				Help: "Configuration",
				Flags: []Flag[testCtx]{
					{Name: "--format", Optional: true, Help: "Output format"},
				},
				Children: []*Node[testCtx]{
					{
						Name: "set",
						Help: "Set a value",
						Positionals: []Positional[testCtx]{
							{
								Name: "key",
								Source: LiteralSource[testCtx](
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
				{Value: "set", Label: "set", Detail: "Set a value", Usage: "set <key> [--format]"},
				{Value: "get", Label: "get", Detail: "Get a value", Usage: "get [--format]"},
				{Value: "reset", Label: "reset", Detail: "Reset config", Usage: "reset [--format]"},
				{Value: "--format", Label: "--format", Detail: "Output format"},
			}},
		},
		{
			name: "subcommand names filtered", raw: "/config s",
			want: Completion{Visible: true, ReplaceStart: 8, ReplaceEnd: 9, AppendSpace: true, TypedPrefix: "s", Suggestions: []Suggestion{
				{Value: "set", Label: "set", Detail: "Set a value", Usage: "set <key> [--format]"},
				{Value: "reset", Label: "reset", Detail: "Reset config", Usage: "reset [--format]"},
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
			want: Completion{Visible: true, ReplaceStart: 12, ReplaceEnd: 14, AppendSpace: true, TypedPrefix: "--", Suggestions: []Suggestion{
				{Value: "--format", Label: "--format", Detail: "Output format"},
			}},
		},
		{
			name: "child positional filtered", raw: "/config set th",
			want: Completion{Visible: true, ReplaceStart: 12, ReplaceEnd: 14, AppendSpace: true, TypedPrefix: "th", Suggestions: []Suggestion{
				{Value: "theme", Label: "theme"},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, complete(cmds, testCtxValue, tt.raw, len([]rune(tt.raw)), domain.KindChannel, nil))
		})
	}
}

func TestComplete_group_node_combines_child_and_flag_suggestions(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "config",
				Flags: []Flag[testCtx]{
					{Name: "--format", Optional: true, Help: "Output format"},
				},
				Children: []*Node[testCtx]{
					{Name: "set", Help: "Set a value"},
					{Name: "get", Help: "Get a value"},
				},
			},
		},
	}

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 8, ReplaceEnd: 8, AppendSpace: true,
		Suggestions: []Suggestion{
			{Value: "set", Label: "set", Detail: "Set a value", Usage: "set [--format]"},
			{Value: "get", Label: "get", Detail: "Get a value", Usage: "get [--format]"},
			{Value: "--format", Label: "--format", Detail: "Output format"},
		},
	}, complete(cmds, testCtxValue, "/config ", 8, domain.KindChannel, nil))
}

func TestComplete_ancestor_flag_value_uses_source(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "config",
				Flags: []Flag[testCtx]{
					{
						Name:     "--format",
						Optional: true,
						Source: LiteralSource[testCtx](
							Suggestion{Value: "json", Label: "json"},
							Suggestion{Value: "yaml", Label: "yaml"},
						),
					},
				},
				Children: []*Node[testCtx]{
					{
						Name: "set",
						Positionals: []Positional[testCtx]{
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
	}, complete(cmds, testCtxValue, "/config set --format ", 21, domain.KindChannel, nil))
}

func TestComplete_used_ancestor_flags_are_excluded(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "config",
				Flags: []Flag[testCtx]{
					{Name: "--format", Optional: true, Help: "Output format"},
				},
				Children: []*Node[testCtx]{
					{Name: "set", Help: "Set"},
				},
			},
		},
	}

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 26, ReplaceEnd: 28, AppendSpace: true, TypedPrefix: "--",
		Suggestions: []Suggestion{},
	}, complete(cmds, testCtxValue, "/config set --format json --", 28, domain.KindChannel, nil))
}

func TestComplete_bool_flag_does_not_expect_a_value(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "config",
				Flags: []Flag[testCtx]{
					{Name: "--reset", Boolean: true, Optional: true, Help: "Reset"},
				},
				Children: []*Node[testCtx]{
					{Name: "api-key", Help: "API key"},
					{Name: "poke-interval", Help: "Poke interval"},
				},
			},
		},
	}

	require.Equal(t, Completion{
		Visible: true, ReplaceStart: 16, ReplaceEnd: 16, AppendSpace: true,
		Suggestions: []Suggestion{
			{Value: "api-key", Label: "api-key", Detail: "API key", Usage: "api-key [--reset]"},
			{Value: "poke-interval", Label: "poke-interval", Detail: "Poke interval", Usage: "poke-interval [--reset]"},
		},
	}, complete(cmds, testCtxValue, "/config --reset ", 16, domain.KindChannel, nil))
}

func TestComplete_deep_nesting_walks_into_grandchildren(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "admin",
				Help: "Admin commands",
				Children: []*Node[testCtx]{
					{
						Name: "user",
						Help: "User management",
						Children: []*Node[testCtx]{
							{
								Name: "ban",
								Help: "Ban a user",
								Positionals: []Positional[testCtx]{
									{
										Name: "nick",
										Source: LiteralSource[testCtx](
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
				{Value: "user", Label: "user", Detail: "User management", Usage: "user <command>"},
				{Value: "stats", Label: "stats", Detail: "Show stats", Usage: "stats"},
			}},
		},
		{
			name: "level 2: grandchildren", raw: "/admin user ",
			want: Completion{Visible: true, ReplaceStart: 12, ReplaceEnd: 12, AppendSpace: true, Suggestions: []Suggestion{
				{Value: "ban", Label: "ban", Detail: "Ban a user", Usage: "ban <nick>"},
				{Value: "unban", Label: "unban", Detail: "Unban a user", Usage: "unban"},
			}},
		},
		{
			name: "level 2: filtered grandchildren", raw: "/admin user b",
			want: Completion{Visible: true, ReplaceStart: 12, ReplaceEnd: 13, AppendSpace: true, TypedPrefix: "b", Suggestions: []Suggestion{
				{Value: "ban", Label: "ban", Detail: "Ban a user", Usage: "ban <nick>"},
				{Value: "unban", Label: "unban", Detail: "Unban a user", Usage: "unban"},
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
			want: Completion{Visible: true, ReplaceStart: 16, ReplaceEnd: 17, TypedPrefix: "a", Suggestions: []Suggestion{
				{Value: "alice", Label: "alice"},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, complete(cmds, testCtxValue, tt.raw, len([]rune(tt.raw)), domain.KindChannel, nil))
		})
	}
}

func TestComplete_optional_positional_with_source(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "invite",
				Help: "Invite a model",
				Positionals: []Positional[testCtx]{
					{
						Name:     "model",
						Optional: true,
						Source: LiteralSource[testCtx](
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
	}, complete(cmds, testCtxValue, "/invite ", 8, domain.KindChannel, nil))
}

func TestComplete_command_suggestions_include_aliases(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{Name: "join", Help: "Join a channel", Aliases: []string{"j", "jo"}, Positionals: []Positional[testCtx]{{Name: "channel"}}},
			{Name: "quit", Help: "Exit.", Aliases: []string{"q"}},
		},
	}

	joinSuggestion := Suggestion{
		Value:   "join",
		Label:   "/join (/j, /jo)",
		Detail:  "Join a channel",
		Usage:   "/join (/j, /jo) <channel>",
		Aliases: []string{"j", "jo"},
	}

	quitSuggestion := Suggestion{
		Value:   "quit",
		Label:   "/quit (/q)",
		Detail:  "Exit.",
		Usage:   "/quit (/q)",
		Aliases: []string{"q"},
	}

	tests := []struct {
		name string
		raw  string
		want Completion
	}{
		{
			name: "all commands appear once with aliases inline",
			raw:  "/",
			want: Completion{
				Visible:      true,
				ReplaceStart: 1,
				ReplaceEnd:   1,
				AppendSpace:  true,
				Suggestions:  []Suggestion{joinSuggestion, quitSuggestion},
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
				TypedPrefix:  "j",
				Suggestions:  []Suggestion{joinSuggestion},
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
				TypedPrefix:  "q",
				Suggestions:  []Suggestion{quitSuggestion},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, complete(cmds, testCtxValue, tt.raw, len([]rune(tt.raw)), domain.KindChannel, nil))
		})
	}
}

func TestComplete_child_suggestions_include_aliases(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "config",
				Help: "Configuration",
				Children: []*Node[testCtx]{
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
			{Value: "set", Label: "set (s)", Detail: "Set a value", Usage: "set (s)", Aliases: []string{"s"}},
			{Value: "get", Label: "get", Detail: "Get a value", Usage: "get"},
		},
	}, complete(cmds, testCtxValue, raw, len([]rune(raw)), domain.KindChannel, nil))
}

func TestComplete_child_suggestion_usage_includes_args(t *testing.T) {
	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name: "config",
				Help: "Configuration",
				Children: []*Node[testCtx]{
					{
						Name:    "set",
						Help:    "Set a value",
						Aliases: []string{"s"},
						Positionals: []Positional[testCtx]{
							{Name: "key"},
							{Name: "value"},
						},
					},
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
			{
				Value:   "set",
				Label:   "set (s)",
				Detail:  "Set a value",
				Usage:   "set (s) <key> <value>",
				Aliases: []string{"s"},
			},
		},
	}, complete(cmds, testCtxValue, raw, len([]rune(raw)), domain.KindChannel, nil))
}

func TestComplete_alias_resolves_to_positional_suggestions(t *testing.T) {
	channels := LiteralSource[testCtx](
		Suggestion{Value: "#general", Label: "#general"},
		Suggestion{Value: "#random", Label: "#random"},
	)

	cmds := Set[testCtx]{
		Commands: []*Node[testCtx]{
			{
				Name:    "join",
				Help:    "Join a channel",
				Aliases: []string{"j"},
				Positionals: []Positional[testCtx]{
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
	}, complete(cmds, testCtxValue, raw, len([]rune(raw)), domain.KindChannel, nil))
}

// --- Capability filter tests ---

// capGrammar exercises `caps:` filtering in suggestions and parsing.
// Three commands with three different requirement shapes; held
// capabilities are varied per test case.
type capGrammar struct {
	Open     capOpenCmd     `cmd:"" help:"Always visible."`
	OperOnly capOperOnlyCmd `cmd:"" caps:"alpha" help:"Visible when alpha held."`
	Both     capBothCmd     `cmd:"" caps:"alpha,beta" help:"Visible when alpha+beta held."`
}

type capOpenCmd struct{}
type capOperOnlyCmd struct{}
type capBothCmd struct{}

func TestCommandSuggestions_FiltersByCapabilities(t *testing.T) {
	set, err := Build[testCtx](&capGrammar{})
	require.NoError(t, err)

	tests := []struct {
		name   string
		holder CapabilityHolder
		want   []string
	}{
		{name: "nil holder shows only unrestricted", holder: nil, want: []string{"open"}},
		{name: "empty holder shows only unrestricted", holder: held(), want: []string{"open"}},
		{name: "alpha shows alpha-gated", holder: held("alpha"), want: []string{"open", "oper-only"}},
		{name: "alpha+beta shows all", holder: held("alpha", "beta"), want: []string{"open", "oper-only", "both"}},
		{name: "beta only excludes alpha-gated", holder: held("beta"), want: []string{"open"}},
		{name: "unrelated cap shows only unrestricted", holder: held("gamma"), want: []string{"open"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := commandSuggestions(set, domain.KindChannel, tt.holder)
			names := make([]string, 0, len(got))
			for _, s := range got {
				names = append(names, s.Value)
			}
			require.Equal(t, tt.want, names)
		})
	}
}

func TestVisibleCommands_FiltersByCapabilities(t *testing.T) {
	set, err := Build[testCtx](&capGrammar{})
	require.NoError(t, err)

	tests := []struct {
		name   string
		holder CapabilityHolder
		want   []string
	}{
		{name: "nil holder shows only unrestricted", holder: nil, want: []string{"open"}},
		{name: "alpha shows alpha-gated", holder: held("alpha"), want: []string{"open", "oper-only"}},
		{name: "alpha+beta shows all", holder: held("alpha", "beta"), want: []string{"open", "oper-only", "both"}},
		{name: "no-capabilities holder shows only unrestricted", holder: NoCapabilities(), want: []string{"open"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := VisibleCommands(set, tt.holder)
			names := make([]string, 0, len(got))
			for _, n := range got {
				names = append(names, n.Name)
			}
			require.Equal(t, tt.want, names)
		})
	}
}

func TestReflect_ParsesCapsTag_OnGrammar(t *testing.T) {
	set, err := Build[testCtx](&capGrammar{})
	require.NoError(t, err)

	byName := map[string][]Capability{}
	for _, n := range set.Commands {
		byName[n.Name] = n.RequiredCapabilities
	}

	require.Nil(t, byName["open"], "open command has no required caps")
	require.Equal(t, []Capability{"alpha"}, byName["oper-only"])
	require.Equal(t, []Capability{"alpha", "beta"}, byName["both"])
}

// --- Tool schema tests ---

type toolJoinCmd struct {
	Channel string `arg:"channel" help:"Channel to join"`
}

type toolTopicCmd struct {
	Topic []string `arg:"" optional:"" tool:"" help:"Topic text"`
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
	Tags []string `arg:"" optional:"" tool:"Tag values exposed to the model" help:"Tags to apply"`
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

func toolSet(t *testing.T) Set[testCtx] {
	t.Helper()

	set, err := Build[testCtx](&toolGrammar{})
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
		node Node[testCtx]
		want string
	}{
		{name: "simple", node: Node[testCtx]{Name: "join"}, want: "join"},
		{name: "with parent", node: func() Node[testCtx] {
			parent := &Node[testCtx]{Name: "config"}
			child := &Node[testCtx]{Name: "set", Parent: parent}
			return *child
		}(), want: "config set"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.node.ToolName())
		})
	}
}

// TestToolParameters pins the strict-mode JSON Schema shape
// emitted for each leaf's parameters. Every property name appears
// in `required` and optional fields carry a nullable type union
// so providers that enforce strict function-call schemas (Azure
// OpenAI) accept the schema.
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
					"type":        []any{"array", "null"},
					"items":       map[string]any{"type": "string"},
					"description": "Topic text",
				},
			},
			"required":             []string{"topic"},
			"additionalProperties": false,
		}, params)
	})

	t.Run("mixed required and optional", func(t *testing.T) {
		node := s.Find("kick")
		require.Equal(t, map[string]any{
			"type": "object",
			"properties": map[string]any{
				"nick":   map[string]any{"type": "string", "description": "Nick to kick"},
				"reason": map[string]any{"type": []any{"string", "null"}, "description": "Kick reason"},
			},
			"required":             []string{"nick", "reason"},
			"additionalProperties": false,
		}, node.ToolParameters())
	})

	t.Run("int and bool types", func(t *testing.T) {
		node := s.Find("count")
		require.Equal(t, map[string]any{
			"type": "object",
			"properties": map[string]any{
				"count": map[string]any{"type": "integer", "description": "Number of items"},
				"force": map[string]any{"type": []any{"boolean", "null"}, "description": "Force operation"},
			},
			"required":             []string{"count", "force"},
			"additionalProperties": false,
		}, node.ToolParameters())
	})

	t.Run("slice type", func(t *testing.T) {
		node := s.Find("slice")
		params := node.ToolParameters()

		props := params["properties"].(map[string]any)
		require.Equal(t, map[string]any{
			"type":        []any{"array", "null"},
			"items":       map[string]any{"type": "string"},
			"description": "Tag values exposed to the model",
		}, props["tags"])
	})
}

// strictStyle and strictSpan stand in for the nested-struct shape
// in protocol.ReplySpan / protocol.ReplyStyle. The reflected schema
// must satisfy OpenAI strict-mode invariants on every object level
// — `additionalProperties: false`, every property in `required`,
// optional fields nullable — or providers such as Azure reject the
// tool with `invalid_function_parameters`.
type strictStyle struct {
	Bold bool   `json:"bold,omitempty"`
	FG   *uint8 `json:"fg,omitempty"`
	BG   *uint8 `json:"bg,omitempty"`
}

type strictSpan struct {
	Text  string       `json:"text"`
	Style *strictStyle `json:"style,omitempty"`
}

func TestStructSchemaViaReflector_strict(t *testing.T) {
	schema, err := schemaViaReflector(reflect.TypeFor[strictSpan]())
	require.NoError(t, err)

	require.Equal(t, map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"text", "style"},
		"properties": map[string]any{
			"text": map[string]any{"type": "string"},
			"style": map[string]any{
				"type":                 []any{"object", "null"},
				"additionalProperties": false,
				"required":             []string{"bg", "bold", "fg"},
				"properties": map[string]any{
					"bold": map[string]any{"type": []any{"boolean", "null"}},
					"fg":   map[string]any{"type": []any{"integer", "null"}},
					"bg":   map[string]any{"type": []any{"integer", "null"}},
				},
			},
		},
	}, schema)
}

type constrainedStyle struct {
	FG *uint8 `json:"fg,omitempty" jsonschema:"minimum=0,maximum=15"`
	BG *uint8 `json:"bg,omitempty" jsonschema:"minimum=0,maximum=15"`
}

type constrainedSpan struct {
	Text  string            `json:"text" jsonschema:"minLength=1"`
	Style *constrainedStyle `json:"style,omitempty"`
}

type constrainedSpans []constrainedSpan

func TestStructSchemaViaReflector_preserves_type_constraints(t *testing.T) {
	schema, err := schemaViaReflector(reflect.TypeFor[constrainedSpan]())
	require.NoError(t, err)
	properties := schema["properties"].(map[string]any)
	require.Equal(t, float64(1), properties["text"].(map[string]any)["minLength"])

	style := properties["style"].(map[string]any)
	styleProperties := style["properties"].(map[string]any)
	for _, colour := range []string{"fg", "bg"} {
		colourSchema := styleProperties[colour].(map[string]any)
		require.Equal(t, float64(0), colourSchema["minimum"])
		require.Equal(t, float64(15), colourSchema["maximum"])
	}
}

type xorSchemaCommand struct {
	Body    []string         `arg:"" passthrough:"all" xor:"content" max:"2" tool:"Plain bodies"`
	Spans   constrainedSpans `cli:"-" xor:"content" max:"3" tool:"Styled spans"`
	CLIOnly string           `optional:"" tool:"-"`
}

type xorSchemaGrammar struct {
	Msg xorSchemaCommand `cmd:"" tool:""`
}

type toolOnlyPayload struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

type toolOnlyStructCommand struct {
	Data toolOnlyPayload `cli:"-" tool:"Structured data"`
}

type toolOnlyStructGrammar struct {
	Inspect toolOnlyStructCommand `cmd:"" tool:""`
}

type optionalUnionValue struct{}

func (optionalUnionValue) JSONSchema() *invjsonschema.Schema {
	return &invjsonschema.Schema{
		AnyOf: []*invjsonschema.Schema{
			{Type: "string"},
			{Type: "integer"},
		},
	}
}

type pointerOnlyUnionValue struct{}

func (*pointerOnlyUnionValue) JSONSchema() *invjsonschema.Schema {
	return &invjsonschema.Schema{
		AnyOf: []*invjsonschema.Schema{
			{Type: "boolean"},
			{Type: "integer"},
		},
	}
}

type nestedPointerOnlyUnionValue struct {
	Value pointerOnlyUnionValue `json:"value"`
}

type enumOnlyValue string

func (enumOnlyValue) JSONSchema() *invjsonschema.Schema {
	return &invjsonschema.Schema{Type: "string", Enum: []any{"alpha", "beta"}}
}

type constOnlyValue string

func (constOnlyValue) JSONSchema() *invjsonschema.Schema {
	return &invjsonschema.Schema{Type: "string", Const: "fixed"}
}

type optionalConstraintCommand struct {
	Enum  enumOnlyValue  `cli:"-" optional:"" tool:"Enum value"`
	Const constOnlyValue `cli:"-" optional:"" tool:"Const value"`
}

type optionalConstraintGrammar struct {
	Inspect optionalConstraintCommand `cmd:"" tool:""`
}

type definitionPayload struct {
	Required string `json:"required"`
	Optional string `json:"optional,omitempty"`
}

type emptyObjectValue struct{}

func (emptyObjectValue) JSONSchema() *invjsonschema.Schema {
	return &invjsonschema.Schema{Type: "object"}
}

type emptyObjectCommand struct {
	Value emptyObjectValue `cli:"-" tool:"Empty object"`
}

type emptyObjectGrammar struct {
	Inspect emptyObjectCommand `cmd:"" tool:""`
}

type trueSchemaValue string

func (trueSchemaValue) JSONSchema() *invjsonschema.Schema {
	return invjsonschema.TrueSchema
}

type falseSchemaValue string

func (falseSchemaValue) JSONSchema() *invjsonschema.Schema {
	return invjsonschema.FalseSchema
}

type implicitTrueSchemaValue string

func (implicitTrueSchemaValue) JSONSchema() *invjsonschema.Schema {
	return &invjsonschema.Schema{}
}

type nestedTrueSchemaValue struct {
	Value trueSchemaValue `json:"value"`
}

type providerKeywordProperties struct {
	If   string `json:"if"`
	Not  string `json:"not"`
	Then string `json:"then"`
}

type providerKeywordPropertiesCommand struct {
	Value providerKeywordProperties `cli:"-" tool:"Keyword-named properties"`
}

type providerKeywordPropertiesGrammar struct {
	Inspect providerKeywordPropertiesCommand `cmd:"" tool:""`
}

type unsupportedMapCommand struct {
	Value map[string]string `cli:"-" tool:"Map value"`
}

type unsupportedMapGrammar struct {
	Inspect unsupportedMapCommand `cmd:"" tool:""`
}

type nestedMapValue struct {
	Labels map[string]string `json:"labels"`
}

type nestedMapCommand struct {
	Value nestedMapValue `cli:"-" tool:"Nested map value"`
}

type nestedMapGrammar struct {
	Inspect nestedMapCommand `cmd:"" tool:""`
}

type customMapValue struct{}

func (customMapValue) JSONSchema() *invjsonschema.Schema {
	return &invjsonschema.Schema{
		Type:                 "object",
		AdditionalProperties: &invjsonschema.Schema{Type: "string"},
	}
}

type customMapCommand struct {
	Value customMapValue `cli:"-" tool:"Custom map value"`
}

type customMapGrammar struct {
	Inspect customMapCommand `cmd:"" tool:""`
}

type optionalUnionCommand struct {
	Value optionalUnionValue `cli:"-" optional:"" tool:"Optional union"`
}

type optionalUnionGrammar struct {
	Inspect optionalUnionCommand `cmd:"" tool:""`
}

type unsupportedProviderValue struct{}

func (unsupportedProviderValue) JSONSchema() *invjsonschema.Schema {
	return &invjsonschema.Schema{
		AllOf: []*invjsonschema.Schema{{Type: "string"}},
	}
}

type unsupportedProviderCommand struct {
	Value unsupportedProviderValue `cli:"-" tool:"Unsupported value"`
}

type unsupportedProviderGrammar struct {
	Inspect unsupportedProviderCommand `cmd:"" tool:""`
}

func xorSchemaSet(t *testing.T) Set[testCtx] {
	t.Helper()

	set, err := Build[testCtx](&xorSchemaGrammar{})
	require.NoError(t, err)

	return set
}

func TestToolParameters_transforms_flat_xor_fields(t *testing.T) {
	node := xorSchemaSet(t).Find("msg")
	require.Equal(t, expectedXORProviderSchema(), node.ToolParameters())
}

func expectedXORProviderSchema() map[string]any {
	bodyBranch := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"body": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Plain bodies",
			},
		},
		"required":             []string{"body"},
		"additionalProperties": false,
	}
	spansBranch := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"spans": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"text", "style"},
					"properties": map[string]any{
						"text": map[string]any{"type": "string"},
						"style": map[string]any{
							"type":                 []any{"object", "null"},
							"additionalProperties": false,
							"required":             []string{"bg", "fg"},
							"properties": map[string]any{
								"fg": map[string]any{"type": []any{"integer", "null"}},
								"bg": map[string]any{"type": []any{"integer", "null"}},
							},
						},
					},
				},
				"description": "Styled spans",
			},
		},
		"required":             []string{"spans"},
		"additionalProperties": false,
	}

	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"content": map[string]any{
				"anyOf": []any{bodyBranch, spansBranch},
			},
		},
		"required":             []string{"content"},
		"additionalProperties": false,
	}
}

func TestToolParameters_returns_a_defensive_copy(t *testing.T) {
	node := toolSet(t).Find("join")
	first := node.ToolParameters()
	properties := first["properties"].(map[string]any)
	delete(properties, "channel")
	first["required"].([]string)[0] = "changed"

	require.Equal(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"channel": map[string]any{"type": "string", "description": "Channel to join"},
		},
		"required":             []string{"channel"},
		"additionalProperties": false,
	}, node.ToolParameters())
}

func TestProviderToolSchema_removes_unsupported_keywords_without_mutating_runtime_schema(t *testing.T) {
	runtime := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"choice": map[string]any{
				"oneOf": []any{
					map[string]any{"type": "string"},
					map[string]any{"type": "null"},
				},
			},
			"maximum": map[string]any{
				"type":    "number",
				"minimum": float64(0),
				"maximum": float64(15),
			},
			"values": map[string]any{
				"type":     "array",
				"minItems": 1,
				"maxItems": 3,
				"items": map[string]any{
					"type":      "string",
					"minLength": float64(1),
				},
			},
		},
		"required":             []string{"choice", "maximum", "values"},
		"additionalProperties": false,
	}
	runtimeBefore := cloneToolSchema(runtime)

	provider, err := providerToolSchema("test", runtime)
	require.NoError(t, err)

	require.Equal(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"choice": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string"},
					map[string]any{"type": "null"},
				},
			},
			"maximum": map[string]any{"type": "number"},
			"values": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
		},
		"required":             []string{"choice", "maximum", "values"},
		"additionalProperties": false,
	}, provider)
	require.Equal(t, runtimeBefore, runtime)
}

func TestToolValue_enforces_constraints_omitted_from_the_provider_schema(t *testing.T) {
	node := xorSchemaSet(t).Find("msg")

	tests := []struct {
		name string
		raw  string
		want []ToolArgumentViolation
	}{
		{
			name: "maximum array length",
			raw:  `{"content":{"body":["one","two","three"]}}`,
			want: []ToolArgumentViolation{
				{},
				{InstanceLocation: []string{"content"}, KeywordLocation: []string{"anyOf"}},
				{InstanceLocation: []string{"content", "body"}, KeywordLocation: []string{"maxItems"}},
				{InstanceLocation: []string{"content"}},
				{InstanceLocation: []string{"content"}, KeywordLocation: []string{"required"}},
				{InstanceLocation: []string{"content"}, KeywordLocation: []string{"additionalProperties"}},
			},
		},
		{
			name: "minimum span text length",
			raw:  `{"content":{"spans":[{"text":"","style":null}]}}`,
			want: []ToolArgumentViolation{
				{},
				{InstanceLocation: []string{"content"}, KeywordLocation: []string{"anyOf"}},
				{InstanceLocation: []string{"content"}},
				{InstanceLocation: []string{"content"}, KeywordLocation: []string{"required"}},
				{InstanceLocation: []string{"content"}, KeywordLocation: []string{"additionalProperties"}},
				{InstanceLocation: []string{"content", "spans", "0", "text"}, KeywordLocation: []string{"minLength"}},
			},
		},
		{
			name: "maximum colour value",
			raw:  `{"content":{"spans":[{"text":"hello","style":{"fg":16,"bg":null}}]}}`,
			want: []ToolArgumentViolation{
				{},
				{InstanceLocation: []string{"content"}, KeywordLocation: []string{"anyOf"}},
				{InstanceLocation: []string{"content"}},
				{InstanceLocation: []string{"content"}, KeywordLocation: []string{"required"}},
				{InstanceLocation: []string{"content"}, KeywordLocation: []string{"additionalProperties"}},
				{InstanceLocation: []string{"content", "spans", "0", "style", "fg"}, KeywordLocation: []string{"maximum"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := node.ToolValue(json.RawMessage(tt.raw))

			var validation *ToolArgumentsValidationError
			require.ErrorAs(t, err, &validation)
			require.Equal(t, struct {
				Tool       string
				Violations []ToolArgumentViolation
			}{Tool: "msg", Violations: tt.want}, struct {
				Tool       string
				Violations []ToolArgumentViolation
			}{Tool: validation.Tool, Violations: validation.Violations})
		})
	}
}

func TestToolValue_validates_the_provider_schema_before_flattening(t *testing.T) {
	node := xorSchemaSet(t).Find("msg")

	value, err := node.ToolValue(json.RawMessage(`{"content":{"body":["one","two"]}}`))
	require.NoError(t, err)
	require.Equal(t, xorSchemaCommand{Body: []string{"one", "two"}}, value)

	tests := []struct {
		name string
		raw  string
		want []ToolArgumentViolation
	}{
		{
			name: "missing group",
			raw:  `{}`,
			want: []ToolArgumentViolation{
				{},
				{KeywordLocation: []string{"required"}},
			},
		},
		{
			name: "both branches",
			raw:  `{"content":{"body":["one"],"spans":[]}}`,
			want: []ToolArgumentViolation{
				{},
				{InstanceLocation: []string{"content"}, KeywordLocation: []string{"anyOf"}},
				{InstanceLocation: []string{"content"}, KeywordLocation: []string{"additionalProperties"}},
				{InstanceLocation: []string{"content"}, KeywordLocation: []string{"additionalProperties"}},
			},
		},
		{
			name: "unknown property",
			raw:  `{"content":{"body":["one"]},"extra":true}`,
			want: []ToolArgumentViolation{
				{},
				{KeywordLocation: []string{"additionalProperties"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := node.ToolValue(json.RawMessage(tt.raw))

			var validation *ToolArgumentsValidationError
			require.ErrorAs(t, err, &validation)
			require.Equal(t, struct {
				Tool       string
				Violations []ToolArgumentViolation
			}{
				Tool:       "msg",
				Violations: tt.want,
			}, struct {
				Tool       string
				Violations []ToolArgumentViolation
			}{
				Tool:       validation.Tool,
				Violations: validation.Violations,
			})
		})
	}
}

func TestFieldScopes_separate_CLI_and_tool_parameters(t *testing.T) {
	set := xorSchemaSet(t)

	value, err := set.ParseValue("/msg --cli-only local hello there")
	require.NoError(t, err)
	require.Equal(t, xorSchemaCommand{Body: []string{"hello there"}, CLIOnly: "local"}, value)

	value, err = set.ParseValue("/msg --spans literal")
	require.NoError(t, err)
	require.Equal(t, xorSchemaCommand{Body: []string{"--spans literal"}}, value)

	require.Equal(t, expectedXORProviderSchema(), set.Find("msg").ToolParameters())
}

func TestFieldScopes_tool_only_struct_needs_no_CLI_decoder(t *testing.T) {
	set, err := Build[testCtx](&toolOnlyStructGrammar{})
	require.NoError(t, err)

	require.Equal(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"data": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"value", "count"},
				"properties": map[string]any{
					"value": map[string]any{"type": "string"},
					"count": map[string]any{"type": "integer"},
				},
				"description": "Structured data",
			},
		},
		"required":             []string{"data"},
		"additionalProperties": false,
	}, set.Find("inspect").ToolParameters())

	value, err := set.Find("inspect").ToolValue(json.RawMessage(`{"data":{"value":"present","count":4.0}}`))
	require.NoError(t, err)
	require.Equal(t, toolOnlyStructCommand{Data: toolOnlyPayload{Value: "present", Count: 4}}, value)
}

func TestToolParameters_optional_custom_union_adds_a_null_branch(t *testing.T) {
	set, err := Build[testCtx](&optionalUnionGrammar{})
	require.NoError(t, err)

	require.Equal(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"value": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string"},
					map[string]any{"type": "integer"},
					map[string]any{"type": "null"},
				},
				"description": "Optional union",
			},
		},
		"required":             []string{"value"},
		"additionalProperties": false,
	}, set.Find("inspect").ToolParameters())
}

func TestToolParameters_optional_constraints_preserve_non_null_values(t *testing.T) {
	set, err := Build[testCtx](&optionalConstraintGrammar{})
	require.NoError(t, err)
	node := set.Find("inspect")

	runtimeSchema, _, err := buildToolParameters(node.toolFields)
	require.NoError(t, err)
	require.Equal(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"enum": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string", "enum": []any{"alpha", "beta"}},
					map[string]any{"type": "null"},
				},
				"description": "Enum value",
			},
			"const": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string", "const": "fixed"},
					map[string]any{"type": "null"},
				},
				"description": "Const value",
			},
		},
		"required":             []string{"enum", "const"},
		"additionalProperties": false,
	}, runtimeSchema)

	require.Equal(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"enum": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string", "enum": []any{"alpha", "beta"}},
					map[string]any{"type": "null"},
				},
				"description": "Enum value",
			},
			"const": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string", "enum": []any{"fixed"}},
					map[string]any{"type": "null"},
				},
				"description": "Const value",
			},
		},
		"required":             []string{"enum", "const"},
		"additionalProperties": false,
	}, node.ToolParameters())

	value, err := node.ToolValue(json.RawMessage(`{"enum":"alpha","const":"fixed"}`))
	require.NoError(t, err)
	require.Equal(t, optionalConstraintCommand{
		Enum:  "alpha",
		Const: "fixed",
	}, value)

	value, err = node.ToolValue(json.RawMessage(`{"enum":null,"const":null}`))
	require.NoError(t, err)
	require.Equal(t, optionalConstraintCommand{}, value)

	_, err = node.ToolValue(json.RawMessage(`{"enum":"alpha","const":"other"}`))
	var validation *ToolArgumentsValidationError
	require.ErrorAs(t, err, &validation)
	require.Equal(t, struct {
		Tool       string
		Violations []ToolArgumentViolation
	}{
		Tool: "inspect",
		Violations: []ToolArgumentViolation{
			{},
			{InstanceLocation: []string{"const"}, KeywordLocation: []string{"anyOf"}},
			{InstanceLocation: []string{"const"}, KeywordLocation: []string{"enum"}},
			{InstanceLocation: []string{"const"}, KeywordLocation: []string{"type"}},
		},
	}, struct {
		Tool       string
		Violations []ToolArgumentViolation
	}{
		Tool:       validation.Tool,
		Violations: validation.Violations,
	})
}

func TestToolParameters_strictifies_object_definitions(t *testing.T) {
	schema, err := schemaViaCustomType(invjsonschema.Reflect(definitionPayload{}))
	require.NoError(t, err)

	require.Equal(t, map[string]any{
		"$ref": "#/$defs/definitionPayload",
		"$defs": map[string]any{
			"definitionPayload": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"required": map[string]any{"type": "string"},
					"optional": map[string]any{"type": []any{"string", "null"}},
				},
				"required":             []string{"required", "optional"},
				"additionalProperties": false,
			},
		},
	}, schema)

	compiled, err := compileToolSchema("inspect", schema)
	require.NoError(t, err)
	require.NoError(t, compiled.Validate(map[string]any{"required": "yes", "optional": nil}))
}

func TestMakeStrictObjectSchema_preserves_optional_references(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"value": map[string]any{"$ref": "#/$defs/value"},
		},
		"$defs": map[string]any{
			"value": map[string]any{"type": "string", "enum": []any{"referenced"}},
		},
	}

	makeStrictObjectSchema(schema)
	provider, err := providerToolSchema("inspect", schema)
	require.NoError(t, err)
	require.Equal(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"value": map[string]any{
				"anyOf": []any{
					map[string]any{"$ref": "#/$defs/value"},
					map[string]any{"type": "null"},
				},
			},
		},
		"required":             []string{"value"},
		"additionalProperties": false,
		"$defs": map[string]any{
			"value": map[string]any{"type": "string", "enum": []any{"referenced"}},
		},
	}, provider)

	compiled, err := compileToolSchema("inspect", schema)
	require.NoError(t, err)
	require.NoError(t, compiled.Validate(map[string]any{"value": "referenced"}))
	require.NoError(t, compiled.Validate(map[string]any{"value": nil}))
}

func TestToolParameters_strictifies_empty_objects(t *testing.T) {
	set, err := Build[testCtx](&emptyObjectGrammar{})
	require.NoError(t, err)

	require.Equal(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"value": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"description":          "Empty object",
			},
		},
		"required":             []string{"value"},
		"additionalProperties": false,
	}, set.Find("inspect").ToolParameters())
}

func TestToolSchemaForType_rejects_boolean_schemas(t *testing.T) {
	tests := []struct {
		name     string
		typ      reflect.Type
		value    bool
		location []string
	}{
		{name: "true", typ: reflect.TypeFor[trueSchemaValue](), value: true},
		{name: "false", typ: reflect.TypeFor[falseSchemaValue](), value: false},
		{name: "implicit true", typ: reflect.TypeFor[implicitTrueSchemaValue](), value: true},
		{
			name:     "nested true",
			typ:      reflect.TypeFor[nestedTrueSchemaValue](),
			value:    true,
			location: []string{"properties", "value"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := toolSchemaForType(tt.typ)

			var schemaErr *UnsupportedBooleanToolSchemaError
			require.ErrorAs(t, err, &schemaErr)
			require.Equal(t, &UnsupportedBooleanToolSchemaError{
				Value:    tt.value,
				Location: tt.location,
			}, schemaErr)
		})
	}
}

func TestBuild_rejects_structural_keywords_the_provider_cannot_represent(t *testing.T) {
	_, err := Build[testCtx](&unsupportedProviderGrammar{})

	var keywordErr *UnsupportedProviderSchemaKeywordError
	require.ErrorAs(t, err, &keywordErr)
	require.Equal(t, &UnsupportedProviderSchemaKeywordError{
		Tool:     "inspect",
		Keyword:  "allOf",
		Location: []string{"properties", "value", "allOf"},
	}, keywordErr)
}

func TestBuild_treats_property_names_as_opaque_schema_data(t *testing.T) {
	set, err := Build[testCtx](&providerKeywordPropertiesGrammar{})
	require.NoError(t, err)

	require.Equal(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"value": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"if":   map[string]any{"type": "string"},
					"not":  map[string]any{"type": "string"},
					"then": map[string]any{"type": "string"},
				},
				"required":             []string{"if", "not", "then"},
				"additionalProperties": false,
				"description":          "Keyword-named properties",
			},
		},
		"required":             []string{"value"},
		"additionalProperties": false,
	}, set.Find("inspect").ToolParameters())
}

func TestBuild_rejects_tool_maps_without_a_fixed_property_schema(t *testing.T) {
	_, err := Build[testCtx](&unsupportedMapGrammar{})

	var typeErr *UnsupportedToolSchemaTypeError
	require.ErrorAs(t, err, &typeErr)
	require.Equal(t, &UnsupportedToolSchemaTypeError{Type: reflect.TypeFor[map[string]string]()}, typeErr)
}

func TestBuild_rejects_nested_tool_maps_without_a_fixed_property_schema(t *testing.T) {
	_, err := Build[testCtx](&nestedMapGrammar{})

	var keywordErr *UnsupportedProviderSchemaKeywordError
	require.ErrorAs(t, err, &keywordErr)
	require.Equal(t, &UnsupportedProviderSchemaKeywordError{
		Tool:    "inspect",
		Keyword: "additionalProperties",
		Location: []string{
			"properties", "value", "properties", "labels", "additionalProperties",
		},
	}, keywordErr)
}

func TestBuild_rejects_custom_dictionary_schemas(t *testing.T) {
	_, err := Build[testCtx](&customMapGrammar{})

	var keywordErr *UnsupportedProviderSchemaKeywordError
	require.ErrorAs(t, err, &keywordErr)
	require.Equal(t, &UnsupportedProviderSchemaKeywordError{
		Tool:     "inspect",
		Keyword:  "additionalProperties",
		Location: []string{"properties", "value", "additionalProperties"},
	}, keywordErr)
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

	t.Run("supplied zero values remain present", func(t *testing.T) {
		node := s.Find("count")

		val, err := node.ToolValue(json.RawMessage(`{"count":0,"force":false}`))
		require.NoError(t, err)
		require.Equal(t, toolCountCmd{Count: 0, Force: false}, val)
	})

	t.Run("mathematical integers use the Go integer representation", func(t *testing.T) {
		node := s.Find("count")

		for _, raw := range []string{
			`{"count":4,"force":false}`,
			`{"count":4.0,"force":false}`,
			`{"count":4e0,"force":false}`,
		} {
			t.Run(raw, func(t *testing.T) {
				value, err := node.ToolValue(json.RawMessage(raw))
				require.NoError(t, err)
				require.Equal(t, toolCountCmd{Count: 4, Force: false}, value)
			})
		}
	})

	t.Run("empty args uses defaults", func(t *testing.T) {
		node := s.Find("topic")

		val, err := node.ToolValue(json.RawMessage(`{"topic": null}`))
		require.NoError(t, err)
		require.Equal(t, toolTopicCmd{}, val)
	})

	t.Run("missing required arg", func(t *testing.T) {
		node := s.Find("join")
		raw := json.RawMessage(`{}`)

		_, err := node.ToolValue(raw)

		var validation *ToolArgumentsValidationError
		require.ErrorAs(t, err, &validation)
		require.Equal(t, "join", validation.Tool)
	})

	t.Run("malformed JSON", func(t *testing.T) {
		node := s.Find("join")

		_, err := node.ToolValue(json.RawMessage(`{not json}`))
		require.Error(t, err)
	})

	t.Run("no factory", func(t *testing.T) {
		node := &Node[testCtx]{Name: "broken"}

		_, err := node.ToolValue(json.RawMessage(`{}`))

		var noFactory *NoFactoryError
		require.ErrorAs(t, err, &noFactory)
		require.Equal(t, &NoFactoryError{Path: "broken"}, noFactory)
	})

	t.Run("non-leaf", func(t *testing.T) {
		node := &Node[testCtx]{Name: "parent", Children: []*Node[testCtx]{{Name: "child"}}}

		_, err := node.ToolValue(json.RawMessage(`{}`))

		var notLeaf *NotToolLeafError
		require.ErrorAs(t, err, &notLeaf)
		require.Equal(t, &NotToolLeafError{Path: "parent"}, notLeaf)
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
		{
			name: "fixed string array",
			typ:  reflect.TypeFor[[2]string](),
			want: map[string]any{
				"type":     "array",
				"items":    map[string]any{"type": "string"},
				"minItems": 2,
				"maxItems": 2,
			},
		},
		{
			name: "pointer-only custom schema",
			typ:  reflect.TypeFor[pointerOnlyUnionValue](),
			want: map[string]any{
				"anyOf": []any{
					map[string]any{"type": "boolean"},
					map[string]any{"type": "integer"},
				},
			},
		},
		{
			name: "nested pointer-only custom schema",
			typ:  reflect.TypeFor[nestedPointerOnlyUnionValue](),
			want: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"value": map[string]any{
						"anyOf": []any{
							map[string]any{"type": "boolean"},
							map[string]any{"type": "integer"},
						},
					},
				},
				"required":             []string{"value"},
				"additionalProperties": false,
			},
		},
		{name: "pointer to string", typ: reflect.TypeFor[*string](), want: map[string]any{"type": "string"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := toolSchemaForType(tt.typ)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestToolDescription_three_tiers(t *testing.T) {
	t.Run("tier 3: falls back to help", func(t *testing.T) {
		node := &Node[testCtx]{Help: "Join a channel.", ToolDesc: ""}

		require.Equal(t, "Join a channel.", node.ToolDescription(struct{}{}))
	})

	t.Run("tier 2: non-empty tool tag", func(t *testing.T) {
		node := &Node[testCtx]{Help: "Exit modeloff.", ToolDesc: "Shut down your instance."}

		require.Equal(t, "Shut down your instance.", node.ToolDescription(struct{}{}))
	})

	t.Run("tier 1: ToolDescriber interface", func(t *testing.T) {
		node := &Node[testCtx]{Help: "Quit.", ToolDesc: "Should be overridden."}

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
		node := &Node[testCtx]{Name: "broken"}
		require.Nil(t, node.NewZero())
	})
}
