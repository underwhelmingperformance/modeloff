package chatcmd

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

// TestCloseCommand_Run_answers_per_window_kind pins what `/close`
// means in each kind of window. Only a channel reaches the wire; a
// query window is client state the server holds nothing for, and
// `&modeloff` is the client's own view of the server.
func TestCloseCommand_Run_answers_per_window_kind(t *testing.T) {
	tests := []struct {
		name   string
		active domain.Window
		want   tea.Msg
	}{
		{
			name: "no window in view",
			want: UsageError{Command: "close", Usage: "no window to close"},
		},
		{
			name:   "the status window",
			active: domain.WindowKey(domain.StatusChannelName),
			want:   UsageError{Command: "close", Usage: "&modeloff stays open for the session"},
		},
		{
			name:   "a query window",
			active: domain.WindowKey("inst-botty"),
			want:   DMClosedMsg{Window: "inst-botty"},
		},
		{
			name:   "the self query window",
			active: domain.WindowKey(""),
			want:   DMClosedMsg{Window: ""},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CloseCommand{}.Run(t.Context(), Context{Active: tc.active})()

			if closed, isDM := got.(DMClosedMsg); isDM {
				require.False(t, closed.At.IsZero(), "the close should be stamped")
				closed.At = time.Time{}
				got = closed
			}

			require.Equal(t, tc.want, got)
		})
	}
}

// TestCloseCommand_Run_parts_a_channel_window pins the other half of
// the rule: a channel window exists because the user is in the
// channel, so closing it leaves.
func TestCloseCommand_Run_parts_a_channel_window(t *testing.T) {
	sess, user := newToolTestSession(t)
	require.NoError(t, user.Join(t.Context(), "#general"))

	rc := Context{
		Session: sess,
		Active:  domain.WindowKey("#general"),
		Actor:   user.Instance(),
		Client:  user,
	}

	require.Nil(t, CloseCommand{}.Run(t.Context(), rc)())

	_, stillJoined := user.Channels().Get("#general")
	require.False(t, stillJoined)
}
