package chatcmd

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/store/storetest"
)

func TestBuildToolRegistry_returns_expected_tools(t *testing.T) {
	reg, err := BuildToolRegistry()
	require.NoError(t, err)

	defs := reg.Definitions()

	got := make([]api.ToolDefinition, len(defs))
	copy(got, defs)

	require.Equal(t, []api.ToolDefinition{
		{
			Name:        "join",
			Description: "Switch to a channel or create it if needed.",
			Parameters:  toolParams("join"),
		},
		{
			Name:        "part",
			Description: "Leave the current channel with an optional farewell message.",
			Parameters:  toolParams("part"),
		},
		{
			Name:        "list",
			Description: "List all known channels.",
			Parameters:  toolParams("list"),
		},
		{
			Name:        "invite",
			Description: "Invite a nick to a channel.",
			Parameters:  toolParams("invite"),
		},
		{
			Name:        "kick",
			Description: "Remove a nick from the current channel.",
			Parameters:  toolParams("kick"),
		},
		{
			Name:        "msg",
			Description: "Open a direct message and optionally send text.",
			Parameters:  toolParams("msg"),
		},
		{
			Name:        "nick",
			Description: "Change your nickname.",
			Parameters:  toolParams("nick"),
		},
		{
			Name:        "topic",
			Description: "Set or clear the current channel topic.",
			Parameters:  toolParams("topic"),
		},
		{
			Name:        "me",
			Description: "Send an action message (e.g. /me waves).",
			Parameters:  toolParams("me"),
		},
		{
			Name:        "whois",
			Description: "Show details about a model instance.",
			Parameters:  toolParams("whois"),
		},
		{
			Name:        "help",
			Description: "Show available commands.",
			Parameters:  toolParams("help"),
		},
		{
			Name:        "quit",
			Description: "Shut down your instance and leave all channels.",
			Parameters:  toolParams("quit"),
		},
	}, got)
}

func TestBuildToolRegistry_tool_tag_overrides_help(t *testing.T) {
	reg, err := BuildToolRegistry()
	require.NoError(t, err)

	defs := reg.Definitions()
	byName := make(map[string]api.ToolDefinition, len(defs))
	for _, d := range defs {
		byName[d.Name] = d
	}

	// Non-empty tool:"..." tag overrides the help text.
	require.Equal(t,
		"Leave the current channel with an optional farewell message.",
		byName["part"].Description,
	)
	require.Equal(t,
		"Shut down your instance and leave all channels.",
		byName["quit"].Description,
	)

	// Empty tool:"" tag falls back to the help text.
	require.Equal(t,
		"Switch to a channel or create it if needed.",
		byName["join"].Description,
	)
	require.Equal(t,
		"List all known channels.",
		byName["list"].Description,
	)
}

// toolParams returns ToolParameters for the named tool node in the grammar.
func toolParams(name string) map[string]any {
	set := command.Build(&Grammar{})

	for _, node := range set.ToolNodes() {
		if node.ToolName() == name {
			return node.ToolParameters()
		}
	}

	return nil
}

type toolTestAPI struct{}

func (toolTestAPI) ListModels(context.Context) ([]api.ModelInfo, error) { return nil, nil }

func (toolTestAPI) SendEvents(
	context.Context,
	domain.ModelID,
	string,
	string,
	[]protocol.IRCMessage,
	[]protocol.IRCMessage,
	...api.ToolDefinition,
) (api.CompletionResult, error) {
	return api.CompletionResult{
		Response: protocol.ModelResponse{Kind: protocol.ResponseSilence},
	}, nil
}

func (toolTestAPI) ContinueWithToolResults(
	context.Context,
	*api.Conversation,
	[]api.ToolResult,
	...api.ToolDefinition,
) (api.CompletionResult, error) {
	return api.CompletionResult{
		Response: protocol.ModelResponse{Kind: protocol.ResponseSilence},
	}, nil
}

func (toolTestAPI) GenerateNick(context.Context, domain.ModelID, domain.ModelID) (api.NicknameResult, error) {
	return api.NicknameResult{Nick: "testbot"}, nil
}

func (toolTestAPI) GeneratePersonas(context.Context, domain.ModelID) ([]domain.Persona, error) {
	return nil, nil
}

func newToolTestSession(t *testing.T) *session.Session {
	t.Helper()

	s := storetest.NewMemoryStore(t)
	return session.New(s, nil, toolTestAPI{}, "testuser", "", "")
}

func toolValue(t *testing.T, name string, rawJSON string) any {
	t.Helper()

	set := command.Build(&Grammar{})

	for _, node := range set.ToolNodes() {
		if node.ToolName() == name {
			v, err := node.ToolValue(json.RawMessage(rawJSON))
			require.NoError(t, err)
			return v
		}
	}

	t.Fatalf("tool %q not found", name)
	return nil
}

func TestRunTool_join_with_channel(t *testing.T) {
	sess := newToolTestSession(t)
	tc := session.ToolContext{
		Session: sess,
		Actor:   "testuser",
		Channel: "#general",
	}

	v := toolValue(t, "join", `{"channel": "#testing"}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "JoinCommand should implement ToolCommand")

	result := tool.RunTool(t.Context(), tc)

	require.Equal(t, session.ToolResultPayload{
		OK:      true,
		Summary: "joined #testing",
	}, result)
}

func TestRunTool_help_no_args(t *testing.T) {
	tc := session.ToolContext{
		Actor:   "testuser",
		Channel: "#general",
	}

	v := toolValue(t, "help", `{}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "HelpCommand should implement ToolCommand")

	result := tool.RunTool(t.Context(), tc)

	require.Equal(t, session.ToolResultPayload{
		OK:      true,
		Summary: "available command tools include join, part, list, invite, kick, msg, nick, topic, me, whois, help, and quit",
	}, result)
}

func TestRunTool_part_no_channel_returns_error(t *testing.T) {
	tc := session.ToolContext{
		Actor: "testuser",
	}

	v := toolValue(t, "part", `{}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "PartCommand should implement ToolCommand")

	result := tool.RunTool(t.Context(), tc)

	require.Equal(t, session.ToolResultPayload{
		OK:    false,
		Error: "no active channel",
	}, result)
}

func TestRunTool_kick_no_channel_returns_error(t *testing.T) {
	tc := session.ToolContext{
		Actor: "testuser",
	}

	v := toolValue(t, "kick", `{"nick": "haiku"}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "KickCommand should implement ToolCommand")

	result := tool.RunTool(t.Context(), tc)

	require.Equal(t, session.ToolResultPayload{
		OK:    false,
		Error: "no active channel",
	}, result)
}

func TestRunTool_invite_missing_nick_returns_error(t *testing.T) {
	tc := session.ToolContext{
		Actor:   "testuser",
		Channel: "#general",
	}

	v := toolValue(t, "invite", `{}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "InviteCommand should implement ToolCommand")

	result := tool.RunTool(t.Context(), tc)

	require.Equal(t, session.ToolResultPayload{
		OK:    false,
		Error: "target nick is required",
	}, result)
}

func TestRunTool_msg_opens_dm(t *testing.T) {
	sess := newToolTestSession(t)

	// Join a channel and add a model so the nick resolves.
	require.NoError(t, sess.Join(t.Context(), "#lobby"))
	require.NoError(t, sess.AddModel(t.Context(), "#lobby", "anthropic/haiku", ""))

	tc := session.ToolContext{
		Session: sess,
		Actor:   "testuser",
	}

	v := toolValue(t, "msg", `{"nick": "testbot"}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "MsgCommand should implement ToolCommand")

	result := tool.RunTool(t.Context(), tc)

	require.True(t, result.OK)
	require.Contains(t, result.Summary, "testbot")
}

func TestRunTool_nick_changes_nick(t *testing.T) {
	sess := newToolTestSession(t)
	tc := session.ToolContext{
		Session: sess,
		Actor:   "testuser",
		Channel: "#general",
	}

	v := toolValue(t, "nick", `{"new_nick": "newname"}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "NickCommand should implement ToolCommand")

	result := tool.RunTool(t.Context(), tc)

	require.Equal(t, session.ToolResultPayload{
		OK:      true,
		Summary: "changed nick to newname",
	}, result)
}

func TestRunTool_me_no_channel_returns_error(t *testing.T) {
	tc := session.ToolContext{
		Actor: "testuser",
	}

	v := toolValue(t, "me", `{"action": ["waves"]}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "MeCommand should implement ToolCommand")

	result := tool.RunTool(t.Context(), tc)

	require.Equal(t, session.ToolResultPayload{
		OK:    false,
		Error: "no active channel",
	}, result)
}

func TestRunTool_quit_succeeds(t *testing.T) {
	sess := newToolTestSession(t)
	tc := session.ToolContext{
		Session: sess,
		Actor:   "testuser",
	}

	v := toolValue(t, "quit", `{}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "QuitCommand should implement ToolCommand")

	result := tool.RunTool(t.Context(), tc)

	require.Equal(t, session.ToolResultPayload{
		OK:      true,
		Summary: "shut down and left all channels",
	}, result)
}
