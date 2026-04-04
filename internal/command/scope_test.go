package command

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/set"
)

func TestMerge_prefers_nearest_scope(t *testing.T) {
	child := Scope{Commands: []Spec{{Name: "join", Help: "child"}}}
	parent := Scope{Commands: []Spec{{Name: "join", Help: "parent"}, {Name: "list", Help: "list"}}}

	merged := Merge(child, parent)

	require.Len(t, merged.Commands, 2)
	require.Equal(t, "child", merged.Commands[0].Help)
	require.Equal(t, "list", merged.Commands[1].Name)
}

func TestExecute_uses_scoped_handler(t *testing.T) {
	called := ""
	scope := Scope{
		Commands: []Spec{
			{
				Name: "join",
				Handler: func(inv Invocation) tea.Cmd {
					called = inv.Raw
					return nil
				},
			},
		},
	}

	_, err := Execute(scope, "/join #general")

	require.NoError(t, err)
	require.Equal(t, "/join #general", called)
}

func TestComplete_command_suggestions_carry_usage(t *testing.T) {
	scope := Scope{
		Commands: []Spec{
			{Name: "join", Help: "Join channels", Usage: "/join <channel>"},
			{Name: "list", Help: "List channels", Usage: "/list"},
			{Name: "quit", Help: "Exit.", Usage: "/quit"},
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
			completion := Complete(scope, tt.raw, len([]rune(tt.raw)), CompletionContext{})

			require.True(t, completion.Visible)
			require.Equal(t, tt.suggestions, completion.Suggestions)
		})
	}
}

func TestComplete_argument_sources_are_contextual(t *testing.T) {
	scope := Scope{
		Commands: []Spec{
			{
				Name:  "kick",
				Help:  "Kick a nick",
				Usage: "/kick <nick>",
				Args: []ArgSpec{
					{Name: "nick", Source: ActiveMembersSource()},
				},
			},
		},
	}

	ctx := CompletionContext{
		UserNick:      "testuser",
		ActiveMembers: []domain.Nick{"testuser", "botty", "helper"},
	}

	completion := Complete(scope, "/kick h", 7, ctx)

	require.Equal(t, []Suggestion{{Value: "helper", Label: "helper", Detail: ""}}, completion.Suggestions)
	require.False(t, completion.AppendSpace)
}

func TestComplete_free_form_arguments_have_no_suggestions(t *testing.T) {
	scope := Scope{
		Commands: []Spec{
			{
				Name:  "msg",
				Help:  "Direct message",
				Usage: "/msg <nick> [message]",
				Args: []ArgSpec{
					{Name: "nick", Source: InstancesSource()},
					{Name: "message", FreeForm: true, Optional: true, Help: "Message body"},
				},
			},
		},
	}

	ctx := CompletionContext{
		Instances: []domain.ModelInstance{{Nick: "botty", ModelID: "test/model"}},
	}

	completion := Complete(scope, "/msg botty hello", len([]rune("/msg botty hello")), ctx)

	require.True(t, completion.Visible)
	require.Empty(t, completion.Suggestions)
}

func TestComplete_composes_local_and_live_model_suggestions(t *testing.T) {
	scope := Scope{
		Commands: []Spec{
			{
				Name:  "invite",
				Help:  "Invite a model",
				Usage: "/invite <model>",
				Args: []ArgSpec{
					{
						Name: "model",
						Source: ComposeSources(
							ReusableInstancesSource(),
							LiveModelsSource(),
						),
					},
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

	completion := Complete(scope, "/invite ", len([]rune("/invite ")), ctx)

	require.Equal(t, []Suggestion{
		{Value: "botty", Label: "botty", Detail: "test/model-a"},
		{Value: "anthropic/claude-3-haiku", Label: "anthropic/claude-3-haiku", Detail: "Claude Haiku"},
	}, completion.Suggestions)
	require.True(t, completion.AppendSpace)
}
