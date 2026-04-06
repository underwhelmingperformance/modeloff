package components

import (
	"testing"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui"
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
			got := ansi.Strip(RenderStatusBar(tt.width, bindings, nil))

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
	}, nil))

	require.Contains(t, got, "Tab accept")
	require.Contains(t, got, "^C quit")
}

func TestStatusBar_renders_rhs_summary_when_space_allows(t *testing.T) {
	got := ansi.Strip(RenderStatusBar(120, []key.Binding{
		key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("^C", "quit")),
	}, []ui.StatusItem{{
		ID:       "metrics",
		Side:     ui.StatusSideRight,
		Priority: 100,
		Full:     "req 4  in 12  out 8  cache 5/2  cost 0.2500",
	}}))

	require.Contains(t, got, "^C quit")
	require.Contains(t, got, "req 4")
	require.Contains(t, got, "cost 0.2500")
}

func TestStatusBar_preserves_rhs_by_shortening_key_help(t *testing.T) {
	got := ansi.Strip(RenderStatusBar(80, []key.Binding{
		key.NewBinding(key.WithKeys("ctrl+d", "ctrl+u"), key.WithHelp("^D/U", "channels")),
		key.NewBinding(key.WithKeys("ctrl+o"), key.WithHelp("^O", "switch")),
		key.NewBinding(key.WithKeys("ctrl+l"), key.WithHelp("^L", "logs")),
		key.NewBinding(key.WithKeys("pgup", "pgdown"), key.WithHelp("PgUp/Dn", "scroll")),
		key.NewBinding(key.WithKeys("ctrl+up", "ctrl+down"), key.WithHelp("^↑/↓", "scroll")),
		key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑↓", "history")),
		key.NewBinding(key.WithKeys("enter"), key.WithHelp("↵", "send")),
		key.NewBinding(key.WithKeys("ctrl+n"), key.WithHelp("^N", "nicks")),
		key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("^C", "quit")),
	}, []ui.StatusItem{{
		ID:       "obs",
		Side:     ui.StatusSideRight,
		Priority: 100,
		Full:     "responding",
	}}))

	require.Contains(t, got, "responding")
	require.NotContains(t, got, "channels")
}

func TestStatusBar_compacts_lower_priority_status_first(t *testing.T) {
	got := ansi.Strip(RenderStatusBar(40, []key.Binding{
		key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("^C", "quit")),
	}, []ui.StatusItem{
		{
			ID:       "metrics",
			Side:     ui.StatusSideRight,
			Priority: 100,
			Full:     "req 44  in 120  out 80  cache 10/4  cost 0.2500",
			Compact:  "120/80  c10/4  0.2500",
		},
		{
			ID:       "obs",
			Side:     ui.StatusSideRight,
			Priority: 10,
			Full:     "obs metrics",
			Compact:  "obs",
		},
	}))

	require.Contains(t, got, "120/80")
	require.Contains(t, got, "obs")
}
