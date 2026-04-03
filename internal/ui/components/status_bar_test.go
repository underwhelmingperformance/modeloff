package components

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestStatusBar_abbreviates_at_narrow_width(t *testing.T) {
	tests := []struct {
		name      string
		width     int
		wantFull  bool
		wantShort bool
	}{
		{
			name:      "wide enough for full bar",
			width:     80,
			wantFull:  true,
			wantShort: false,
		},
		{
			name:      "narrow triggers abbreviation",
			width:     60,
			wantFull:  false,
			wantShort: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ansi.Strip(statusBar(tt.width))

			if tt.wantFull {
				require.Contains(t, got, "channels")
				require.Contains(t, got, "commands")
				require.Contains(t, got, "scroll")
			}

			if tt.wantShort {
				require.NotContains(t, got, "channels")
				require.NotContains(t, got, "commands")
				require.Contains(t, got, "switch")
				require.Contains(t, got, "quit")
			}
		})
	}
}
