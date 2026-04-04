package command

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/set"
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

		require.Equal(t, "child", merged.Commands[0].Help)
	})
}

func TestExecute_uses_handler(t *testing.T) {
	type execGrammar struct {
		Join JoinCommand `cmd:"" help:"Join."`
	}

	cmds := Build(&execGrammar{})
	called := ""

	Bind(cmds, "join", func(_ JoinCommand) tea.Cmd {
		return func() tea.Msg {
			called = "join called"
			return nil
		}
	})

	cmd, err := Execute(cmds, "/join #general")

	require.NoError(t, err)
	require.NotNil(t, cmd)
	cmd()
	require.Equal(t, "join called", called)
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
			completion := Complete(cmds, tt.raw, len([]rune(tt.raw)), CompletionContext{})

			require.True(t, completion.Visible)
			require.Equal(t, tt.suggestions, completion.Suggestions)
		})
	}
}

func TestComplete_argument_sources_are_contextual(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{
				Name: "kick",
				Help: "Kick a nick",
				Positionals: []Positional{
					{Name: "nick", Source: ActiveMembersSource()},
				},
			},
		},
	}

	ctx := CompletionContext{
		UserNick:      "testuser",
		ActiveMembers: []domain.Nick{"testuser", "botty", "helper"},
	}

	completion := Complete(cmds, "/kick h", 7, ctx)

	require.Equal(t, []Suggestion{{Value: "helper", Label: "helper", Detail: ""}}, completion.Suggestions)
	require.False(t, completion.AppendSpace)
}

func TestComplete_free_form_arguments_have_no_suggestions(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{
				Name: "msg",
				Help: "Direct message",
				Positionals: []Positional{
					{Name: "nick", Source: InstancesSource()},
					{Name: "message", Variadic: true, Optional: true, Help: "Message body"},
				},
			},
		},
	}

	ctx := CompletionContext{
		Instances: []domain.ModelInstance{{Nick: "botty", ModelID: "test/model"}},
	}

	completion := Complete(cmds, "/msg botty hello", len([]rune("/msg botty hello")), ctx)

	require.True(t, completion.Visible)
	require.Empty(t, completion.Suggestions)
}

func TestComplete_composes_local_and_live_model_suggestions(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{
				Name: "invite",
				Help: "Invite a model",
				Positionals: []Positional{
					{
						Name: "model",
						Source: ComposeSources(
							ReusableInstancesSource(),
							LiveModelsSource(),
						),
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

	ctx := CompletionContext{
		ActiveChannel: "#general",
		Instances: []domain.ModelInstance{
			{
				Nick:     "botty",
				ModelID:  "test/model-a",
				Channels: set.NewOrdered[domain.ChannelName]("#random"),
			},
			{
				Nick:     "busybot",
				ModelID:  "test/model-b",
				Channels: set.NewOrdered[domain.ChannelName]("#general"),
			},
		},
		LiveModels: []ModelOption{
			{ID: "anthropic/claude-3-haiku", Name: "Claude Haiku"},
		},
	}

	completion := Complete(cmds, "/invite ", len([]rune("/invite ")), ctx)

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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.node.Usage())
		})
	}
}

func TestInvocation_Run_nil_handler(t *testing.T) {
	inv := &Invocation{
		node: &Node{Name: "quit"},
	}

	cmd := inv.Run()

	require.Nil(t, cmd)
}

func TestExecute_no_handler(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{
				Name: "quit",
				factory: func() any {
					return &QuitCommand{}
				},
			},
		},
	}

	_, err := Execute(cmds, "/quit")

	require.ErrorContains(t, err, "no handler")
}

func TestBind_overwrites_existing_handler(t *testing.T) {
	type grammar struct {
		Join JoinCommand `cmd:"" help:"Join."`
	}

	cmds := Build(&grammar{})

	Bind(cmds, "join", func(_ JoinCommand) tea.Cmd {
		return func() tea.Msg { return "first" }
	})

	Bind(cmds, "join", func(_ JoinCommand) tea.Cmd {
		return func() tea.Msg { return "second" }
	})

	inv, err := cmds.Parse("/join #test")
	require.NoError(t, err)

	msg := inv.Run()()
	require.Equal(t, "second", msg)
}

func TestSetSource_nonexistent_positional(t *testing.T) {
	node := &Node{
		Name:        "join",
		Positionals: []Positional{{Name: "channel"}},
	}

	node.SetSource("nonexistent", ChannelsSource())

	require.Nil(t, node.Positionals[0].Source)
}

func TestSetSource_attaches_source(t *testing.T) {
	node := &Node{
		Name:        "join",
		Positionals: []Positional{{Name: "channel"}},
	}

	node.SetSource("channel", ChannelsSource())

	require.NotNil(t, node.Positionals[0].Source)
}

func TestComplete_token_boundaries(t *testing.T) {
	cmds := Set{
		Commands: []*Node{
			{
				Name: "kick",
				Help: "Kick a nick",
				Positionals: []Positional{
					{Name: "nick", Source: ActiveMembersSource()},
				},
			},
			{Name: "quit", Help: "Exit."},
		},
	}

	ctx := CompletionContext{
		ActiveMembers: []domain.Nick{"alice", "bob"},
	}

	tests := []struct {
		name       string
		raw        string
		cursor     int
		wantCmd    bool
		wantArgLen int
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
			name:       "cursor after space shows argument suggestions",
			raw:        "/kick ",
			cursor:     6,
			wantArgLen: 2,
		},
		{
			name:       "cursor mid-argument filters",
			raw:        "/kick al",
			cursor:     8,
			wantArgLen: 1,
		},
		{
			name:       "cursor beyond length is clamped",
			raw:        "/kick ",
			cursor:     100,
			wantArgLen: 2,
		},
		{
			name:    "not a command",
			raw:     "hello",
			cursor:  5,
			wantCmd: false,
		},
		{
			name:       "multiple spaces between tokens",
			raw:        "/kick   a",
			cursor:     9,
			wantArgLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completion := Complete(cmds, tt.raw, tt.cursor, ctx)

			if !tt.wantCmd && tt.wantArgLen == 0 {
				require.False(t, completion.Visible)
				return
			}

			require.True(t, completion.Visible)

			if tt.wantArgLen > 0 {
				require.Len(t, completion.Suggestions, tt.wantArgLen)
			}
		})
	}
}

func TestComplete_unknown_command_has_no_suggestions(t *testing.T) {
	cmds := Set{
		Commands: []*Node{{Name: "quit", Help: "Exit."}},
	}

	completion := Complete(cmds, "/unknown arg", len([]rune("/unknown arg")), CompletionContext{})

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
	completion := Complete(cmds, raw, len([]rune(raw)), CompletionContext{})

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

func TestParse_no_factory(t *testing.T) {
	cmds := Set{
		Commands: []*Node{{Name: "broken"}},
	}

	_, err := cmds.Parse("/broken")

	require.ErrorContains(t, err, "no factory")
}

func TestParse_after_merge(t *testing.T) {
	type childGrammar struct {
		Join JoinCommand `cmd:"" help:"Child join."`
	}

	type parentGrammar struct {
		Join JoinCommand `cmd:"" help:"Parent join."`
		Quit QuitCommand `cmd:"" help:"Quit."`
	}

	child := Build(&childGrammar{})
	parent := Build(&parentGrammar{})

	Bind(child, "join", func(_ JoinCommand) tea.Cmd {
		return func() tea.Msg { return "child-join" }
	})

	Bind(parent, "quit", func(_ QuitCommand) tea.Cmd {
		return func() tea.Msg { return "quit" }
	})

	merged := Merge(child, parent)

	t.Run("child command wins", func(t *testing.T) {
		inv, err := merged.Parse("/join #test")
		require.NoError(t, err)

		msg := inv.Run()()
		require.Equal(t, "child-join", msg)
	})

	t.Run("parent command accessible", func(t *testing.T) {
		inv, err := merged.Parse("/quit")
		require.NoError(t, err)

		msg := inv.Run()()
		require.Equal(t, "quit", msg)
	})

	t.Run("unknown command errors", func(t *testing.T) {
		_, err := merged.Parse("/unknown")
		require.Error(t, err)
	})
}
