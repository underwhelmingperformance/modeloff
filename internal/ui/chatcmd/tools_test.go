package chatcmd

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/store/storetest"
	"github.com/laney/modeloff/internal/testclient"
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
			Description: "Send one or more messages addressed to either a #channel you are in, or a user by nick. Each body array element is delivered as a separate IRC message in order.",
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
			Description: "Set or clear one or more channel modes. Supply each change as an object; the mode descriptions explain their effects and parameters.",
			Parameters:  toolParams(t, "mode"),
		},
		{
			Name:        "me",
			Description: "Send one or more /me actions to the current window. Each action array element is delivered separately in order.",
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

func TestModeToolSchema_describes_declarative_changes(t *testing.T) {
	require.Equal(t, expectedModeToolSchema(), toolParams(t, "mode"))
}

type modeToolSchemaSpec struct {
	name                 string
	description          string
	parameter            string
	parameterDescription string
	parameterType        string
	parameterOnRemove    bool
}

func expectedModeToolSchema() map[string]any {
	specs := []modeToolSchemaSpec{
		{name: "operator", description: "Grant or revoke channel-operator status for the target nick. Channel operators can perform moderation tasks.", parameter: "target", parameterDescription: "The member whose channel-operator status should change.", parameterType: "string", parameterOnRemove: true},
		{name: "voice", description: "Grant or revoke voice for the target nick. Voice allows a member to speak in a moderated channel.", parameter: "target", parameterDescription: "The member whose voice status should change.", parameterType: "string", parameterOnRemove: true},
		{name: "anonymous", description: "Hide member identities in messages and membership events."},
		{name: "invite_only", description: "Require a client to receive an invitation before joining the channel."},
		{name: "moderated", description: "Allow only channel operators and voiced members to send messages or actions to the channel."},
		{name: "no_external", description: "Require a client to be joined to the channel before sending messages or actions to it."},
		{name: "private", description: "Hide the channel from LIST and WHOIS for clients who are not joined to it."},
		{name: "quiet", description: "Allow only channel operators to send messages or actions to the channel."},
		{name: "secret", description: "Hide the channel from LIST and WHOIS for clients who are not joined to it."},
		{name: "topic_lock", description: "Require channel-operator status to change the channel topic."},
		{name: "user_limit", description: "Limit how many clients may be joined to the channel.", parameter: "limit", parameterDescription: "A positive integer giving the maximum number of clients that may be joined.", parameterType: "integer"},
		{name: "key", description: "Require clients to supply the channel key when joining.", parameter: "key", parameterDescription: "The key a client must supply when joining.", parameterType: "string"},
		{name: "flood_limit", description: "Limit how many messages the channel accepts in each flood window.", parameter: "limit", parameterDescription: "A positive integer giving the maximum number of messages the channel accepts in each flood window.", parameterType: "integer"},
	}

	branches := make([]any, 0, len(specs)*2)
	for _, spec := range specs {
		branches = append(branches,
			expectedModeToolSchemaBranch(spec, "add", spec.parameter != ""),
			expectedModeToolSchemaBranch(spec, "remove", spec.parameterOnRemove),
		)
	}

	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"changes": map[string]any{
				"type":        "array",
				"items":       map[string]any{"anyOf": branches},
				"description": "One to 16 ordered channel-mode changes.",
			},
		},
		"required":             []string{"changes"},
		"additionalProperties": false,
	}
}

func expectedModeToolSchemaBranch(spec modeToolSchemaSpec, change string, withParameter bool) map[string]any {
	properties := map[string]any{
		"change": map[string]any{
			"type":        "string",
			"enum":        []any{change},
			"description": "Whether to add or remove the mode.",
		},
		"mode": map[string]any{
			"type":        "string",
			"enum":        []any{spec.name},
			"description": spec.description,
		},
	}
	required := []string{"change", "mode"}
	if withParameter {
		properties[spec.parameter] = map[string]any{
			"type":        spec.parameterType,
			"description": spec.parameterDescription,
		}
		required = append(required, spec.parameter)
	}

	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}
}

func TestModeToolValue_decodes_and_validates_changes(t *testing.T) {
	set, err := command.Build[CompletionContext](&Grammar{})
	require.NoError(t, err)
	node := set.Find("mode")

	for name, limit := range map[string]string{
		"integer literal":  "10",
		"decimal integer":  "10.0",
		"exponent integer": "1e1",
	} {
		t.Run(name, func(t *testing.T) {
			value, err := node.ToolValue(json.RawMessage(`{
				"changes": [
					{"change":"remove","mode":"invite_only"},
					{"change":"add","mode":"user_limit","limit":` + limit + `}
				]
			}`))

			require.NoError(t, err)
			require.Equal(t, ModeCommand{Changes: ModeChanges{
				{Change: Remove, Mode: ModeInviteOnly{}},
				{Change: Add, Mode: ModeUserLimit{Limit: 10}},
			}}, value)
		})
	}

	_, err = node.ToolValue(json.RawMessage(`{
		"changes": [
			{"change":"add","mode":"quiet","target":"alice"}
		]
	}`))
	var validation *command.ToolArgumentsValidationError
	require.ErrorAs(t, err, &validation)

	_, err = node.ToolValue(json.RawMessage(`{
		"changes": [
			{"change":"add","mode":"user_limit","limit":0}
		]
	}`))
	require.ErrorAs(t, err, &validation)
}

func TestFreeTextToolFields_are_strings(t *testing.T) {
	tests := []struct {
		tool            string
		field           string
		wantDescription string
	}{
		{tool: "part", field: "message", wantDescription: "Optional farewell message"},
		{tool: "kill", field: "reason", wantDescription: "Optional reason; defaults to 'No reason given'."},
		{tool: "topic", field: "topic", wantDescription: "Topic text"},
		{tool: "quit", field: "message", wantDescription: "Optional farewell message"},
	}

	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			properties := toolParams(t, tt.tool)["properties"].(map[string]any)
			field := properties[tt.field].(map[string]any)

			require.Equal(t, map[string]any{
				"type":        []any{"string", "null"},
				"description": tt.wantDescription,
			}, field)
		})
	}
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
func userToolContext(sess *session.Session, user *userclient.UserClient, target protocol.MsgTarget) modelclient.ToolContext {
	return modelclient.ToolContext{
		Session: sess,
		Actor:   user.Instance(),
		Target:  target,
		Client:  user,
	}
}

type channelEventReader interface {
	EventsBefore(context.Context, domain.ChannelName, *int64, int) ([]domain.StoredEvent, error)
}

type messageShape struct {
	Body   string
	Action bool
}

func storedMessageShapes(t *testing.T, eventStore channelEventReader, channel domain.ChannelName) []messageShape {
	t.Helper()

	events, err := eventStore.EventsBefore(t.Context(), channel, nil, 100)
	require.NoError(t, err)

	return storedMessageShapesFromEvents(events)
}

func storedMessageShapesFromEvents(events []domain.StoredEvent) []messageShape {
	var messages []messageShape
	for _, stored := range events {
		message, ok := stored.Event.(domain.Message)
		if !ok {
			continue
		}

		messages = append(messages, messageShape{Body: message.Body, Action: message.Action})
	}

	return messages
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

func runTool(t *testing.T, tool ToolCommand, tc modelclient.ToolContext) modelclient.ToolResultPayload {
	t.Helper()

	outcome := tool.RunTool(t.Context(), tc)
	require.NoError(t, outcome.ExecutionError)

	return outcome.Payload
}

func executeTool(t *testing.T, name string, rawJSON string, tc modelclient.ToolContext) modelclient.ToolResultPayload {
	t.Helper()

	registry, err := BuildToolRegistry()
	require.NoError(t, err)

	spec, ok := registry.Find(name)
	require.True(t, ok)

	result, err := spec.Execute(t.Context(), tc, json.RawMessage(rawJSON))
	require.NoError(t, err)

	return result
}

type toolTestClient struct {
	send func(context.Context, protocol.Command) (protocol.Response, error)
}

func (c toolTestClient) Identity() protocol.ClientID { return protocol.UserClientID }

func (c toolTestClient) Send(ctx context.Context, cmd protocol.Command) (protocol.Response, error) {
	return c.send(ctx, cmd)
}

func (toolTestClient) Events() <-chan protocol.Delivery { return nil }

func (toolTestClient) Caps() command.CapabilityHolder { return command.NoCapabilities() }

func TestRunTool_join_with_channel(t *testing.T) {
	sess, user := newToolTestSession(t)
	tc := userToolContext(sess, user, protocol.ChannelTarget("#general"))

	v := toolValue(t, "join", `{"channel": "#testing", "key": null}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "JoinCommand should implement ToolCommand")

	result := runTool(t, tool, tc)

	require.Equal(t, modelclient.ToolResultPayload{
		OK:      true,
		Summary: "joined #testing",
	}, result)
}

func TestRunTool_session_failure_is_an_execution_error(t *testing.T) {
	sentinel := errors.New("session unavailable")
	client := toolTestClient{send: func(context.Context, protocol.Command) (protocol.Response, error) {
		return protocol.Response{}, sentinel
	}}
	registry, err := BuildToolRegistry()
	require.NoError(t, err)
	spec, ok := registry.Find("list")
	require.True(t, ok)

	payload, err := spec.Execute(t.Context(), modelclient.ToolContext{Client: client}, json.RawMessage(`{}`))

	require.Equal(t, modelclient.ToolResultPayload{}, payload)
	require.ErrorIs(t, err, sentinel)

	var executionError *modelclient.ToolExecutionError
	require.ErrorAs(t, err, &executionError)
	require.Equal(t, &modelclient.ToolExecutionError{Tool: "list", Err: sentinel}, executionError)
}

// TestRunTool_join_multi_target_partial_success covers a
// multi-target JOIN where one channel joins and one is refused: the
// call reports success, and the summary names both outcomes.
func TestRunTool_join_multi_target_partial_success(t *testing.T) {
	s := storetest.NewMemoryStore(t)
	sess, _, user := uitest.NewTestSession(t, s, toolTestAPI{}, nil, nil, "", "", t.Context)
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

	locked := domain.NewChannelWindow("#locked", time.Now())
	locked.Modes = domain.ChannelModes{InviteOnly: true}
	require.NoError(t, s.SaveWindow(t.Context(), locked))

	tc := userToolContext(sess, user, protocol.ChannelTarget("#general"))

	v := toolValue(t, "join", `{"channel": "#open,#locked", "key": null}`)
	tool, ok := v.(ToolCommand)
	require.True(t, ok, "JoinCommand should implement ToolCommand")

	result := runTool(t, tool, tc)

	require.Equal(t, modelclient.ToolResultPayload{
		OK:      true,
		Summary: "joined #open; cannot join #locked: invite-only channel",
	}, result)
}

// TestJoinCommand_Run_multi_target_partial_success_shows_a_notice
// covers the chat-screen path for the same partial-success shape.
// The channel that joined shows up through the ordinary JOIN
// broadcast, so Run's own return value only needs to report the
// refusal: a system notice naming both outcomes.
func TestJoinCommand_Run_multi_target_partial_success_shows_a_notice(t *testing.T) {
	s := storetest.NewMemoryStore(t)
	sess, _, user := uitest.NewTestSession(t, s, toolTestAPI{}, nil, nil, "", "", t.Context)
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

	locked := domain.NewChannelWindow("#locked", time.Now())
	locked.Modes = domain.ChannelModes{InviteOnly: true}
	require.NoError(t, s.SaveWindow(t.Context(), locked))

	join := JoinCommand{Channel: "#open,#locked"}
	rc := Context{Client: user, Active: domain.WindowKey("#general")}

	msg := join.Run(t.Context(), rc)()

	events, ok := msg.(ReplyEvents)
	require.True(t, ok, "expected ReplyEvents, got %T", msg)

	type noticeResult struct {
		Target domain.ChannelName
		Text   string
		AtSet  bool
	}
	got := make([]noticeResult, 0, len(events))
	for _, event := range events {
		notice, ok := event.(domain.SystemNotice)
		require.True(t, ok, "expected a SystemNotice, got %T", event)
		got = append(got, noticeResult{
			Target: notice.Target,
			Text:   notice.Text,
			AtSet:  !notice.At.IsZero(),
		})
	}

	require.Equal(t, []noticeResult{{
		Target: "#general",
		Text:   "joined #open; cannot join #locked: invite-only channel",
		AtSet:  true,
	}}, got)
}

// TestJoinCommand_Run_focuses_the_channel_the_server_joined covers
// the spelling `/join` moves focus to. Channel names compare
// case-insensitively, so `/join #DEV` against an existing `#dev`
// joins `#dev`; focusing the typed spelling would look up a window
// that does not exist and leave the user where they were, with the
// channel joined behind their back.
func TestJoinCommand_Run_focuses_the_channel_the_server_joined(t *testing.T) {
	tests := []struct {
		name  string
		typed ChannelArg
		want  domain.ChannelName
	}{
		{name: "same spelling", typed: "#dev", want: "#dev"},
		{name: "another case", typed: "#DEV", want: "#dev"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := storetest.NewMemoryStore(t)
			sess, _, user := uitest.NewTestSession(t, s, toolTestAPI{}, nil, nil, "", "", t.Context)
			t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

			require.NoError(t, s.SaveWindow(t.Context(), domain.NewChannelWindow("#dev", time.Now())))

			join := JoinCommand{Channel: tt.typed}
			msg := join.Run(t.Context(), Context{Client: user, Active: domain.WindowKey("#general")})()

			focus, ok := msg.(ChannelFocusMsg)
			require.True(t, ok, "expected ChannelFocusMsg, got %T", msg)
			require.Equal(t, tt.want, focus.Channel)
		})
	}
}

func TestRunTool_help_no_args(t *testing.T) {
	sess, user := newToolTestSession(t)
	tc := userToolContext(sess, user, protocol.ChannelTarget("#general"))

	v := toolValue(t, "help", `{}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "HelpCommand should implement ToolCommand")

	result := runTool(t, tool, tc)

	require.Equal(t, modelclient.ToolResultPayload{
		OK:      true,
		Summary: "available command tools include join, part, list, invite, kick, msg, nick, topic, me, whois, help, and quit",
	}, result)
}

func TestRunTool_part_no_channel_returns_error(t *testing.T) {
	sess, user := newToolTestSession(t)
	tc := userToolContext(sess, user, nil)

	v := toolValue(t, "part", `{"message": null}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "PartCommand should implement ToolCommand")

	result := runTool(t, tool, tc)

	require.Equal(t, modelclient.ToolResultPayload{
		OK:    false,
		Error: "no active channel",
	}, result)
}

func TestRunTool_kick_no_channel_returns_error(t *testing.T) {
	sess, user := newToolTestSession(t)
	tc := userToolContext(sess, user, nil)

	v := toolValue(t, "kick", `{"nick": "haiku"}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "KickCommand should implement ToolCommand")

	result := runTool(t, tool, tc)

	require.Equal(t, modelclient.ToolResultPayload{
		OK:    false,
		Error: "no active channel",
	}, result)
}

func TestRunTool_invite_missing_nick_returns_error(t *testing.T) {
	sess, user := newToolTestSession(t)
	tc := userToolContext(sess, user, protocol.ChannelTarget("#general"))

	v := toolValue(t, "invite", `{"nick": null, "channel": null}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "InviteCommand should implement ToolCommand")

	result := runTool(t, tool, tc)

	require.Equal(t, modelclient.ToolResultPayload{
		OK:    false,
		Error: "target nick is required",
	}, result)
}

func TestRunTool_invite_unknown_nick_reports_failure(t *testing.T) {
	sess, user := newToolTestSession(t)

	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#general")))
	tc := userToolContext(sess, user, protocol.ChannelTarget("#general"))

	v := toolValue(t, "invite", `{"nick": "nobody", "channel": null}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "InviteCommand should implement ToolCommand")

	require.Equal(t, modelclient.ToolResultPayload{
		OK:    false,
		Error: "no such nick: nobody",
	}, runTool(t, tool, tc))
}

func TestTopicCommand_tool_distinguishes_query_from_clear(t *testing.T) {
	query := toolValue(t, "topic", `{"topic": null}`)
	clearValue := toolValue(t, "topic", `{"topic": ""}`)
	empty := CommandText("")
	require.Equal(t, struct {
		Query any
		Clear any
	}{
		Query: TopicCommand{},
		Clear: TopicCommand{Topic: &empty},
	}, struct {
		Query any
		Clear any
	}{
		Query: query,
		Clear: clearValue,
	})

	var sent protocol.Command
	client := toolTestClient{send: func(_ context.Context, cmd protocol.Command) (protocol.Response, error) {
		sent = cmd
		return protocol.Response{}, nil
	}}
	clearCommand, ok := clearValue.(TopicCommand)
	if !ok {
		t.Fatalf("decoded clear command has type %T", clearValue)
	}

	outcome := clearCommand.RunTool(t.Context(), modelclient.ToolContext{
		Client: client,
		Target: protocol.ChannelTarget("#lobby"),
	})
	require.Equal(t, struct {
		Outcome modelclient.ToolOutcome
		Command protocol.Command
	}{
		Outcome: modelclient.ToolOutcome{Payload: modelclient.ToolResultPayload{
			OK:      true,
			Summary: "updated topic for #lobby",
		}},
		Command: protocol.Topic{Channel: "#lobby", Body: ""},
	}, struct {
		Outcome modelclient.ToolOutcome
		Command protocol.Command
	}{
		Outcome: outcome,
		Command: sent,
	})
}

func TestRunTool_msg_sends_to_nick(t *testing.T) {
	sess, user := newToolTestSession(t)

	// Join a channel and add a model so the nick resolves.
	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#lobby")))
	uitest.AddModel(t, user, "#lobby", "anthropic/haiku", "")

	tc := userToolContext(sess, user, nil)

	v := toolValue(t, "msg", `{"target": "testbot", "content": {"body": ["hello"]}}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "MsgCommand should implement ToolCommand")

	result := runTool(t, tool, tc)

	require.True(t, result.OK)
	require.Equal(t, "messaged testbot", result.Summary)
}

func TestRunTool_msg_pins_a_nick_batch_to_the_first_recipient(t *testing.T) {
	var targets []protocol.MsgTarget
	client := toolTestClient{send: func(_ context.Context, cmd protocol.Command) (protocol.Response, error) {
		message, ok := cmd.(protocol.PrivMsg)
		require.True(t, ok)
		targets = append(targets, message.Target)

		return protocol.Response{Events: []protocol.Event{
			domain.Message{Target: "original-instance"},
		}}, nil
	}}

	outcome := (MsgCommand{
		Target: "shared-nick",
		Body:   MessageBodies{"first", "second"},
	}).RunTool(t.Context(), modelclient.ToolContext{Client: client})
	require.NoError(t, outcome.ExecutionError)
	require.Equal(t, modelclient.ToolResultPayload{
		OK:      true,
		Summary: "sent 2 messages to shared-nick",
	}, outcome.Payload)
	require.Equal(t, []protocol.MsgTarget{
		protocol.NickTarget("shared-nick"),
		protocol.ClientTarget("original-instance"),
	}, targets)
}

func TestRunTool_msg_does_not_follow_a_renamed_and_reclaimed_nick(t *testing.T) {
	eventStore := storetest.NewMemoryStore(t)
	sess, _, user := uitest.NewTestSession(t, eventStore, toolTestAPI{}, nil, nil, "", "", t.Context)
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

	original := testclient.New("shared", sess, testclient.WithInstanceID("original-instance"))
	require.NoError(t, original.Attach(t.Context()))
	t.Cleanup(original.Detach)

	var replacement *testclient.TestClient
	paceCalls := 0
	tc := userToolContext(sess, user, nil)
	tc.Pace = func(ctx context.Context, _ string) error {
		paceCalls++
		if paceCalls != 2 {
			return nil
		}

		resp, err := original.Send(ctx, protocol.Nick{New: "renamed"})
		require.NoError(t, err)
		require.NoError(t, resp.Err)

		replacement = testclient.New("shared", sess, testclient.WithInstanceID("replacement-instance"))
		require.NoError(t, replacement.Attach(ctx))

		return nil
	}

	outcome := (MsgCommand{
		Target: "shared",
		Body:   MessageBodies{"first", "second"},
	}).RunTool(t.Context(), tc)
	require.NoError(t, outcome.ExecutionError)
	require.Equal(t, modelclient.ToolResultPayload{
		OK:      true,
		Summary: "sent 2 messages to shared",
	}, outcome.Payload)
	require.NotNil(t, replacement)
	t.Cleanup(replacement.Detach)

	originalEvents, err := eventStore.DMEventsBefore(t.Context(), user.Instance().ID(), original.Instance().ID(), nil, 100)
	require.NoError(t, err)
	require.Equal(t, []messageShape{{Body: "first"}, {Body: "second"}}, storedMessageShapesFromEvents(originalEvents))

	replacementEvents, err := eventStore.DMEventsBefore(t.Context(), user.Instance().ID(), replacement.Instance().ID(), nil, 100)
	require.NoError(t, err)
	require.Equal(t, []domain.StoredEvent(nil), replacementEvents)
}

func TestRunTool_msg_counts_a_missing_confirmation_as_sent(t *testing.T) {
	client := toolTestClient{send: func(_ context.Context, _ protocol.Command) (protocol.Response, error) {
		return protocol.Response{}, nil
	}}

	outcome := (MsgCommand{
		Target: "shared-nick",
		Body:   MessageBodies{"first", "second"},
	}).RunTool(t.Context(), modelclient.ToolContext{Client: client})

	var batchError *ToolBatchError
	require.ErrorAs(t, outcome.ExecutionError, &batchError)

	var responseError *MissingMessageResponseError
	require.ErrorAs(t, outcome.ExecutionError, &responseError)
	require.Equal(t, &ToolBatchError{
		Kind:  protocol.ReplyMessage,
		Sent:  1,
		Total: 2,
		Err:   responseError,
	}, batchError)
	require.Equal(t, &MissingMessageResponseError{Tool: "msg"}, responseError)
}

func TestMsgTool_pacing_failure_is_a_typed_execution_error(t *testing.T) {
	sentinel := errors.New("pacing stopped")
	sends := 0
	client := toolTestClient{send: func(_ context.Context, _ protocol.Command) (protocol.Response, error) {
		sends++
		return protocol.Response{Events: []protocol.Event{domain.Message{Target: "#lobby"}}}, nil
	}}
	tc := modelclient.ToolContext{
		Client: client,
		Pace: func(_ context.Context, _ string) error {
			if sends == 1 {
				return sentinel
			}

			return nil
		},
	}

	registry, err := BuildToolRegistry()
	require.NoError(t, err)
	spec, ok := registry.Find("msg")
	require.True(t, ok)

	_, err = spec.Execute(t.Context(), tc, json.RawMessage(
		`{"target":"#lobby","content":{"body":["first","second"]}}`,
	))

	var executionError *modelclient.ToolExecutionError
	require.ErrorAs(t, err, &executionError)
	require.Equal(t, "msg", executionError.Tool)
	require.ErrorIs(t, err, sentinel)

	var batchError *ToolBatchError
	require.ErrorAs(t, err, &batchError)
	require.Equal(t, 1, batchError.Sent)
	require.Equal(t, 2, batchError.Total)
	require.Equal(t, 1, sends)
}

func TestRunTool_sends_each_plain_array_element_separately(t *testing.T) {
	tests := []struct {
		name        string
		toolName    string
		rawJSON     string
		wantSummary string
		want        []messageShape
	}{
		{
			name:        "messages",
			toolName:    "msg",
			rawJSON:     `{"target": "#lobby", "content": {"body": ["a", "b", "c"]}}`,
			wantSummary: "sent 3 messages to #lobby",
			want: []messageShape{
				{Body: "a"},
				{Body: "b"},
				{Body: "c"},
			},
		},
		{
			name:        "actions",
			toolName:    "me",
			rawJSON:     `{"content": {"action": ["waves", "sits down"]}}`,
			wantSummary: "sent 2 actions to #lobby",
			want: []messageShape{
				{Body: "waves", Action: true},
				{Body: "sits down", Action: true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eventStore := storetest.NewMemoryStore(t)
			sess, _, user := uitest.NewTestSession(t, eventStore, toolTestAPI{}, nil, nil, "", "", t.Context)
			t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })
			require.NoError(t, user.Join(t.Context(), "#lobby"))

			var paced []string
			tc := userToolContext(sess, user, protocol.ChannelTarget("#lobby"))
			tc.Pace = func(_ context.Context, body string) error {
				paced = append(paced, body)
				return nil
			}

			value := toolValue(t, tt.toolName, tt.rawJSON)
			tool, ok := value.(ToolCommand)
			require.True(t, ok)

			require.Equal(t, modelclient.ToolResultPayload{OK: true, Summary: tt.wantSummary}, runTool(t, tool, tc))
			require.Equal(t, tt.want, storedMessageShapes(t, eventStore, "#lobby"))

			wantPacing := make([]string, len(tt.want))
			for index, message := range tt.want {
				wantPacing[index] = message.Body
			}
			require.Equal(t, wantPacing, paced)
		})
	}
}

func TestRunTool_preflights_every_plain_array_element_before_sending(t *testing.T) {
	eventStore := storetest.NewMemoryStore(t)
	sess, _, user := uitest.NewTestSession(t, eventStore, toolTestAPI{}, nil, nil, "", "", t.Context)
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })
	require.NoError(t, user.Join(t.Context(), "#lobby"))

	var paced []string
	tc := userToolContext(sess, user, protocol.ChannelTarget("#lobby"))
	tc.Pace = func(_ context.Context, body string) error {
		paced = append(paced, body)
		return nil
	}

	result := executeTool(t, "msg", `{"target":"#lobby","content":{"body":["valid","line one\nline two"]}}`, tc)
	require.False(t, result.OK)
	require.Equal(t, []string(nil), paced)
	require.Equal(t, []messageShape(nil), storedMessageShapes(t, eventStore, "#lobby"))
}

// TestRunTool_msg_unknown_nick_reports_failure pins that a target
// naming nobody comes back as a failure the model can act on, so a
// mistyped nick is something the model can correct.
func TestRunTool_msg_unknown_nick_reports_failure(t *testing.T) {
	sess, user := newToolTestSession(t)

	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#lobby")))
	uitest.AddModel(t, user, "#lobby", "anthropic/haiku", "")

	tc := userToolContext(sess, user, nil)

	v := toolValue(t, "msg", `{"target": "testbto", "content": {"body": ["hello"]}}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "MsgCommand should implement ToolCommand")

	require.Equal(t, modelclient.ToolResultPayload{
		OK:    false,
		Error: "no such nick: testbto",
	}, runTool(t, tool, tc))
}

func TestRunTool_msg_rejects_empty_body(t *testing.T) {
	sess, user := newToolTestSession(t)

	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#lobby")))
	uitest.AddModel(t, user, "#lobby", "anthropic/haiku", "")

	tc := userToolContext(sess, user, nil)

	result := executeTool(t, "msg", `{"target":"testbot"}`, tc)
	require.False(t, result.OK)
}

func TestRenderReplyMessages_returns_typed_validation_errors(t *testing.T) {
	tests := []struct {
		name    string
		bodies  MessageBodies
		spans   []protocol.ReplySpan
		wantErr error
	}{
		{
			name:    "neither representation",
			wantErr: protocol.ReplyPartShapeError{},
		},
		{
			name:    "both representations",
			bodies:  MessageBodies{"hello"},
			spans:   []protocol.ReplySpan{{Text: "there"}},
			wantErr: protocol.ReplyPartShapeError{HasBody: true, HasSpans: true},
		},
		{
			name:    "empty array element",
			bodies:  MessageBodies{"hello", ""},
			wantErr: domain.NoTextToSendError{Command: "PRIVMSG"},
		},
		{
			name:    "framing byte in later array element",
			bodies:  MessageBodies{"hello", "line one\nline two"},
			wantErr: domain.InvalidMessageBodyError{Command: "PRIVMSG"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := renderReplyMessages(protocol.ReplyMessage, tt.bodies, tt.spans)
			require.Equal(t, tt.wantErr, err)
		})
	}
}

func TestRunTool_whois_stamps_issuing_window(t *testing.T) {
	sess, user := newToolTestSession(t)

	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#lobby")))
	uitest.AddModel(t, user, "#lobby", "anthropic/haiku", "")

	tc := userToolContext(sess, user, protocol.ChannelTarget("#lobby"))

	v := toolValue(t, "whois", `{"nick": "testbot"}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "WhoisCommand should implement ToolCommand")

	result := runTool(t, tool, tc)

	require.True(t, result.OK)
	whois, ok := result.Data.(domain.Whois)
	require.True(t, ok, "whois tool returns a domain.Whois snapshot")
	require.Equal(t, domain.ChannelName("#lobby"), whois.Target)
}

func TestRunTool_nick_changes_nick(t *testing.T) {
	sess, user := newToolTestSession(t)
	tc := userToolContext(sess, user, protocol.ChannelTarget("#general"))

	v := toolValue(t, "nick", `{"new_nick": "newname"}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "NickCommand should implement ToolCommand")

	result := runTool(t, tool, tc)

	require.Equal(t, modelclient.ToolResultPayload{
		OK:      true,
		Summary: "changed nick to newname",
	}, result)
}

// TestRunTool_me_without_a_window_returns_error covers the one thing
// `/me` asks of its caller: that there be a window to act in. A nil
// target is the only value that says there is none, which is what
// keeps it apart from the DM with the user, a window whose key is the
// empty `InstanceID`.
func TestRunTool_me_without_a_window_returns_error(t *testing.T) {
	sess, user := newToolTestSession(t)
	tc := userToolContext(sess, user, nil)

	v := toolValue(t, "me", `{"content": {"action": ["waves"]}}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "MeCommand should implement ToolCommand")

	require.Equal(t, modelclient.ToolResultPayload{
		OK:    false,
		Error: "no active window",
	}, runTool(t, tool, tc))
}

func TestMeCommand_Run_preserves_a_typed_body_error(t *testing.T) {
	msg := (MeCommand{Action: ActionBodies{"line one\nline two"}}).Run(t.Context(), Context{
		Active: domain.WindowKey("#lobby"),
	})()

	event, ok := msg.(domain.ErrorEvent)
	require.True(t, ok)
	require.Equal(t, "me", event.Operation)
	require.Equal(t, domain.ChannelName("#lobby"), event.Target)

	var bodyError domain.InvalidMessageBodyError
	require.ErrorAs(t, event.Err, &bodyError)
	require.Equal(t, "ACTION", bodyError.Command)
}

// TestRunTool_me_in_a_dm_addresses_the_counterpart covers `/me`
// inside a DM. The window a turn runs in is the conversation with the
// counterpart, so that is where the action goes.
func TestRunTool_me_in_a_dm_addresses_the_counterpart(t *testing.T) {
	sess, user := newToolTestSession(t)

	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#lobby")))
	uitest.AddModel(t, user, "#lobby", "anthropic/haiku", "")

	bot, err := sess.ResolveNick(t.Context(), "testbot")
	require.NoError(t, err)

	tc := userToolContext(sess, user, protocol.ClientTarget(bot.ID()))

	v := toolValue(t, "me", `{"content": {"action": ["waves"]}}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "MeCommand should implement ToolCommand")

	require.Equal(t, modelclient.ToolResultPayload{
		OK:      true,
		Summary: "sent action to " + string(bot.ID()),
	}, runTool(t, tool, tc))
}

func TestRunTool_me_sends_action_to_channel(t *testing.T) {
	sess, user := newToolTestSession(t)
	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#lobby")))
	tc := userToolContext(sess, user, protocol.ChannelTarget("#lobby"))

	v := toolValue(t, "me", `{"content": {"action": ["waves"]}}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "MeCommand should implement ToolCommand")

	require.Equal(t, modelclient.ToolResultPayload{
		OK:      true,
		Summary: "sent action to #lobby",
	}, runTool(t, tool, tc))
}

func TestRunTool_msg_with_spans_renders_irc_formatting(t *testing.T) {
	sess, user := newToolTestSession(t)
	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#lobby")))
	tc := userToolContext(sess, user, protocol.ChannelTarget("#lobby"))
	var paced []string
	tc.Pace = func(_ context.Context, body string) error {
		paced = append(paced, body)
		return nil
	}

	v := toolValue(t, "msg",
		`{"target":"#lobby","content":{"spans":[{"text":"hello ","style":null},{"text":"world","style":{"bold":true,"italic":false,"underline":false,"reverse":false,"strike":false,"fg":4,"bg":null}}]}}`,
	)

	tool, ok := v.(ToolCommand)
	require.True(t, ok)

	require.Equal(t, modelclient.ToolResultPayload{
		OK:      true,
		Summary: "messaged #lobby",
	}, runTool(t, tool, tc))
	require.Equal(t, []string{"hello world"}, paced)
}

func TestToolValue_reply_colours_accept_JSON_integer_spellings(t *testing.T) {
	colour := protocol.ReplyPaletteIndex(4)
	spans := protocol.ReplySpans{{Text: "hello", Style: &protocol.ReplyStyle{FG: &colour}}}

	tests := []struct {
		name string
		tool string
		raw  string
		want any
	}{
		{
			name: "msg decimal integer",
			tool: "msg",
			raw:  `{"target":"#lobby","content":{"spans":[{"text":"hello","style":{"bold":false,"italic":false,"underline":false,"reverse":false,"strike":false,"fg":4.0,"bg":null}}]}}`,
			want: MsgCommand{Target: "#lobby", Spans: spans},
		},
		{
			name: "msg exponent integer",
			tool: "msg",
			raw:  `{"target":"#lobby","content":{"spans":[{"text":"hello","style":{"bold":false,"italic":false,"underline":false,"reverse":false,"strike":false,"fg":4e0,"bg":null}}]}}`,
			want: MsgCommand{Target: "#lobby", Spans: spans},
		},
		{
			name: "me decimal integer",
			tool: "me",
			raw:  `{"content":{"spans":[{"text":"hello","style":{"bold":false,"italic":false,"underline":false,"reverse":false,"strike":false,"fg":4.0,"bg":null}}]}}`,
			want: MeCommand{Spans: spans},
		},
		{
			name: "me exponent integer",
			tool: "me",
			raw:  `{"content":{"spans":[{"text":"hello","style":{"bold":false,"italic":false,"underline":false,"reverse":false,"strike":false,"fg":4e0,"bg":null}}]}}`,
			want: MeCommand{Spans: spans},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, toolValue(t, tt.tool, tt.raw))
		})
	}
}

func TestRunTool_quit_succeeds(t *testing.T) {
	sess, user := newToolTestSession(t)
	tc := userToolContext(sess, user, nil)

	v := toolValue(t, "quit", `{"message": null}`)

	tool, ok := v.(ToolCommand)
	require.True(t, ok, "QuitCommand should implement ToolCommand")

	result := runTool(t, tool, tc)

	require.Equal(t, modelclient.ToolResultPayload{
		OK:      true,
		Summary: "shut down and left all channels",
	}, result)
}
