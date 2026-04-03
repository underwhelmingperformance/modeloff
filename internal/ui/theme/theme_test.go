package theme

import (
	"fmt"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"
)

func TestNickStyle_deterministic(t *testing.T) {
	a := NickStyle("alice")
	b := NickStyle("alice")

	require.Equal(t, a.GetForeground(), b.GetForeground())
}

func TestNickStyle_different_nicks_can_differ(t *testing.T) {
	nicks := []string{"alice", "bob", "charlie"}

	colours := make(map[lipgloss.ANSIColor]bool)

	for _, n := range nicks {
		fg := NickStyle(n).GetForeground().(lipgloss.ANSIColor)
		colours[fg] = true
	}

	require.Greater(t, len(colours), 1,
		"expected different nicks to produce different colours")
}

func TestNickStyle_uses_all_colours(t *testing.T) {
	colours := make(map[lipgloss.ANSIColor]bool)

	for i := range 100 {
		nick := fmt.Sprintf("nick%d", i)
		fg := NickStyle(nick).GetForeground().(lipgloss.ANSIColor)
		colours[fg] = true
	}

	require.Equal(t, len(nickColours), len(colours),
		"expected all %d nick colours to be used", len(nickColours))
}
