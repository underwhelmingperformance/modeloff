package screens

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

// WelcomeScreen renders the first-run onboarding state shown before
// any channels exist.
type WelcomeScreen struct {
	nick domain.Nick
}

// NewWelcomeScreen creates a welcome screen for the given user nick.
func NewWelcomeScreen(nick domain.Nick) WelcomeScreen {
	return WelcomeScreen{nick: nick}
}

// Init implements ui.Model.
func (s WelcomeScreen) Init() tea.Cmd {
	return nil
}

// Update implements ui.Model.
func (s WelcomeScreen) Update(_ tea.Msg) (ui.Model, tea.Cmd) {
	return s, nil
}

// View implements ui.Model.
func (s WelcomeScreen) View(width, height int) string {
	if width < theme.MinTerminalWidth {
		return theme.NarrowTerminalView(width, height)
	}

	contentWidth := width - 4

	lines := []string{
		theme.Info.Render("Welcome to modeloff"),
		theme.Dim.Render("Connected as ") + theme.UserNick.Render(string(s.nick)),
		"",
		"Start by creating a channel and configuring OpenRouter.",
		"",
		theme.Bold.Render("/join #general"),
		theme.Dim.Render("Create your first channel."),
		"",
		theme.Bold.Render("/config api-key <value>"),
		theme.Dim.Render("Set the API key needed to invite models."),
		"",
		theme.Bold.Render("ctrl+d, ctrl+u, ctrl+o"),
		theme.Dim.Render("Move around the sidebar once you have channels."),
	}

	body := lipgloss.NewStyle().
		Width(contentWidth).
		Render(strings.Join(lines, "\n"))

	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, body)
}
