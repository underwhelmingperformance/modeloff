package screens

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

// PlaceholderScreen is a temporary screen shown after the connection
// sequence completes, until the main chat screen is built.
type PlaceholderScreen struct {
	nick string
}

// NewPlaceholderScreen creates a placeholder with the user's nick.
func NewPlaceholderScreen(nick string) PlaceholderScreen {
	return PlaceholderScreen{nick: nick}
}

// Init implements ui.Model.
func (s PlaceholderScreen) Init() tea.Cmd {
	return nil
}

// Update implements ui.Model.
func (s PlaceholderScreen) Update(_ tea.Msg) (ui.Model, tea.Cmd) {
	return s, nil
}

// View implements ui.Model.
func (s PlaceholderScreen) View(width, height int) string {
	text := theme.Info.Render("modeloff") + " — connected as " + theme.UserNick.Render(s.nick) +
		"\n\n" + theme.Dim.Render("Use /join to enter a channel. Main chat screen coming soon.")

	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, text)
}
