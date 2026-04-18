package chatcmd

import (
	"iter"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
)

func testContext(kind domain.ChannelKind) CompletionContext {
	channels := []domain.Channel{
		{Name: "#general", Topic: "Welcome"},
		{Name: "#random"},
	}
	haikuChannels := orderedmap.New[domain.ChannelName, time.Time]()
	haikuChannels.Set("#general", time.Time{})

	instances := []*domain.Instance{
		domain.NewModelInstance("inst-haiku", "haiku", "anthropic/haiku", "", haikuChannels),
		domain.NewModelInstance("inst-sonnet", "sonnet", "anthropic/sonnet", "", nil),
	}
	members := []domain.Nick{"testuser", "haiku"}
	models := []ModelOption{
		{ID: "anthropic/haiku", Name: "Haiku"},
		{ID: "anthropic/sonnet", Name: "Sonnet"},
	}
	personas := []domain.Persona{
		{ID: "bard", Description: "A travelling storyteller"},
		{ID: "sage", Description: "A wise advisor"},
	}

	return CompletionContext{
		Channels:      func() iter.Seq[domain.Channel] { return slices.Values(channels) },
		Instances:     func() iter.Seq[*domain.Instance] { return slices.Values(instances) },
		ActiveMembers: func() iter.Seq[domain.Nick] { return slices.Values(members) },
		ActiveChannel: func() domain.ChannelName { return "#general" },
		UserNick:      func() domain.Nick { return "testuser" },
		LiveModels:    func() iter.Seq[ModelOption] { return slices.Values(models) },
		Personas:      func() iter.Seq[domain.Persona] { return slices.Values(personas) },
		Kind:          func() domain.ChannelKind { return kind },
	}
}

var testParser = func() Parser {
	p, err := NewParser()
	if err != nil {
		panic(err)
	}

	return p
}()

func testSet(ctx CompletionContext) command.CompletionSet[CompletionContext] {
	return command.CompletionSet[CompletionContext]{
		Set: testParser.Set(),
		Ctx: ctx,
	}
}

func complete(t *testing.T, input string) command.Completion {
	t.Helper()

	return testSet(testContext(domain.KindChannel)).Complete(input, len(input))
}

func completeInKind(t *testing.T, input string, kind domain.ChannelKind) command.Completion {
	t.Helper()

	return testSet(testContext(kind)).Complete(input, len(input))
}

func suggestionValues(c command.Completion) []string {
	values := make([]string, len(c.Suggestions))
	for i, s := range c.Suggestions {
		values[i] = s.Value
	}

	return values
}

func TestComplete_dm_excludes_channel_only_commands(t *testing.T) {
	c := completeInKind(t, "/", domain.KindDM)

	require.Equal(t, []string{
		"join", "part", "list",
		"msg", "nick", "me", "whois", "config",
		"personas", "regenerate-personas",
		"help", "clear", "quit",
	}, suggestionValues(c))
}

func TestComplete_channel_includes_all_commands(t *testing.T) {
	c := completeInKind(t, "/", domain.KindChannel)

	require.Equal(t, []string{
		"join", "part", "list", "add-model", "invite", "kick",
		"msg", "nick", "topic", "me", "whois", "config",
		"personas", "regenerate-personas",
		"help", "clear", "quit",
	}, suggestionValues(c))
}

func TestNewParser_produces_all_commands(t *testing.T) {
	set := testParser.Set()

	names := make([]string, 0, len(set.Commands))
	for _, node := range set.Commands {
		names = append(names, node.Name)
	}

	require.Equal(t, []string{
		"join", "part", "list", "add-model", "invite", "kick",
		"msg", "nick", "topic", "me", "whois", "config",
		"personas", "regenerate-personas",
		"help", "clear", "quit",
	}, names)

	join := set.Find("join")
	require.Equal(t, []string{"j"}, join.Aliases)

	quit := set.Find("quit")
	require.Equal(t, []string{"q"}, quit.Aliases)
}

func TestNewParser_parse_returns_typed_command(t *testing.T) {
	cmd, err := testParser.Parse("/help")
	require.NoError(t, err)
	require.Equal(t, HelpCommand{}, cmd)
}

func TestQuitCommand_quitMessage_defaults_to_leaving(t *testing.T) {
	tests := []struct {
		name string
		cmd  QuitCommand
		want string
	}{
		{
			name: "nil message uses default",
			cmd:  QuitCommand{},
			want: "leaving",
		},
		{
			name: "empty message uses default",
			cmd:  QuitCommand{Message: []string{}},
			want: "leaving",
		},
		{
			name: "whitespace-only message uses default",
			cmd:  QuitCommand{Message: []string{"  "}},
			want: "leaving",
		},
		{
			name: "custom message is preserved",
			cmd:  QuitCommand{Message: []string{"see", "ya"}},
			want: "see ya",
		},
		{
			name: "single word message is preserved",
			cmd:  QuitCommand{Message: []string{"goodbye"}},
			want: "goodbye",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.cmd.quitMessage())
		})
	}
}

func TestComplete_join_suggests_channels(t *testing.T) {
	c := complete(t, "/join ")

	require.True(t, c.Visible)
	require.Equal(t, []string{"#general", "#random"}, suggestionValues(c))
}

func TestComplete_join_filters_by_prefix(t *testing.T) {
	c := complete(t, "/join #r")

	require.True(t, c.Visible)
	require.Equal(t, []string{"#random"}, suggestionValues(c))
}

func TestComplete_kick_suggests_active_members_excluding_self(t *testing.T) {
	c := complete(t, "/kick ")

	require.True(t, c.Visible)
	require.Equal(t, []string{"haiku"}, suggestionValues(c))
}

func TestComplete_add_model_suggests_reusable_then_live(t *testing.T) {
	c := complete(t, "/add-model ")

	require.True(t, c.Visible)

	values := suggestionValues(c)
	// sonnet is reusable (not in #general), haiku is excluded (already in #general)
	// then live models follow
	require.Equal(t, []string{"sonnet", "anthropic/haiku", "anthropic/sonnet"}, values)
}

func TestComplete_add_model_persona_suggests_personas(t *testing.T) {
	c := complete(t, "/add-model somemodel --persona ")

	require.True(t, c.Visible)
	require.Equal(t, []string{"bard", "sage"}, suggestionValues(c))
}

func TestComplete_invite_suggests_instance_nicks(t *testing.T) {
	c := complete(t, "/invite ")

	require.True(t, c.Visible)
	require.Equal(t, []string{"haiku", "sonnet"}, suggestionValues(c))
}

func TestComplete_msg_suggests_all_instances(t *testing.T) {
	c := complete(t, "/msg ")

	require.True(t, c.Visible)
	require.Equal(t, []string{"haiku", "sonnet"}, suggestionValues(c))
}

func TestComplete_whois_suggests_all_instances(t *testing.T) {
	c := complete(t, "/whois ")

	require.True(t, c.Visible)
	require.Equal(t, []string{"haiku", "sonnet"}, suggestionValues(c))
}

func TestComplete_config_suggests_subcommands(t *testing.T) {
	c := complete(t, "/config ")

	require.True(t, c.Visible)
	require.Equal(t, []string{
		"api-key", "base-url", "poke-interval",
		"small-model", "embedding-model", "highlight", "timestamp-format", "persona", "--reset",
	}, suggestionValues(c))
}

func TestComplete_config_poke_interval_suggests_durations(t *testing.T) {
	c := complete(t, "/config poke-interval ")

	require.True(t, c.Visible)
	require.Equal(t, []string{"5m", "10m", "30m", "1h"}, suggestionValues(c))
}

func TestComplete_config_api_key_no_value_suggestions(t *testing.T) {
	c := complete(t, "/config api-key ")

	require.True(t, c.Visible)
	require.Equal(t, []command.Suggestion(nil), c.Suggestions)
}

func TestComplete_config_reset_before_subcommand(t *testing.T) {
	c := complete(t, "/config --reset ")

	require.True(t, c.Visible)
	require.Equal(t, []string{
		"api-key", "base-url", "poke-interval",
		"small-model", "embedding-model", "highlight", "timestamp-format", "persona",
	}, suggestionValues(c))
}

func TestComplete_config_reset_after_subcommand_does_not_expect_value(t *testing.T) {
	c := complete(t, "/config api-key --reset ")

	require.True(t, c.Visible)
	require.Equal(t, []command.Suggestion(nil), c.Suggestions)
}

func TestParse_personas_command(t *testing.T) {
	cmd, err := testParser.Parse("/personas")
	require.NoError(t, err)
	require.IsType(t, PersonasCommand{}, cmd)
}

func TestParse_regenerate_personas_command(t *testing.T) {
	cmd, err := testParser.Parse("/regenerate-personas")
	require.NoError(t, err)
	require.IsType(t, RegeneratePersonasCommand{}, cmd)
}

func TestParse_clear_command(t *testing.T) {
	cmd, err := testParser.Parse("/clear")
	require.NoError(t, err)
	require.Equal(t, ClearCommand{}, cmd)
}

func TestClearCommand_Run_returns_ClearResult(t *testing.T) {
	cmd := ClearCommand{}
	c := cmd.Run(Context{})
	msg := c()
	require.Equal(t, ClearResult{}, msg)
}

func TestParse_config_persona_command(t *testing.T) {
	cmd, err := testParser.Parse("/config persona bard A travelling storyteller")
	require.NoError(t, err)
	require.Equal(t, PersonaConfig{ID: "bard", Description: []string{"A", "travelling", "storyteller"}}, cmd)
}

func TestComplete_config_persona_no_value_suggestions(t *testing.T) {
	c := complete(t, "/config persona ")

	require.True(t, c.Visible)
	require.Equal(t, []command.Suggestion(nil), c.Suggestions)
}

func TestComplete_live_data_reflects_changes(t *testing.T) {
	var channels []domain.Channel

	ctx := CompletionContext{
		Channels: func() iter.Seq[domain.Channel] { return slices.Values(channels) },
		UserNick: func() domain.Nick { return "u" },
		Kind:     func() domain.ChannelKind { return domain.KindChannel },
	}

	cs := testSet(ctx)

	before := cs.Complete("/join ", 6)
	require.Equal(t, command.Completion{
		Visible:      true,
		Suggestions:  []command.Suggestion{},
		ReplaceStart: 6,
		ReplaceEnd:   6,
	}, before)

	// Mutate the underlying data — the live context sees the change.
	channels = []domain.Channel{{Name: "#new"}}

	after := cs.Complete("/join ", 6)
	require.Equal(t, command.Completion{
		Visible:      true,
		ReplaceStart: 6,
		ReplaceEnd:   6,
		Suggestions: []command.Suggestion{
			{Value: "#new", Label: "#new"},
		},
	}, after)
}
