package chatcmd

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/store/storetest"
	"github.com/laney/modeloff/internal/ui/uitest"
	"github.com/laney/modeloff/internal/userclient"
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
			Parameters:  toolParams(t, "join"),
		},
		{
			Name:        "part",
			Description: "Leave the current channel for an extended absence — a real exit, not a brief away. The message is the parting line peers see; an empty message parts silently. Do NOT part for short absences (brb, afk, food, sleep): just say so in chat and stay. Parting drops you from the channel; rejoining requires a fresh JOIN or an invite. Reserve PART for when you genuinely intend to leave the room.",
			Parameters:  toolParams(t, "part"),
		},
		{
			Name:        "list",
			Description: "List all known channels.",
			Parameters:  toolParams(t, "list"),
		},
		{
			Name:        "add_model",
			Description: "Add a new model instance to the current channel by model ID, optionally with a persona.",
			Parameters:  toolParams(t, "add_model"),
		},
		{
			Name:        "invite",
			Description: "Invite a nick to a channel.",
			Parameters:  toolParams(t, "invite"),
		},
		{
			Name:        "kick",
			Description: "Remove a nick from the current channel.",
			Parameters:  toolParams(t, "kick"),
		},
		{
			Name:        "kill",
			Description: "Forcibly disconnect a model instance from the server with a reason.",
			Parameters:  toolParams(t, "kill"),
		},
		{
			Name:        "msg",
			Description: "Send a message addressed to either a #channel you are in, or a user (by nick). The recipient sees the message and may reply.",
			Parameters:  toolParams(t, "msg"),
		},
		{
			Name:        "nick",
			Description: "Change your nickname.",
			Parameters:  toolParams(t, "nick"),
		},
		{
			Name:        "topic",
			Description: "Set or clear the current channel topic.",
			Parameters:  toolParams(t, "topic"),
		},
		{
			Name:        "mode",
			Description: "Set or clear one or more channel modes. Syntax: <modes> [args]. Examples: +o nick, +tn, -i+l 10, +k secret, +ov-i alice bob.",
			Parameters:  toolParams(t, "mode"),
		},
		{
			Name:        "me",
			Description: "Send an action message (e.g. /me waves).",
			Parameters:  toolParams(t, "me"),
		},
		{
			Name:        "whois",
			Description: "Show details about a model instance.",
			Parameters:  toolParams(t, "whois"),
		},
		{
			Name:        "help",
			Description: "Show available commands.",
			Parameters:  toolParams(t, "help"),
		},
		{
			Name:        "quit",
			Description: "Shut down your instance and leave all channels.",
			Parameters:  toolParams(t, "quit"),
		},
		{
			Name:        "pass",
			Description: "Explicitly record that you have nothing to say this turn, with a brief reason. Silence is the default — you only need to call this if you want the reason captured for observability. Do not call this in the same turn as a msg or me tool.",
			Parameters:  toolParams(t, "pass"),
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
		"Leave the current channel for an extended absence — a real exit, not a brief away. The message is the parting line peers see; an empty message parts silently. Do NOT part for short absences (brb, afk, food, sleep): just say so in chat and stay. Parting drops you from the channel; rejoining requires a fresh JOIN or an invite. Reserve PART for when you genuinely intend to leave the room.",
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
func toolParams(t *testing.T, name string) map[string]any {
	t.Helper()

	set, err := command.Build[CompletionContext](&Grammar{})
	require.NoError(t, err)

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
	domain.InstanceID,
	string,
	[]protocol.IRCMessage,
	[]protocol.IRCMessage,
	...api.ToolDefinition,
) (api.CompletionResult, error) {
	return api.CompletionResult{}, nil
}

func (toolTestAPI) ContinueWithToolResults(
	context.Context,
	*api.Conversation,
	[]api.ToolResult,
	...api.ToolDefinition,
) (api.CompletionResult, error) {
	return api.CompletionResult{}, nil
}

func (toolTestAPI) GenerateNick(context.Context, domain.ModelID, string, []domain.Nick) (api.NicknameResult, error) {
	return api.NicknameResult{Nick: "testbot"}, nil
}

func (toolTestAPI) GeneratePersonas(context.Context, domain.ModelID) ([]domain.Persona, error) {
	return nil, nil
}

func newToolTestSession(t *testing.T) (*session.Session, *userclient.UserClient) {
	t.Helper()

	s := storetest.NewMemoryStore(t)
	apiClient := toolTestAPI{}
	sess, _, user := uitest.NewTestSession(t, s, apiClient, nil, nil, "", "", t.Context)
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

	return sess, user
}

// userToolContext returns the [modelclient.ToolContext] tests use when
// invoking `RunTool` as the user. The user-client handle is the
// active actor so dispatched commands route through the same
// [protocol.Client.Send] path the chat-screen exercises.
func userToolContext(sess *session.Session, user *userclient.UserClient, channel domain.ChannelName) modelclient.ToolContext {
	return modelclient.ToolContext{
		Session: sess,
		Actor:   user.Instance(),
		Channel: channel,
		Client:  user,
	}
}

func toolValue(t *testing.T, name string, rawJSON string) any {
	t.Helper()

	set, err := command.Build[CompletionContext](&Grammar{})
	require.NoError(t, err)

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
	sess, user := newToolTestSession(t)
	tc := userToolContext(sess, user, "#general")

	v := toolValue(t, "join", `{"channel": "#testing"}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "JoinCommand should implement ToolCommand")

	result := tool.RunTool(t.Context(), tc)

	require.Equal(t, modelclient.ToolResultPayload{
		OK:      true,
		Summary: "joined #testing",
	}, result)
}

func TestRunTool_help_no_args(t *testing.T) {
	sess, user := newToolTestSession(t)
	tc := userToolContext(sess, user, "#general")

	v := toolValue(t, "help", `{}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "HelpCommand should implement ToolCommand")

	result := tool.RunTool(t.Context(), tc)

	require.Equal(t, modelclient.ToolResultPayload{
		OK:      true,
		Summary: "available command tools include join, part, list, invite, kick, msg, nick, topic, me, whois, help, and quit",
	}, result)
}

func TestRunTool_part_no_channel_returns_error(t *testing.T) {
	sess, user := newToolTestSession(t)
	tc := userToolContext(sess, user, "")

	v := toolValue(t, "part", `{}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "PartCommand should implement ToolCommand")

	result := tool.RunTool(t.Context(), tc)

	require.Equal(t, modelclient.ToolResultPayload{
		OK:    false,
		Error: "no active channel",
	}, result)
}

func TestRunTool_kick_no_channel_returns_error(t *testing.T) {
	sess, user := newToolTestSession(t)
	tc := userToolContext(sess, user, "")

	v := toolValue(t, "kick", `{"nick": "haiku"}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "KickCommand should implement ToolCommand")

	result := tool.RunTool(t.Context(), tc)

	require.Equal(t, modelclient.ToolResultPayload{
		OK:    false,
		Error: "no active channel",
	}, result)
}

func TestRunTool_invite_missing_nick_returns_error(t *testing.T) {
	sess, user := newToolTestSession(t)
	tc := userToolContext(sess, user, "#general")

	v := toolValue(t, "invite", `{}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "InviteCommand should implement ToolCommand")

	result := tool.RunTool(t.Context(), tc)

	require.Equal(t, modelclient.ToolResultPayload{
		OK:    false,
		Error: "target nick is required",
	}, result)
}

func TestRunTool_invite_unknown_nick_reports_failure(t *testing.T) {
	sess, user := newToolTestSession(t)

	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#general")))
	tc := userToolContext(sess, user, "#general")

	v := toolValue(t, "invite", `{"nick": "nobody"}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "InviteCommand should implement ToolCommand")

	require.Equal(t, modelclient.ToolResultPayload{
		OK:    false,
		Error: "no such nick: nobody",
	}, tool.RunTool(t.Context(), tc))
}

func TestRunTool_msg_sends_to_nick(t *testing.T) {
	sess, user := newToolTestSession(t)

	// Join a channel and add a model so the nick resolves.
	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#lobby")))
	uitest.AddModel(t, user, "#lobby", "anthropic/haiku", "")

	tc := userToolContext(sess, user, "")

	v := toolValue(t, "msg", `{"target": "testbot", "body": ["hello"]}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "MsgCommand should implement ToolCommand")

	result := tool.RunTool(t.Context(), tc)

	require.True(t, result.OK)
	require.Equal(t, "messaged testbot", result.Summary)
}

func TestRunTool_msg_rejects_empty_body(t *testing.T) {
	sess, user := newToolTestSession(t)

	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#lobby")))
	uitest.AddModel(t, user, "#lobby", "anthropic/haiku", "")

	tc := userToolContext(sess, user, "")

	v := toolValue(t, "msg", `{"target": "testbot"}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok)

	require.Equal(t, modelclient.ToolResultPayload{
		OK:    false,
		Error: "reply part must contain exactly one of body or spans",
	}, tool.RunTool(t.Context(), tc))
}

func TestRunTool_whois_stamps_issuing_window(t *testing.T) {
	sess, user := newToolTestSession(t)

	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#lobby")))
	uitest.AddModel(t, user, "#lobby", "anthropic/haiku", "")

	tc := userToolContext(sess, user, "#lobby")

	v := toolValue(t, "whois", `{"nick": "testbot"}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "WhoisCommand should implement ToolCommand")

	result := tool.RunTool(t.Context(), tc)

	require.True(t, result.OK)
	whois, ok := result.Data.(domain.Whois)
	require.True(t, ok, "whois tool returns a domain.Whois snapshot")
	require.Equal(t, domain.ChannelName("#lobby"), whois.Target)
}

func TestRunTool_nick_changes_nick(t *testing.T) {
	sess, user := newToolTestSession(t)
	tc := userToolContext(sess, user, "#general")

	v := toolValue(t, "nick", `{"new_nick": "newname"}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "NickCommand should implement ToolCommand")

	result := tool.RunTool(t.Context(), tc)

	require.Equal(t, modelclient.ToolResultPayload{
		OK:      true,
		Summary: "changed nick to newname",
	}, result)
}

func TestRunTool_me_no_channel_returns_error(t *testing.T) {
	sess, user := newToolTestSession(t)
	tc := userToolContext(sess, user, "")

	v := toolValue(t, "me", `{"action": ["waves"]}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "MeCommand should implement ToolCommand")

	result := tool.RunTool(t.Context(), tc)

	require.Equal(t, modelclient.ToolResultPayload{
		OK:    false,
		Error: "no active channel",
	}, result)
}

func TestRunTool_me_sends_action_to_channel(t *testing.T) {
	sess, user := newToolTestSession(t)
	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#lobby")))
	tc := userToolContext(sess, user, "#lobby")

	v := toolValue(t, "me", `{"action": ["waves"]}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "MeCommand should implement ToolCommand")

	require.Equal(t, modelclient.ToolResultPayload{
		OK:      true,
		Summary: "sent action to #lobby",
	}, tool.RunTool(t.Context(), tc))
}

func TestRunTool_msg_with_spans_renders_irc_formatting(t *testing.T) {
	sess, user := newToolTestSession(t)
	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#lobby")))
	tc := userToolContext(sess, user, "#lobby")

	v := toolValue(t, "msg",
		`{"target": "#lobby", "spans": [{"text": "hello "}, {"text": "world", "style": {"bold": true, "fg": 4}}]}`,
	)

	tool, ok := v.(ToolCommand)
	require.True(t, ok)

	require.Equal(t, modelclient.ToolResultPayload{
		OK:      true,
		Summary: "messaged #lobby",
	}, tool.RunTool(t.Context(), tc))
}

func TestRunTool_quit_succeeds(t *testing.T) {
	sess, user := newToolTestSession(t)
	tc := userToolContext(sess, user, "")

	v := toolValue(t, "quit", `{}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "QuitCommand should implement ToolCommand")

	result := tool.RunTool(t.Context(), tc)

	require.Equal(t, modelclient.ToolResultPayload{
		OK:      true,
		Summary: "shut down and left all channels",
	}, result)
}
