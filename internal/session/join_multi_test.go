package session

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
)

// TestSession_handleJoin_multi_target covers RFC 2812 §3.2.1's
// multi-target JOIN ("JOIN #a,#b,#c") at the dispatcher: a gate
// refusal on one channel must not stop the others named in the same
// command. `Response.Events` carries one entry per channel
// processed, in list order: a [domain.JoinedChannel] for the one
// that joined and the gate's typed refusal
// ([domain.ChannelFullError], the same type the single-channel JOIN
// already returns) for the one that did not. `Response.Err` carries
// every refusal joined together, so a caller that only checks
// success or failure still gets one answer.
func TestSession_handleJoin_multi_target(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#full", "botty")
	setChannelModes(t, sess, "#full", domain.ChannelModes{UserLimit: 1})

	resp, err := userClient(t, sess).Send(ctx, protocol.Join{
		Channels: []domain.ChannelName{"#open", "#full"},
	})
	require.NoError(t, err)

	var fullErr domain.ChannelFullError
	require.ErrorAs(t, resp.Err, &fullErr)
	require.Equal(t, domain.ChannelFullError{Channel: "#full", At: fixedTime}, fullErr)
	require.Equal(t, []protocol.Event{
		domain.JoinedChannel{Channel: "#open"},
		fullErr,
	}, resp.Events)

	openWindow, err := sess.loadChannelWindow(ctx, "#open")
	require.NoError(t, err)
	require.True(t, openWindow.Members.HasInstance(userInstance(t, sess)),
		"a refusal on #full must not stop #open from joining")

	fullWindow, err := sess.loadChannelWindow(ctx, "#full")
	require.NoError(t, err)
	require.False(t, fullWindow.Members.HasInstance(userInstance(t, sess)),
		"the refused channel must not gain the user as a member")
	require.Equal(t, 1, fullWindow.Members.Len())
}

// TestSession_handleJoin_refuses_over_cap_list covers the
// TARGMAX-style bound on a multi-target JOIN: a list longer than
// [protocol.MaxJoinTargets] is refused whole, with
// [domain.TooManyJoinTargetsError], before any channel in it is
// touched. Without this cap the connection's flood-control penalty
// (RFC 1459 §8.10), which charges a JOIN once regardless of its
// channel count, would let one command's channel list grow without
// bound and hold the session's single writer loop for as long as
// that list takes to process.
func TestSession_handleJoin_refuses_over_cap_list(t *testing.T) {
	sess, _ := newTestSession(t)
	ctx := t.Context()

	channels := make([]domain.ChannelName, protocol.MaxJoinTargets+1)
	for i := range channels {
		channels[i] = domain.ChannelName(fmt.Sprintf("#room%d", i))
	}

	resp, err := userClient(t, sess).Send(ctx, protocol.Join{Channels: channels})
	require.NoError(t, err)

	var tooMany domain.TooManyJoinTargetsError
	require.ErrorAs(t, resp.Err, &tooMany)
	require.Equal(t, domain.TooManyJoinTargetsError{
		Requested: protocol.MaxJoinTargets + 1,
		Max:       protocol.MaxJoinTargets,
		At:        fixedTime,
	}, tooMany)
	require.Empty(t, resp.Events)

	for _, ch := range channels {
		_, err := sess.loadChannelWindow(ctx, ch)
		require.ErrorIs(t, err, storemod.ErrNoSuchChannel,
			"an over-cap JOIN must not create any channel in its list")
	}
}

// TestSession_handleJoin_refuses_erroneous_channel_name covers the
// dispatcher's own backstop against a channel name that fails the
// RFC 2812 §1.3 grammar. A comma-separated JOIN list can produce a
// bare prefix from an empty entry between two commas even after the
// chatcmd grammar's own validation, and joinAs does not trust a
// client to have caught that or anything else first.
func TestSession_handleJoin_refuses_erroneous_channel_name(t *testing.T) {
	tests := []struct {
		name   string
		ch     domain.ChannelName
		reason domain.ChannelNameRejection
	}{
		{name: "bare hash", ch: "#", reason: domain.ChannelNameBare},
		{name: "bare ampersand", ch: "&", reason: domain.ChannelNameBare},
		{name: "embedded space", ch: "#de v", reason: domain.ChannelNameBadCharacter},
		{name: "embedded comma", ch: "#de,v", reason: domain.ChannelNameBadCharacter},
		{name: "too long", ch: domain.ChannelName("#" + strings.Repeat("d", domain.ChannelNameMaxLen)), reason: domain.ChannelNameTooLong},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess, _ := newTestSession(t)
			ctx := t.Context()

			resp, err := userClient(t, sess).Send(ctx, protocol.Join{
				Channels: []domain.ChannelName{tt.ch},
			})
			require.NoError(t, err)

			want := domain.ErroneousChannelNameError{Channel: tt.ch, Reason: tt.reason, At: fixedTime}

			var nameErr domain.ErroneousChannelNameError
			require.ErrorAs(t, resp.Err, &nameErr)
			require.Equal(t, want, nameErr)
			require.Equal(t, []protocol.Event{want}, resp.Events)

			_, err = sess.loadChannelWindow(ctx, tt.ch)
			require.ErrorIs(t, err, storemod.ErrNoSuchChannel,
				"an erroneous channel name must not be created")
		})
	}
}
