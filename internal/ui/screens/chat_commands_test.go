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
	"github.com/laney/modeloff/internal/ui/chatcmd"
)

type stubAPI struct{}

func (stubAPI) ListModels(context.Context) ([]api.ModelInfo, error) { return nil, nil }

func (stubAPI) SendEvents(
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

func (stubAPI) GenerateNick(context.Context, domain.ModelID, domain.ModelID) (api.NicknameResult, error) {
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
	screen := NewChatScreen(t.Context(), newTestSession(t), nil)
	cmds := screen.Commands()

	require.NotEmpty(t, cmds.Commands)

	seen := make(map[string]struct{}, len(cmds.Commands))
	for _, spec := range cmds.Commands {
		require.NotEmpty(t, spec.Name)
		require.NotEmpty(t, spec.Help)
		require.NotEmpty(t, spec.Usage())

		_, exists := seen[spec.Name]
		require.Falsef(t, exists, "duplicate command %q", spec.Name)
		seen[spec.Name] = struct{}{}
	}
}

func TestChatScreen_Commands_exposes_chat_commands(t *testing.T) {
	screen := NewChatScreen(t.Context(), newTestSession(t), nil)

	cmds := screen.Commands()
	names := make([]string, 0, len(cmds.Commands))
	for _, spec := range cmds.Commands {
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
	screen := NewChatScreen(t.Context(), newTestSession(t), nil)

	cmd, err := screen.buildParser().Parse("/help")
	require.NoError(t, err)

	msg := cmd.Run(screen.runContext())()
	require.Equal(t, chatcmd.HelpResult{}, msg)
}

func TestChatScreen_QuitCommand_returns_quit(t *testing.T) {
	screen := NewChatScreen(t.Context(), newTestSession(t), nil)

	cmd, err := screen.buildParser().Parse("/quit")
	require.NoError(t, err)

	msg := cmd.Run(screen.runContext())()
	require.Equal(t, tea.QuitMsg{}, msg)
}
