package chatcmd

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

func TestContext_ActiveName_distinguishes_self_DM_from_absence(t *testing.T) {
	type result struct {
		name domain.ChannelName
		ok   bool
	}

	tests := []struct {
		name   string
		active domain.Window
		want   result
	}{
		{name: "self DM", active: domain.WindowKey(""), want: result{ok: true}},
		{name: "no active window"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := (Context{Active: tc.active}).ActiveName()

			require.Equal(t, tc.want, result{name: got, ok: ok})
		})
	}
}

// TestContext_errorEvent_stamps_issuing_window proves errorEvent
// carries the window the command was issued from (rc.Active) as
// ErrorEvent.Target, the same role Whois.Target plays for a
// `/whois` reply. Without it, a command issued in one window whose
// failure arrives after the user has switched elsewhere would
// report into the wrong conversation.
func TestContext_errorEvent_stamps_issuing_window(t *testing.T) {
	tests := []struct {
		name       string
		active     domain.Window
		wantTarget domain.ChannelName
	}{
		{name: "channel window", active: domain.WindowKey("#dev"), wantTarget: "#dev"},
		{name: "DM window", active: domain.WindowKey("claud3"), wantTarget: "claud3"},
		{name: "self DM window", active: domain.WindowKey("")},
		{name: "no active window"},
	}

	wantErr := errors.New("boom")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := Context{Active: tt.active}

			got := rc.errorEvent("join", wantErr)

			require.Equal(t, tt.wantTarget, got.Target)
			require.Equal(t, "join", got.Operation)
			require.Equal(t, wantErr, got.Err)
		})
	}
}
