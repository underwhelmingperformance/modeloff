package chatcmd

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

// TestContext_errorEvent_stamps_issuing_window proves errorEvent
// carries the window the command was issued from (rc.Active) as
// ErrorEvent.Target, the same role Whois.Target plays for a
// `/whois` reply. Without it, a command issued in one window whose
// failure arrives after the user has switched elsewhere would
// report into the wrong conversation.
func TestContext_errorEvent_stamps_issuing_window(t *testing.T) {
	tests := []struct {
		name   string
		active domain.ChannelName
	}{
		{name: "channel window", active: "#dev"},
		{name: "DM window", active: "claud3"},
		{name: "no active window", active: ""},
	}

	wantErr := errors.New("boom")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := Context{Active: tt.active}

			got := rc.errorEvent("join", wantErr)

			require.Equal(t, tt.active, got.Target)
			require.Equal(t, "join", got.Operation)
			require.Equal(t, wantErr, got.Err)
		})
	}
}
