package components

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

// RenderStatusBar renders the active keybindings for the current model tree.
func RenderStatusBar(width int, bindings []key.Binding) string {
	active := ui.ActiveKeyBindings(bindings)
	if len(active) == 0 {
		return ""
	}

	parts := make([]string, 0, len(active))
	for _, binding := range active {
		help := binding.Help()
		if help.Desc == "" {
			parts = append(parts, help.Key)
			continue
		}

		parts = append(parts, fmt.Sprintf("%s %s", help.Key, help.Desc))
	}

	text := strings.Join(parts, "  ")
	if lipgloss.Width(text) > width {
		parts = parts[:0]

		for _, binding := range active {
			parts = append(parts, binding.Help().Key)
		}

		text = strings.Join(parts, "  ")
	}

	return theme.Dim.Width(width).Render(truncateLine(text, width))
}
