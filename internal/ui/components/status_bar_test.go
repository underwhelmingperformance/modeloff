package components

import (
	"testing"

	"github.com/charmbracelet/bubbles/key"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/uitest"
)

func TestStatusBar_abbreviates_at_narrow_width(t *testing.T) {
	bindings := []ui.KeyBinding{
		ui.Bind(key.NewBinding(key.WithKeys("ctrl+d", "ctrl+u"), key.WithHelp("^D/U", "channels"))),
		ui.Bind(key.NewBinding(key.WithKeys("ctrl+o"), key.WithHelp("^O", "switch channel"))),
		ui.Bind(key.NewBinding(key.WithKeys("ctrl+n"), key.WithHelp("^N", "nicks"))),
		ui.Bind(key.NewBinding(key.WithKeys("pgup", "pgdown"), key.WithHelp("PgUp/Dn", "scroll"))),
		ui.Bind(key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("^C", "quit"))),
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
			got := uitest.NonEmptyLines(RenderStatusBar(tt.width, bindings, nil))

			if tt.wantFull {
				require.Equal(t, []string{"^D/U channels  ^O switch channel  ^N nicks  PgUp/Dn scroll  ^C quit"}, got)
			}

			if tt.wantShort {
				require.Equal(t, []string{"^D/U  ^O  ^N  PgUp/Dn  ^C"}, got)
			}
		})
	}
}

func TestStatusBar_shows_context_hint_when_present(t *testing.T) {
	got := uitest.NonEmptyLines(RenderStatusBar(120, []ui.KeyBinding{
		ui.Bind(key.NewBinding(key.WithKeys("tab"), key.WithHelp("Tab", "accept"))),
		ui.Bind(key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑↓", "navigate"))),
		ui.Bind(key.NewBinding(key.WithKeys("esc"), key.WithHelp("Esc", "dismiss"))),
		ui.Bind(key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("^C", "quit"))),
	}, nil))

	require.Equal(t, []string{"Tab accept  ↑↓ navigate  Esc dismiss  ^C quit"}, got)
}

func TestStatusBar_renders_rhs_summary_when_space_allows(t *testing.T) {
	got := uitest.NonEmptyLines(RenderStatusBar(120, []ui.KeyBinding{
		ui.Bind(key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("^C", "quit"))),
	}, []ui.StatusItem{{
		ID:       "metrics",
		Side:     ui.StatusSideRight,
		Priority: 100,
		Full:     "req 4  in 12  out 8  cache 5/2  cost 0.2500",
	}}))

	require.Equal(t, []string{"^C quit                                                                      req 4  in 12  out 8  cache 5/2  cost 0.2500"}, got)
}

func TestStatusBar_preserves_rhs_by_shortening_key_help(t *testing.T) {
	got := uitest.NonEmptyLines(RenderStatusBar(80, []ui.KeyBinding{
		ui.Bind(key.NewBinding(key.WithKeys("ctrl+d", "ctrl+u"), key.WithHelp("^D/U", "channels"))),
		ui.Bind(key.NewBinding(key.WithKeys("ctrl+o"), key.WithHelp("^O", "switch channel"))),
		ui.Bind(key.NewBinding(key.WithKeys("ctrl+l"), key.WithHelp("^L", "logs"))),
		ui.Bind(key.NewBinding(key.WithKeys("pgup", "pgdown"), key.WithHelp("PgUp/Dn", "scroll"))),
		ui.Bind(key.NewBinding(key.WithKeys("ctrl+up", "ctrl+down"), key.WithHelp("^↑/↓", "scroll"))),
		ui.Bind(key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑↓", "history"))),
		ui.Bind(key.NewBinding(key.WithKeys("enter"), key.WithHelp("↵", "send"))),
		ui.Bind(key.NewBinding(key.WithKeys("ctrl+n"), key.WithHelp("^N", "nicks"))),
		ui.Bind(key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("^C", "quit"))),
	}, []ui.StatusItem{{
		ID:       "obs",
		Side:     ui.StatusSideRight,
		Priority: 100,
		Full:     "responding",
	}}))

	require.Equal(t, []string{"^D/U  ^O  ^L  PgUp/Dn  ^↑/↓  ↑↓  ↵  ^N  ^C                            responding"}, got)
}

func TestStatusBar_compacts_lower_priority_status_first(t *testing.T) {
	got := uitest.NonEmptyLines(RenderStatusBar(40, []ui.KeyBinding{
		ui.Bind(key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("^C", "quit"))),
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

	require.Equal(t, []string{"^C quit       120/80  c10/4  0.2500  obs"}, got)
}
