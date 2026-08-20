package chatcmd

import (
	"fmt"
	"iter"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

func testContext(kind domain.ChannelKind) CompletionContext {
	general := domain.NewChannelWindow("#general", time.Time{})
	general.Topic = "Welcome"

	channels := []domain.Window{
		general,
		domain.NewChannelWindow("#random", time.Time{}),
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
		Channels:        func() iter.Seq[domain.Window] { return slices.Values(channels) },
		Instances:       func() iter.Seq[*domain.Instance] { return slices.Values(instances) },
		ActiveMembers:   func() iter.Seq[domain.Nick] { return slices.Values(members) },
		ActiveChannel:   func() domain.ChannelName { return "#general" },
		UserNick:        func() domain.Nick { return "testuser" },
		LiveModels:      func() iter.Seq[ModelOption] { return slices.Values(models) },
		LiveModelsState: func() command.SuggestionState { return command.SuggestionStateReady },
		Personas:        func() iter.Seq[domain.Persona] { return slices.Values(personas) },
		Kind:            func() domain.ChannelKind { return kind },
	}
}

var testParser = func() Parser {
	p, err := NewParser()
	if err != nil {
		panic(err)
	}

	return p
}()

// operatorCaps is a stand-in [command.CapabilityHolder] that
// grants every operator-gated tag. Completion tests use it so the
// operator-only commands surface in the suggestion list — the
// real wiring is via [protocol.Client.Caps] off the user-client,
// which carries `+o` from bootstrap.
type operatorCaps struct{}

func (operatorCaps) Has(c command.Capability) bool { return c == "operator" }

func testSet(ctx CompletionContext) command.CompletionSet[CompletionContext] {
	return command.CompletionSet[CompletionContext]{
		Set:  testParser.Set(),
		Ctx:  ctx,
		Caps: operatorCaps{},
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
		"join", "part", "list", "kill",
		"msg", "query", "nick", "me", "whois", "config",
		"personas", "regenerate-personas",
		"help", "clear", "poke", "quit",
	}, suggestionValues(c))
}

func TestComplete_channel_includes_all_commands(t *testing.T) {
	c := completeInKind(t, "/", domain.KindChannel)

	require.Equal(t, []string{
		"join", "part", "list", "add-model", "invite", "kick", "kill",
		"msg", "query", "nick", "topic", "mode", "me", "whois", "config",
		"personas", "regenerate-personas",
		"help", "clear", "poke", "quit",
	}, suggestionValues(c))
}

func TestNewParser_produces_all_commands(t *testing.T) {
	set := testParser.Set()

	names := make([]string, 0, len(set.Commands))
	for _, node := range set.Commands {
		names = append(names, node.Name)
	}

	require.Equal(t, []string{
		"join", "part", "list", "add-model", "invite", "kick", "kill",
		"msg", "query", "nick", "topic", "mode", "me", "whois", "config",
		"personas", "regenerate-personas",
		"help", "clear", "poke", "quit", "pass",
	}, names)

	join := set.Find("join")
	require.Equal(t, []string{"j"}, join.Aliases)

	quit := set.Find("quit")
	require.Equal(t, []string{"q"}, quit.Aliases)

	help := set.Find("help")
	require.Equal(t, []string{"?"}, help.Aliases)
}

func TestNewParser_parse_returns_typed_command(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "canonical", raw: "/help"},
		{name: "alias", raw: "/?"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := testParser.Parse(tt.raw)
			require.NoError(t, err)
			require.Equal(t, HelpCommand{}, cmd)
		})
	}
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

// TestJoinCommand_ToCommand_multi_target covers RFC 2812 §3.2.1's
// JOIN syntax: `/join` accepts a comma-separated channel list
// ("#a,#b,#c") alongside its ordinary single-channel form, and
// every entry gets the same `#`-prefixing a bare single channel
// does.
func TestJoinCommand_ToCommand_multi_target(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []domain.ChannelName
	}{
		{name: "single channel gets the # prefix", raw: "/join general", want: []domain.ChannelName{"#general"}},
		{name: "already-prefixed channel is unchanged", raw: "/join #general", want: []domain.ChannelName{"#general"}},
		{name: "a comma-separated list splits into its channels", raw: "/join #a,#b,#c", want: []domain.ChannelName{"#a", "#b", "#c"}},
		{name: "each entry in an unprefixed list gets its own #", raw: "/join a,b,c", want: []domain.ChannelName{"#a", "#b", "#c"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := testParser.Parse(tt.raw)
			require.NoError(t, err)

			join, ok := parsed.(JoinCommand)
			require.True(t, ok, "expected a JoinCommand, got %T", parsed)

			wire, err := join.ToCommand(Context{})
			require.NoError(t, err)
			require.Equal(t, protocol.Join{Channels: tt.want}, wire)
		})
	}
}

// TestJoinCommand_comma_hygiene pins the two malformed shapes that
// stay within one space-delimited token: a trailing comma and a
// doubled comma. Both leave an empty entry once split; Decode drops
// it, which keeps a stray comma from manufacturing a channel named
// bare "#".
func TestJoinCommand_comma_hygiene(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []domain.ChannelName
	}{
		{name: "trailing comma drops the empty entry", raw: "/join #a,", want: []domain.ChannelName{"#a"}},
		{name: "doubled comma drops the empty entry", raw: "/join #a,,#b", want: []domain.ChannelName{"#a", "#b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := testParser.Parse(tt.raw)
			require.NoError(t, err)

			join, ok := parsed.(JoinCommand)
			require.True(t, ok, "expected a JoinCommand, got %T", parsed)

			wire, err := join.ToCommand(Context{})
			require.NoError(t, err)
			require.Equal(t, protocol.Join{Channels: tt.want}, wire)
		})
	}
}

// TestJoinCommand_refuses_an_argument_with_no_channel covers a
// comma-only argument ("/join ,"): every entry is empty once
// trimmed, so nothing survives to prefix. Parsing refuses the
// argument, so nothing later can silently join a channel named bare
// "#".
func TestJoinCommand_refuses_an_argument_with_no_channel(t *testing.T) {
	_, err := testParser.Parse("/join ,")
	require.Error(t, err)
}

// TestJoinCommand_refuses_a_key_that_looks_like_a_channel covers
// the shape a space after a comma produces. The CLI grammar reads
// the channel argument and the key argument as separate tokens, so
// "/join #a, #b" parses as Channel "#a," and Key "#b" before Decode
// ever sees a second channel; Decode then drops "#a,"'s trailing
// empty entry down to "#a", leaving no parse error to catch the
// mistake. ToCommand is where it is caught: a key that itself looks
// like a channel name is refused, so "#b" is not silently taken as
// a password and dropped from the join.
func TestJoinCommand_refuses_a_key_that_looks_like_a_channel(t *testing.T) {
	parsed, err := testParser.Parse("/join #a, #b")
	require.NoError(t, err)

	join, ok := parsed.(JoinCommand)
	require.True(t, ok, "expected a JoinCommand, got %T", parsed)
	require.Equal(t, ChannelArg("#a"), join.Channel)
	require.Equal(t, "#b", join.Key)

	_, err = join.ToCommand(Context{})
	require.Error(t, err)
}

// TestJoinOutcome_Text covers how a JOIN's per-channel outcomes
// render into the one line the tool-result payload and the
// chat-screen's notice both carry.
func TestJoinOutcome_Text(t *testing.T) {
	tests := []struct {
		name   string
		events []protocol.Event
		want   string
	}{
		{
			name: "every channel joined",
			events: []protocol.Event{
				domain.JoinedChannel{Channel: "#a"},
				domain.JoinedChannel{Channel: "#b"},
			},
			want: "joined #a, #b",
		},
		{
			name:   "every channel was refused",
			events: []protocol.Event{domain.ChannelFullError{Channel: "#a"}},
			want:   "cannot join #a: channel is full",
		},
		{
			name: "a mix of joined and refused channels",
			events: []protocol.Event{
				domain.JoinedChannel{Channel: "#a"},
				domain.ChannelInviteOnlyError{Channel: "#b"},
			},
			want: "joined #a; cannot join #b: invite-only channel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, newJoinOutcome(tt.events).Text())
		})
	}
}

// TestJoinCommand_help_states_the_channel_cap pins the join
// grammar's help text against [protocol.MaxJoinTargets]: a change
// to the cap that leaves the text stale fails this test.
func TestJoinCommand_help_states_the_channel_cap(t *testing.T) {
	node := testParser.Set().Find("join")
	require.NotNil(t, node)
	require.Len(t, node.Positionals, 2)
	require.Equal(t,
		fmt.Sprintf("Channel to join or create, or a comma-separated list of up to %d to join at once", protocol.MaxJoinTargets),
		node.Positionals[0].Help,
	)
}

func TestComplete_kick_suggests_active_members_excluding_self(t *testing.T) {
	c := complete(t, "/kick ")

	require.True(t, c.Visible)
	require.Equal(t, []string{"haiku"}, suggestionValues(c))
}

func TestComplete_add_model_suggests_only_live_models(t *testing.T) {
	c := complete(t, "/add-model ")

	require.True(t, c.Visible)

	// `/add-model` always creates a fresh instance, so existing instance
	// nicks are not valid inputs; only live OpenRouter model IDs are.
	require.Equal(t, []string{"anthropic/haiku", "anthropic/sonnet"}, suggestionValues(c))
}

func TestComplete_add_model_persona_suggests_personas(t *testing.T) {
	c := complete(t, "/add-model somemodel --persona ")

	require.True(t, c.Visible)
	require.Equal(t, []string{"bard", "sage"}, suggestionValues(c))
}

func TestComplete_add_model_hides_completion_when_live_models_failed(t *testing.T) {
	ctx := testContext(domain.KindChannel)
	ctx.LiveModelsState = func() command.SuggestionState { return command.SuggestionStateError }

	c := testSet(ctx).Complete("/add-model ", len("/add-model "))

	require.Equal(t, command.Completion{}, c)
}

func TestComplete_invite_suggests_instance_nicks(t *testing.T) {
	c := complete(t, "/invite ")

	require.True(t, c.Visible)
	require.Equal(t, []string{"haiku", "sonnet"}, suggestionValues(c))
}

func TestComplete_msg_suggests_channels_and_instances(t *testing.T) {
	c := complete(t, "/msg ")

	require.True(t, c.Visible)
	require.Equal(t, []string{"#general", "#random", "haiku", "sonnet"}, suggestionValues(c))
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
		"api-key", "base-url", "poke-interval", "drain-timeout",
		"small-model", "embedding-model", "highlight", "default-modes", "timestamp-format", "persona", "--reset",
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
		"api-key", "base-url", "poke-interval", "drain-timeout",
		"small-model", "embedding-model", "highlight", "default-modes", "timestamp-format", "persona",
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
	c := cmd.Run(t.Context(), Context{})
	msg := c()
	require.Equal(t, ClearResult{}, msg)
}

func TestParse_poke_command(t *testing.T) {
	cmd, err := testParser.Parse("/poke")
	require.NoError(t, err)
	require.Equal(t, PokeCommand{}, cmd)
}

func TestPokeCommand_Run_returns_PokeRequested(t *testing.T) {
	cmd := PokeCommand{}
	c := cmd.Run(t.Context(), Context{})
	msg := c()
	require.Equal(t, PokeRequested{}, msg)
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
	var channels []domain.Window

	ctx := CompletionContext{
		Channels: func() iter.Seq[domain.Window] { return slices.Values(channels) },
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
	channels = []domain.Window{domain.NewChannelWindow("#new", time.Time{})}

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
