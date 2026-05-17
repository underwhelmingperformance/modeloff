package screens

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// TestConnectionScreen_View_fills_height pins that the animation
// occupies the full vertical budget; the connection screen has no
// status bar of its own (quit handling lives on the chat-screen
// it wraps).
func TestConnectionScreen_View_fills_height(t *testing.T) {
	s := NewConnectionScreen(ConnectionConfig{
		HasAPIKey: true,
		Nick:      "alice",
	}, nil)

	idle := s.View(80, 24)

	require.Equal(t, 24, lipgloss.Height(idle))
	require.Equal(t, []string{"… Connecting to modeloff"}, trimmedLines(idle),
		"only the centred animation is visible")
}

// trimmedLines returns the non-empty rows of a rendered view with
// ANSI escapes stripped and surrounding whitespace trimmed.
func trimmedLines(view string) []string {
	stripped := ansi.Strip(view)
	out := make([]string, 0)

	for line := range strings.SplitSeq(stripped, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		out = append(out, trimmed)
	}

	return out
}
