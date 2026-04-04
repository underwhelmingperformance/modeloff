package screens

import (
	"context"
	"testing"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/ui"
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

func (scopeTestAPI) GenerateNick(context.Context, domain.ModelID, domain.ModelID) (domain.Nick, error) {
	return "scopebot", nil
}

func newScopeTestSession(t *testing.T) *session.Session {
	t.Helper()

	return session.New(storemod.NewFileStore(t.TempDir()), nil, scopeTestAPI{}, nil, "testuser")
}

func TestChatScreen_CommandScope_specs_are_complete(t *testing.T) {
	screen := NewChatScreen(t.Context(), newScopeTestSession(t))
	scope := screen.CommandScope()

	require.NotEmpty(t, scope.Commands)

	seen := make(map[string]struct{}, len(scope.Commands))
	for _, spec := range scope.Commands {
		require.NotEmpty(t, spec.Name)
		require.NotEmpty(t, spec.Help)
		require.NotEmpty(t, spec.Usage)
		require.NotNil(t, spec.Handler)

		_, exists := seen[spec.Name]
		require.Falsef(t, exists, "duplicate command %q", spec.Name)
		seen[spec.Name] = struct{}{}
	}
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
		"topic",
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

func TestChatScreen_KeyBindings_collect_active_bindings(t *testing.T) {
	screen := NewChatScreen(t.Context(), newScopeTestSession(t))

	loaded, _ := screen.Update(screen.Init()())
	screen = loaded.(*ChatScreen)

	require.Equal(t, []key.Help{
		{Key: "↵", Desc: "send"},
		{Key: "^N", Desc: "nicks"},
		{Key: "^C", Desc: "quit"},
	}, bindingHelp(ui.ActiveKeyBindings(screen.KeyBindings())))
}

func TestChatScreen_KeyBindings_switch_to_popover_bindings(t *testing.T) {
	screen := NewChatScreen(t.Context(), newScopeTestSession(t))

	loaded, _ := screen.Update(screen.Init()())
	screen = loaded.(*ChatScreen)

	updated, _ := screen.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	screen = updated.(*ChatScreen)

	require.Equal(t, []key.Help{
		{Key: "Tab", Desc: "accept"},
		{Key: "↑↓", Desc: "navigate"},
		{Key: "Esc", Desc: "dismiss"},
		{Key: "↵", Desc: "send"},
		{Key: "^N", Desc: "nicks"},
		{Key: "^C", Desc: "quit"},
	}, bindingHelp(ui.ActiveKeyBindings(screen.KeyBindings())))
}

func TestChatScreen_popover_no_duplicate_suggestions(t *testing.T) {
	screen := NewChatScreen(t.Context(), newScopeTestSession(t))
	scope := screen.CommandScope()

	for _, spec := range scope.Commands {
		raw := "/" + spec.Name
		completion := command.Complete(scope, raw, len([]rune(raw)), command.CompletionContext{})

		require.True(t, completion.Visible, "popover must be visible for %s", raw)
		require.True(t, completion.SuppressList,
			"exact match for %s must suppress the suggestion list so usage and suggestions don't visually duplicate", raw)
	}
}

func bindingHelp(bindings []key.Binding) []key.Help {
	help := make([]key.Help, 0, len(bindings))

	for _, binding := range bindings {
		help = append(help, binding.Help())
	}

	return help
}
