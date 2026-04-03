package screens

import (
	"strings"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/theme"
)

// welcomeText returns the styled onboarding text shown in the chat
// view content area before any channels exist.
func welcomeText(nick domain.Nick) string {
	lines := []string{
		theme.Info.Render("Welcome to modeloff"),
		theme.Dim.Render("Connected as ") + theme.UserNick.Render(string(nick)),
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

	return strings.Join(lines, "\n")
}
