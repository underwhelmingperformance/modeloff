package screens

import (
	"fmt"
	"strings"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/components"
	"github.com/laney/modeloff/internal/ui/theme"
)

type welcomeChecklist struct {
	nick         domain.Nick
	hasAPIKey    bool
	channelCount int
	modelCount   int
}

func newWelcomeChecklist(nick domain.Nick, hasAPIKey bool) welcomeChecklist {
	return welcomeChecklist{
		nick:      nick,
		hasAPIKey: hasAPIKey,
	}
}

func (w welcomeChecklist) text() string {
	km := components.DefaultSidebarKeyMap

	lines := []string{
		theme.Info.Render("Welcome to modeloff"),
		theme.Dim.Render("Connected as ") + theme.UserNick.Render(string(w.nick)),
		"",
	}

	// API key status.
	if w.hasAPIKey {
		lines = append(lines, theme.Success.Render("✓")+" API key configured")
	} else {
		lines = append(lines,
			theme.Error.Render("✗")+" API key not configured",
			"  "+theme.Bold.Render("/config api-key <value>"),
		)
	}

	lines = append(lines, "")

	// First channel status.
	if w.channelCount > 0 {
		lines = append(lines, theme.Success.Render("✓")+fmt.Sprintf(" %d channel(s) joined", w.channelCount))
	} else {
		lines = append(lines,
			theme.Error.Render("✗")+" No channels joined",
			"  "+theme.Bold.Render("/join #general"),
		)
	}

	lines = append(lines, "")

	// Models available status.
	if w.hasAPIKey {
		noun := "models"
		if w.modelCount == 1 {
			noun = "model"
		}
		lines = append(lines, theme.Success.Render("✓")+fmt.Sprintf(" %d %s available", w.modelCount, noun))
	} else {
		lines = append(lines, theme.Dim.Render("  Set an API key first to browse models."))
	}

	lines = append(lines, "")

	// Sidebar keybinding hints derived from the key map.
	keys := km.Down.Help().Key + ", " + km.Up.Help().Key + ", " + km.Select.Help().Key
	lines = append(lines,
		theme.Bold.Render(keys),
		theme.Dim.Render("Move around the sidebar once you have channels."),
	)

	return strings.Join(lines, "\n")
}
