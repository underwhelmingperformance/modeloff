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
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/ui/chatcmd"
)

type stubAPI struct{}

func (stubAPI) ListModels(context.Context) ([]api.ModelInfo, error) { return nil, nil }

func (stubAPI) SendEvents(
	context.Context,
	domain.ModelID,
	string,
	[]protocol.IRCMessage,
	[]protocol.IRCMessage,
) (protocol.ModelResponse, error) {
	return protocol.ModelResponse{Kind: protocol.ResponseSilence}, nil
}

func (stubAPI) GenerateNick(context.Context, domain.ModelID, domain.ModelID) (domain.Nick, error) {
	return "testbot", nil
}

func newTestSession(t *testing.T) *session.Session {
	t.Helper()

	return session.New(storemod.NewFileStore(t.TempDir()), nil, stubAPI{}, nil, "testuser")
}

func TestChatScreen_Commands_specs_are_complete(t *testing.T) {
	screen := NewChatScreen(t.Context(), newTestSession(t))
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
	screen := NewChatScreen(t.Context(), newTestSession(t))

	cmds := screen.Commands()
	names := make([]string, 0, len(cmds.Commands))
	for _, spec := range cmds.Commands {
		names = append(names, spec.Name)
	}

	require.Equal(t, []string{
		"join",
		"part",
		"list",
		"invite",
		"kick",
		"msg",
		"nick",
		"topic",
		"me",
		"whois",
		"config",
		"help",
		"quit",
	}, names)
}

func TestChatScreen_HelpCommand_emits_typed_event(t *testing.T) {
	screen := NewChatScreen(t.Context(), newTestSession(t))

	cmd, err := screen.buildParser().Parse("/help")
	require.NoError(t, err)

	msg := cmd.Run(screen.runContext())()
	require.Equal(t, chatcmd.HelpResult{}, msg)
}

func TestChatScreen_QuitCommand_returns_quit(t *testing.T) {
	screen := NewChatScreen(t.Context(), newTestSession(t))

	cmd, err := screen.buildParser().Parse("/quit")
	require.NoError(t, err)

	msg := cmd.Run(screen.runContext())()
	require.Equal(t, tea.QuitMsg{}, msg)
}
