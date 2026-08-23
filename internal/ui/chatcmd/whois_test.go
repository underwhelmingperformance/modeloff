package chatcmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// TestWhoisCommand_ToCommand_carries_issuing_window proves the
// `/whois` wire command stamps the active window so the dispatcher
// can route the reply back to where it was issued.
func TestWhoisCommand_ToCommand_carries_issuing_window(t *testing.T) {
	tests := []struct {
		name   string
		active domain.Window
		want   protocol.Whois
	}{
		{
			name:   "channel",
			active: domain.WindowKey("#dev"),
			want:   protocol.Whois{Nick: "claud3", Window: protocol.ChannelWindowTarget("#dev")},
		},
		{
			name:   "direct message with the user sentinel",
			active: domain.WindowKey(""),
			want:   protocol.Whois{Nick: "claud3", Window: protocol.DirectWindowTarget("")},
		},
		{
			name: "no window",
			want: protocol.Whois{Nick: "claud3"},
		},
		{
			name:   "status window",
			active: domain.NewStatusWindow(time.Time{}),
			want:   protocol.Whois{Nick: "claud3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (WhoisCommand{Nick: "claud3"}).ToCommand(Context{Active: tt.active})
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestSingleResponseEvent_requires_one_event_of_the_expected_type(t *testing.T) {
	whois := domain.Whois{Nick: "claud3", ModelID: "test/model"}
	tests := []struct {
		name   string
		events []protocol.Event
		want   domain.Whois
		ok     bool
	}{
		{name: "empty"},
		{name: "one expected event", events: []protocol.Event{whois}, want: whois, ok: true},
		{name: "one different event", events: []protocol.Event{domain.SystemNotice{Text: "unexpected"}}},
		{name: "expected plus extra", events: []protocol.Event{whois, domain.SystemNotice{Text: "unexpected"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := singleResponseEvent[domain.Whois](tt.events)
			require.Equal(t, tt.want, got)
			require.Equal(t, tt.ok, ok)
		})
	}
}
