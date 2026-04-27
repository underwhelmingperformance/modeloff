package screens

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/store/storetest"
	uipkg "github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/chatcmd"
)

type stubAPI struct{}

func (stubAPI) ListModels(context.Context) ([]api.ModelInfo, error) { return nil, nil }

func (stubAPI) SendEvents(
	context.Context,
	domain.ModelID,
	domain.InstanceID,
	string,
	[]protocol.IRCMessage,
	[]protocol.IRCMessage,
	...api.ToolDefinition,
) (api.CompletionResult, error) {
	return api.CompletionResult{
		Response: protocol.ModelResponse{Kind: protocol.ResponseSilence},
	}, nil
}

func (stubAPI) ContinueWithToolResults(
	context.Context,
	*api.Conversation,
	[]api.ToolResult,
	...api.ToolDefinition,
) (api.CompletionResult, error) {
	return api.CompletionResult{
		Response: protocol.ModelResponse{Kind: protocol.ResponseSilence},
	}, nil
}

func (stubAPI) GenerateNick(context.Context, domain.ModelID, string, []domain.Nick) (api.NicknameResult, error) {
	return api.NicknameResult{Nick: "testbot"}, nil
}

func (stubAPI) GeneratePersonas(context.Context, domain.ModelID) ([]domain.Persona, error) {
	return nil, nil
}

func newTestSession(t *testing.T) *session.Session {
	t.Helper()

	s := storetest.NewMemoryStore(t)
	return session.New(s, nil, stubAPI{}, "testuser", "", "")
}

func TestChatScreen_Commands_specs_are_complete(t *testing.T) {
	screen, err := NewChatScreen(t.Context(), newTestSession(t), nil, domain.KindStatus)
	require.NoError(t, err)

	commands := screen.parser.Set().Commands

	type spec struct {
		Name string
		Help string
	}

	var specs []spec
	for _, node := range commands {
		specs = append(specs, spec{Name: node.Name, Help: node.Help})
	}

	require.Equal(t, []spec{
		{Name: "join", Help: "Switch to a channel or create it if needed."},
		{Name: "part", Help: "Part from the current channel."},
		{Name: "list", Help: "List all known channels."},
		{Name: "add-model", Help: "Add a model or reusable instance into the current channel."},
		{Name: "invite", Help: "Invite a nick to a channel."},
		{Name: "kick", Help: "Remove a nick from the current channel."},
		{Name: "msg", Help: "Send a message to a #channel or to a user by nick."},
		{Name: "query", Help: "Open (or focus) a direct-message window with a nick. Optional trailing body is sent as the first message."},
		{Name: "nick", Help: "Change your nickname."},
		{Name: "topic", Help: "Set or clear the current channel topic."},
		{Name: "me", Help: "Send an action message (e.g. /me waves)."},
		{Name: "whois", Help: "Show details about a model instance."},
		{Name: "config", Help: "Update runtime configuration."},
		{Name: "personas", Help: "List all defined personas."},
		{Name: "regenerate-personas", Help: "Regenerate AI-created personas."},
		{Name: "help", Help: "Show available commands."},
		{Name: "clear", Help: "Clear the current window."},
		{Name: "quit", Help: "Exit modeloff."},
	}, specs)
}

func TestChatScreen_Commands_exposes_chat_commands(t *testing.T) {
	screen, err := NewChatScreen(t.Context(), newTestSession(t), nil, domain.KindStatus)
	require.NoError(t, err)

	commands := screen.parser.Set().Commands
	names := make([]string, 0, len(commands))
	for _, spec := range commands {
		names = append(names, spec.Name)
	}

	require.Equal(t, []string{
		"join",
		"part",
		"list",
		"add-model",
		"invite",
		"kick",
		"msg",
		"query",
		"nick",
		"topic",
		"me",
		"whois",
		"config",
		"personas",
		"regenerate-personas",
		"help",
		"clear",
		"quit",
	}, names)
}

func TestChatScreen_HelpCommand_emits_typed_event(t *testing.T) {
	screen, err := NewChatScreen(t.Context(), newTestSession(t), nil, domain.KindStatus)
	require.NoError(t, err)

	cmd, err := screen.parser.Parse("/help")
	require.NoError(t, err)

	msg := cmd.Run(screen.runContext())()
	require.Equal(t, chatcmd.HelpResult{}, msg)
}

func TestChatScreen_QuitCommand_returns_quit_requested(t *testing.T) {
	screen, err := NewChatScreen(t.Context(), newTestSession(t), nil, domain.KindStatus)
	require.NoError(t, err)

	cmd, err := screen.parser.Parse("/quit goodnight")
	require.NoError(t, err)

	msg := cmd.Run(screen.runContext())()
	require.Equal(t, uipkg.QuitRequestedMsg{Message: "goodnight"}, msg)
}

func TestChatScreen_StatusItems_omits_disconnecting_by_default(t *testing.T) {
	screen, err := NewChatScreen(t.Context(), newTestSession(t), nil, domain.KindStatus)
	require.NoError(t, err)

	for _, item := range screen.StatusItems() {
		require.NotEqual(t, "disconnecting", item.ID,
			"disconnecting status item should only appear once a quit is in flight")
	}
}

func TestChatScreen_StatusItems_surfaces_disconnecting_while_quit_in_flight(t *testing.T) {
	screen, err := NewChatScreen(t.Context(), newTestSession(t), nil, domain.KindStatus)
	require.NoError(t, err)

	updated, _ := screen.Update(uipkg.QuitRequestedMsg{})
	chat, ok := updated.(ChatScreen)
	require.True(t, ok, "expected ChatScreen, got %T", updated)

	require.Contains(t, chat.StatusItems(), uipkg.StatusItem{
		ID:       "disconnecting",
		Side:     uipkg.StatusSideRight,
		Priority: 100,
		Full:     "Disconnecting…",
		Compact:  "off…",
	}, "quit-in-flight must surface a Disconnecting… status item")
}

func TestChatScreen_second_quit_request_escalates_to_tea_quit(t *testing.T) {
	screen, err := NewChatScreen(t.Context(), newTestSession(t), nil, domain.KindStatus)
	require.NoError(t, err)

	// First quit starts the disconnect flow.
	updated, _ := screen.Update(uipkg.QuitRequestedMsg{})
	chat, ok := updated.(ChatScreen)
	require.True(t, ok)
	require.True(t, chat.quitting)

	// A second quit request while the first is in flight must return
	// tea.Quit directly, so the user is never stuck waiting on
	// Session.Quit.
	updated, cmd := chat.Update(uipkg.QuitRequestedMsg{})
	require.NotNil(t, cmd)

	second, ok := updated.(ChatScreen)
	require.True(t, ok)
	require.True(t, second.quitting,
		"quitting flag should remain set after the escalation")

	require.Equal(t, tea.Quit(), cmd(),
		"second QuitRequestedMsg should escalate to tea.Quit")
}
