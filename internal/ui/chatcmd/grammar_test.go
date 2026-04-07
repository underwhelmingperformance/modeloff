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

func testSources() Sources {
	channels := []domain.Channel{
		{Name: "#general", Topic: "Welcome"},
		{Name: "#random"},
	}
	instances := []domain.Instance{
		{Nick: "haiku", ModelID: "anthropic/haiku", Channels: func() *orderedmap.OrderedMap[domain.ChannelName, time.Time] {
			m := orderedmap.New[domain.ChannelName, time.Time]()
			m.Set("#general", time.Time{})
			return m
		}()},
		{Nick: "sonnet", ModelID: "anthropic/sonnet"},
	}
	members := []domain.Nick{"testuser", "haiku"}
	models := []ModelOption{
		{ID: "anthropic/haiku", Name: "Haiku"},
		{ID: "anthropic/sonnet", Name: "Sonnet"},
	}

	return Sources{
		Channels:      func() iter.Seq[domain.Channel] { return slices.Values(channels) },
		Instances:     func() iter.Seq[domain.Instance] { return slices.Values(instances) },
		ActiveChannel: func() domain.ChannelName { return "#general" },
		ActiveMembers: func() iter.Seq[domain.Nick] { return slices.Values(members) },
		UserNick:      func() domain.Nick { return "testuser" },
		LiveModels:    func() []ModelOption { return models },
	}
}

func complete(t *testing.T, input string) command.Completion {
	t.Helper()

	parser := BuildParser(testSources())

	return command.Complete(parser.Set(), input, len(input))
}

func suggestionValues(c command.Completion) []string {
	values := make([]string, len(c.Suggestions))
	for i, s := range c.Suggestions {
		values[i] = s.Value
	}

	return values
}

func TestBuildParser_produces_all_commands(t *testing.T) {
	parser := BuildParser(testSources())
	set := parser.Set()

	names := make([]string, 0, len(set.Commands))
	for _, node := range set.Commands {
		names = append(names, node.Name)
	}

	require.Equal(t, []string{
		"join", "part", "list", "invite", "kick",
		"msg", "nick", "topic", "me", "whois", "config",
		"help", "quit",
	}, names)
}

func TestBuildParser_parse_returns_typed_command(t *testing.T) {
	parser := BuildParser(testSources())

	cmd, err := parser.Parse("/help")
	require.NoError(t, err)
	require.NotNil(t, cmd)
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

func TestComplete_invite_suggests_reusable_then_live(t *testing.T) {
	c := complete(t, "/invite ")

	require.True(t, c.Visible)

	values := suggestionValues(c)
	// sonnet is reusable (not in #general), haiku is excluded (already in #general)
	// then live models follow
	require.Equal(t, []string{"sonnet", "anthropic/haiku", "anthropic/sonnet"}, values)
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
		"nick-model", "embedding-model", "highlight", "timestamp-format",
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
	require.Empty(t, c.Suggestions)
}

func TestComplete_live_data_reflects_changes(t *testing.T) {
	var channels []domain.Channel

	src := Sources{
		Channels: func() iter.Seq[domain.Channel] { return slices.Values(channels) },
		UserNick: func() domain.Nick { return "u" },
	}

	parser := BuildParser(src)

	before := command.Complete(parser.Set(), "/join ", 6)
	require.Empty(t, before.Suggestions)

	// Mutate the underlying data — the same parser sees the change.
	channels = []domain.Channel{{Name: "#new"}}

	after := command.Complete(parser.Set(), "/join ", 6)
	require.Equal(t, []string{"#new"}, suggestionValues(after))
}
