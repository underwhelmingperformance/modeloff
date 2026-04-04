package chatcmd

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/set"
)

func testSources() Sources {
	return Sources{
		Channels: func() []domain.Channel {
			return []domain.Channel{
				{Name: "#general", Topic: "Welcome"},
				{Name: "#random"},
			}
		},
		Instances: func() []domain.ModelInstance {
			return []domain.ModelInstance{
				{Nick: "haiku", ModelID: "anthropic/haiku", Channels: set.NewOrdered[domain.ChannelName]("#general")},
				{Nick: "sonnet", ModelID: "anthropic/sonnet"},
			}
		},
		ActiveChannel: func() domain.ChannelName { return "#general" },
		ActiveMembers: func() []domain.Nick { return []domain.Nick{"testuser", "haiku"} },
		UserNick:      func() domain.Nick { return "testuser" },
		LiveModels: func() []ModelOption {
			return []ModelOption{
				{ID: "anthropic/haiku", Name: "Haiku"},
				{ID: "anthropic/sonnet", Name: "Sonnet"},
			}
		},
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

func TestComplete_config_suggests_keys(t *testing.T) {
	c := complete(t, "/config ")

	require.True(t, c.Visible)
	require.Equal(t, []string{"api-key", "nick-model", "poke-interval"}, suggestionValues(c))
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

func TestComplete_sources_read_live_data(t *testing.T) {
	var channels []domain.Channel

	src := Sources{
		Channels:      func() []domain.Channel { return channels },
		Instances:     func() []domain.ModelInstance { return nil },
		ActiveChannel: func() domain.ChannelName { return "" },
		ActiveMembers: func() []domain.Nick { return nil },
		UserNick:      func() domain.Nick { return "u" },
		LiveModels:    func() []ModelOption { return nil },
	}

	parser := BuildParser(src)
	set := parser.Set()

	before := command.Complete(set, "/join ", 6)
	require.Empty(t, before.Suggestions)

	channels = []domain.Channel{{Name: "#new"}}

	after := command.Complete(set, "/join ", 6)
	require.Equal(t, []string{"#new"}, suggestionValues(after))
}
