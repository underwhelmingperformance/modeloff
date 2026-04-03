package screens

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/ui/components"
)

type scopeTestAPI struct{}

func (scopeTestAPI) ListModels(context.Context) ([]api.ModelInfo, error) { return nil, nil }

func (scopeTestAPI) SendEvents(
	context.Context,
	domain.ModelID,
	string,
	[]protocol.IRCMessage,
	[]protocol.IRCMessage,
) (protocol.ModelResponse, error) {
	return protocol.ModelResponse{Kind: protocol.ResponseSilence}, nil
}

func (scopeTestAPI) GenerateNick(context.Context, domain.ModelID) (domain.Nick, error) {
	return "scopebot", nil
}

func newScopeTestSession(t *testing.T) *session.Session {
	t.Helper()

	return session.New(storemod.NewFileStore(t.TempDir()), nil, scopeTestAPI{}, nil, "testuser")
}

func TestChatScreen_CommandScope_exposes_chat_commands(t *testing.T) {
	screen := NewChatScreen(t.Context(), newScopeTestSession(t))

	scope := screen.CommandScope()
	names := make([]string, 0, len(scope.Commands))
	for _, spec := range scope.Commands {
		names = append(names, spec.Name)
	}

	require.Equal(t, []string{
		"join",
		"leave",
		"list",
		"invite",
		"kick",
		"msg",
		"nick",
		"title",
		"whois",
		"config",
		"help",
		"quit",
	}, names)
}

func TestChatScreen_HelpCommand_emits_typed_event(t *testing.T) {
	screen := NewChatScreen(t.Context(), newScopeTestSession(t))

	cmd, err := command.Execute(screen.CommandScope(), "/help")
	require.NoError(t, err)
	require.NotNil(t, cmd)

	msg := cmd()
	event, ok := msg.(systemEventMsg)
	require.True(t, ok, "expected systemEventMsg, got %T", msg)
	require.Equal(t, systemEventMsg{
		events: []components.ChatLine{components.Help{}},
	}, event)
}

func TestChatScreen_QuitCommand_returns_quit(t *testing.T) {
	screen := NewChatScreen(t.Context(), newScopeTestSession(t))

	cmd, err := command.Execute(screen.CommandScope(), "/quit")
	require.NoError(t, err)
	require.NotNil(t, cmd)

	msg := cmd()
	_, ok := msg.(tea.QuitMsg)
	require.True(t, ok, "expected tea.QuitMsg, got %T", msg)
}
