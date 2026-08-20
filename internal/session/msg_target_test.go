package session

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// TestSession_PrivMsg_resolves_its_target covers the server's half of
// RFC 2812 §3.3.1 addressing. The client says what it is addressing
// (a channel, a nick, or a client id) and the dispatcher decides which
// conversation that is. A target naming nobody is answered with
// [domain.UnknownNickError] (numeric 401) and nothing is logged: the
// message the client thought it sent has to come back as a refusal,
// because a client that is told nothing assumes it arrived.
func TestSession_PrivMsg_resolves_its_target(t *testing.T) {
	bottyID := testMemberID("botty")

	cases := []struct {
		name   string
		target protocol.MsgTarget
		want   domain.ChannelName
		wantEr error
	}{
		{
			name:   "a channel by name",
			target: protocol.ChannelTarget("#dev"),
			want:   "#dev",
		},
		{
			name:   "a channel in another case reaches the one channel",
			target: protocol.ChannelTarget("#DEV"),
			want:   "#dev",
		},
		{
			name:   "a nick keys the DM by the counterpart",
			target: protocol.NickTarget("botty"),
			want:   domain.ChannelName(bottyID),
		},
		{
			name:   "a nick in another case reaches the same client",
			target: protocol.NickTarget("BoTTy"),
			want:   domain.ChannelName(bottyID),
		},
		{
			name:   "a client id keys the DM by the same counterpart",
			target: protocol.ClientTarget(bottyID),
			want:   domain.ChannelName(bottyID),
		},
		{
			name:   "a nick nobody holds is refused",
			target: protocol.NickTarget("bottt"),
			wantEr: domain.UnknownNickError{Nick: "bottt", At: fixedTime},
		},
		{
			name:   "an empty nick is ungrammatical, not unknown",
			target: protocol.NickTarget(""),
			wantEr: domain.ErroneousNicknameError{Nick: "", Reason: domain.NickEmpty, At: fixedTime},
		},
		{
			name:   "a client id nobody holds is refused",
			target: protocol.ClientTarget("inst-nobody"),
			wantEr: domain.UnknownNickError{Nick: "inst-nobody", At: fixedTime},
		},
		{
			name:   "a channel that does not exist is refused",
			target: protocol.ChannelTarget("#nowhere"),
			wantEr: domain.NoSuchChannelError{Channel: "#nowhere", At: fixedTime},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess, s := newTestSession(t)
			ctx := t.Context()

			require.NoError(t, userJoin(ctx, t, sess, "#dev"))
			seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})

			resp, err := userClient(t, sess).Send(ctx, protocol.PrivMsg{
				Target: tc.target,
				Body:   "hello",
			})
			require.NoError(t, err)

			if tc.wantEr != nil {
				require.Equal(t, tc.wantEr, resp.Err)
				require.Empty(t, resp.Events)
				require.Empty(t, dmThreadMessages(t, s, "", bottyID))

				return
			}

			require.NoError(t, resp.Err)
			require.Equal(t, []protocol.Event{domain.Message{
				Target:     tc.want,
				From:       userNick(t, sess),
				InstanceID: "",
				Body:       "hello",
				At:         fixedTime,
			}}, resp.Events)
		})
	}
}

// TestSession_PrivMsg_to_an_unresolvable_nick_logs_nothing covers a
// model that mistypes a nick. It hears that it addressed nobody, and
// the event log holds nothing under the name it used.
func TestSession_PrivMsg_to_an_unresolvable_nick_logs_nothing(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})

	client := sess.LookupClient(protocol.ClientID(botty.ID()))
	require.NotNil(t, client)

	resp, err := client.Send(ctx, protocol.PrivMsg{
		Target: protocol.NickTarget("testusre"),
		Body:   "meant for the user",
	})
	require.NoError(t, err)
	require.Equal(t, domain.UnknownNickError{Nick: "testusre", At: fixedTime}, resp.Err)

	require.Empty(t, channelMessages(t, s, "testusre"))
	require.Empty(t, dmThreadMessages(t, s, "", botty.ID()))
}
