package components

import (
	"testing"

	"charm.land/bubbles/v2/key"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/uitest"
)

func TestStatusBar_renders_only_the_highest_priority_complete_hints(t *testing.T) {
	bindings := []ui.KeyBinding{
		ui.Bind(key.NewBinding(key.WithKeys("ctrl+d", "ctrl+u"), key.WithHelp("^D/U", "channels"))).
			WithHelpMetadata(ui.KeyHelpNavigation, ui.KeyHintLow),
		ui.Bind(key.NewBinding(key.WithKeys("ctrl+o"), key.WithHelp("^O", "select window"))).
			WithHelpMetadata(ui.KeyHelpNavigation, ui.KeyHintNormal),
		ui.Bind(key.NewBinding(key.WithKeys("ctrl+n"), key.WithHelp("^N", "next window"))).
			WithHelpMetadata(ui.KeyHelpNavigation, ui.KeyHintHigh),
		ui.Bind(key.NewBinding(key.WithKeys("pgup", "pgdown"), key.WithHelp("PgUp/Dn", "scroll"))).
			WithHelpMetadata(ui.KeyHelpNavigation, ui.KeyHintNone),
		ui.Bind(key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("^C", "quit"))).
			WithHelpMetadata(ui.KeyHelpApplication, ui.KeyHintLow),
		ui.Bind(key.NewBinding(key.WithKeys("f1"), key.WithHelp("F1", "shortcuts"))).
			WithHelpMetadata(ui.KeyHelpApplication, ui.KeyHintEssential),
	}

	tests := []struct {
		name  string
		width int
		want  string
	}{
		{
			name:  "wide enough for four hints",
			width: 100,
			want:  "^D/U channels  ^O select window  ^N next window  F1 shortcuts",
		},
		{
			name:  "narrow drops whole low-priority hints",
			width: 35,
			want:  "^N next window  F1 shortcuts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := uitest.NonEmptyLines(renderStatusBar(tt.width, bindings, nil))

			require.Equal(t, []string{tt.want}, got)
		})
	}
}

func TestStatusBar_shows_context_hint_when_present(t *testing.T) {
	got := uitest.NonEmptyLines(renderStatusBar(120, []ui.KeyBinding{
		ui.Bind(key.NewBinding(key.WithKeys("tab"), key.WithHelp("Tab", "accept"))).
			WithHelpMetadata(ui.KeyHelpCompletion, ui.KeyHintHigh),
		ui.Bind(key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑↓", "navigate"))).
			WithHelpMetadata(ui.KeyHelpCompletion, ui.KeyHintHigh),
		ui.Bind(key.NewBinding(key.WithKeys("esc"), key.WithHelp("Esc", "dismiss"))).
			WithHelpMetadata(ui.KeyHelpCompletion, ui.KeyHintHigh),
		ui.Bind(key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("^C", "quit"))),
	}, nil))

	require.Equal(t, []string{"Tab accept  ↑↓ navigate  Esc dismiss  ^C quit"}, got)
}

func TestStatusBar_renders_rhs_summary_when_space_allows(t *testing.T) {
	got := uitest.NonEmptyLines(renderStatusBar(120, []ui.KeyBinding{
		ui.Bind(key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("^C", "quit"))),
	}, []ui.StatusItem{{
		ID:       "metrics",
		Side:     ui.StatusSideRight,
		Priority: 100,
		Full:     "req 4  in 12  out 8  cache 5/2  cost 0.2500",
	}}))

	require.Equal(t, []string{"^C quit                                                                      req 4  in 12  out 8  cache 5/2  cost 0.2500"}, got)
}

func TestStatusBar_preserves_rhs_while_dropping_complete_key_hints(t *testing.T) {
	got := uitest.NonEmptyLines(renderStatusBar(80, []ui.KeyBinding{
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

	require.Equal(t, []string{"^D/U channels  ^O switch channel  ^L logs  PgUp/Dn scroll             responding"}, got)
}

func TestStatusBar_compacts_lower_priority_status_first(t *testing.T) {
	got := uitest.NonEmptyLines(renderStatusBar(40, []ui.KeyBinding{
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
