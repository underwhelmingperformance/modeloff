package session

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
)

// TestSession_privmsg_to_a_channel_that_does_not_exist covers RFC
// 2812 §3.3.1: a PRIVMSG naming a target the server does not know is
// refused with 401 ERR_NOSUCHNICK. The refusal happens at the send
// gate, so nothing reaches the event log under a name no channel
// answers to.
func TestSession_privmsg_to_a_channel_that_does_not_exist(t *testing.T) {
	tests := []struct {
		name string
		send func(target domain.ChannelName) protocol.Command
	}{
		{
			name: "privmsg",
			send: func(target domain.ChannelName) protocol.Command {
				return protocol.PrivMsg{Target: target, Body: "anyone there"}
			},
		},
		{
			name: "action",
			send: func(target domain.ChannelName) protocol.Command {
				return protocol.Action{Target: target, Body: "waves"}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess, _ := newTestSession(t)
			ctx := t.Context()

			resp, err := userClient(t, sess).Send(ctx, tt.send("#ghost"))
			require.NoError(t, err)

			var missing domain.NoSuchChannelError
			require.ErrorAs(t, resp.Err, &missing)
			require.Equal(t, domain.NoSuchChannelError{Channel: "#ghost", At: fixedTime}, missing)
			require.Empty(t, resp.Events)

			stored, err := sess.EventsBefore(ctx, "#ghost", nil, 10)
			require.NoError(t, err)
			require.Empty(t, stored, "a refused message is not filed")

			_, err = sess.loadChannelWindow(ctx, "#ghost")
			require.ErrorIs(t, err, storemod.ErrNoSuchChannel)
		})
	}
}
