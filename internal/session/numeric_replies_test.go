package session

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
)

// TestMembershipPreconditions_return_typed_numeric_errors pins the
// RFC 2812 numeric vocabulary for the three membership-precondition
// refusals that the dispatcher surfaces through `Response.Err`:
//
//   - PART against a channel the actor is not on -> 442
//     ERR_NOTONCHANNEL ([domain.NotOnChannelError]).
//   - KICK whose target is not a channel member -> 441
//     ERR_USERNOTINCHANNEL ([domain.UserNotInChannelError]).
//   - INVITE whose target is already a channel member -> 443
//     ERR_USERONCHANNEL ([domain.UserOnChannelError]).
//
// Each case dispatches through the user-client's `Send -> Handle`
// path and asserts the whole [protocol.Response] the issuer sees.
func TestMembershipPreconditions_return_typed_numeric_errors(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, sess *Session, s *storemod.SQLiteStore)
		cmd   protocol.Command
		want  protocol.Response
	}{
		{
			name: "part of a channel the actor is not on returns 442",
			setup: func(t *testing.T, sess *Session, s *storemod.SQLiteStore) {
				seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
				seedChannelWithMembers(t, sess, s, "#room", "botty")
			},
			cmd:  protocol.Part{Channel: "#room", Reason: "bye"},
			want: protocol.Response{Err: domain.NotOnChannelError{Channel: "#room", Command: "PART", At: fixedTime}},
		},
		{
			name: "kick of a non-member returns 441",
			setup: func(t *testing.T, sess *Session, s *storemod.SQLiteStore) {
				seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
				seedChannelWithMembers(t, sess, s, "#room", "testuser")
			},
			cmd:  protocol.Kick{Nick: "botty", Channel: "#room"},
			want: protocol.Response{Err: domain.UserNotInChannelError{Nick: "botty", Channel: "#room", Command: "KICK", At: fixedTime}},
		},
		{
			name: "invite of an existing member returns 443",
			setup: func(t *testing.T, sess *Session, s *storemod.SQLiteStore) {
				seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
				seedChannelWithMembers(t, sess, s, "#room", "testuser", "botty")
			},
			cmd:  protocol.Invite{Nick: "botty", Channel: "#room"},
			want: protocol.Response{Err: domain.UserOnChannelError{Nick: "botty", Channel: "#room", At: fixedTime}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sess, s := newTestSession(t)
				ctx := t.Context()

				tc.setup(t, sess, s)

				got, err := userClient(t, sess).Send(ctx, tc.cmd)
				require.NoError(t, err)
				require.Equal(t, tc.want, got)
			})
		})
	}
}

// TestUserJoin_NamesReply_is_terminated_by_NamesEnd pins that a
// user join's RFC 2812 numeric 353 (RPL_NAMREPLY) member-list
// snapshot is followed by the numeric 366 (RPL_ENDOFNAMES)
// terminator on the joiner's own subscription, naming the same
// channel.
func TestUserJoin_NamesReply_is_terminated_by_NamesEnd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#dev"))
		synctest.Wait()

		user := userInstance(t, sess)
		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Target:     "#dev",
				Nick:       "testuser",
				InstanceID: user.ID(),
				Created:    true,
				At:         fixedTime,
				Instance:   user,
			},
			domain.NamesReplyEvent{
				Channel: "#dev",
				Members: testMembers(t, sess, s, "testuser"),
				At:      fixedTime,
			},
			domain.NamesEnd{
				Channel: "#dev",
				At:      fixedTime,
			},
		}, collectEmittedEvents(t, sess))
	})
}
