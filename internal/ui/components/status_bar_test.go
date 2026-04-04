package components

import (
	"testing"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestStatusBar_abbreviates_at_narrow_width(t *testing.T) {
	bindings := []key.Binding{
		key.NewBinding(key.WithKeys("ctrl+d", "ctrl+u"), key.WithHelp("^D/U", "channels")),
		key.NewBinding(key.WithKeys("ctrl+o"), key.WithHelp("^O", "switch")),
		key.NewBinding(key.WithKeys("ctrl+n"), key.WithHelp("^N", "nicks")),
		key.NewBinding(key.WithKeys("pgup", "pgdown"), key.WithHelp("PgUp/Dn", "scroll")),
		key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("^C", "quit")),
	}

	tests := []struct {
		name      string
		width     int
		wantFull  bool
		wantShort bool
	}{
		{
			name:      "wide enough for full bar",
			width:     100,
			wantFull:  true,
			wantShort: false,
		},
		{
			name:      "narrow triggers abbreviation",
			width:     35,
			wantFull:  false,
			wantShort: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ansi.Strip(RenderStatusBar(tt.width, bindings))

			if tt.wantFull {
				require.Contains(t, got, "channels")
				require.Contains(t, got, "scroll")
			}

			if tt.wantShort {
				require.NotContains(t, got, "channels")
				require.NotContains(t, got, "scroll")
				require.Contains(t, got, "^O")
				require.Contains(t, got, "^C")
			}
		})
	}
}

func TestStatusBar_shows_context_hint_when_present(t *testing.T) {
	got := ansi.Strip(RenderStatusBar(120, []key.Binding{
		key.NewBinding(key.WithKeys("tab"), key.WithHelp("Tab", "accept")),
		key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑↓", "navigate")),
		key.NewBinding(key.WithKeys("esc"), key.WithHelp("Esc", "dismiss")),
		key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("^C", "quit")),
	}))

	require.Contains(t, got, "Tab accept")
	require.Contains(t, got, "^C quit")
}
