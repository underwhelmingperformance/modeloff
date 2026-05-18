package session

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

// TestChannelDestroy_when_last_member_parts pins RFC 2811 §2: a
// channel ceases to exist when its last occupant leaves. The
// fixture parts the user from a model-less channel and asserts
// the channel window row is gone from the store afterwards.
func TestChannelDestroy_when_last_member_parts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#room"))
		require.NoError(t, sess.partAs(ctx, userInstance(t, sess), "#room", "leaving"))

		_, err := s.GetWindow(ctx, "#room")
		require.ErrorIs(t, err, store.ErrNoSuchChannel,
			"the channel ceases to exist when the last occupant parts")
	})
}

// TestChannelDestroy_when_last_model_quits pins the QUIT path:
// when a model leaves all its channels via `modelQuit`, any
// channel the model was the sole occupant of is destroyed.
func TestChannelDestroy_when_last_model_quits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#bots-only"),
		})
		seedChannelWithMembers(t, sess, s, "#bots-only", botty.Nick())

		require.NoError(t, sess.modelQuit(ctx, botty, "shutting down"))

		_, err := s.GetWindow(ctx, "#bots-only")
		require.ErrorIs(t, err, store.ErrNoSuchChannel,
			"the only model quit; the channel has no occupants and is destroyed")
	})
}

// TestChannelDestroy_keeps_channel_when_user_remains pins the
// converse: parting a model from a channel where the user is
// still present leaves the channel intact.
func TestChannelDestroy_keeps_channel_when_user_remains(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#shared"),
		})
		require.NoError(t, userJoin(ctx, t, sess, "#shared"))
		seedChannelWithMembers(t, sess, s, "#shared", userNick(t, sess), botty.Nick())

		require.NoError(t, sess.partAs(ctx, botty, "#shared", "bbl"))

		ch, err := sess.loadChannelWindow(ctx, "#shared")
		require.NoError(t, err)
		require.True(t, ch.Members.HasInstance(userInstance(t, sess)),
			"the user remained in the channel; the channel persists")
	})
}

// TestChannelDestroy_invite_evaporates pins the channel-mode
// consequence: outstanding entries in `InvitedNicks` disappear
// with the channel. A re-created channel with the same name has
// a fresh state; a previously-invited nick must be re-invited
// or the channel must be `-i` for them to join.
func TestChannelDestroy_invite_evaporates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		// Seed an existing model so we can issue /invite as the user
		// (the inviter must be a channel member, and our user is
		// the creator of #room).
		require.NoError(t, userJoin(ctx, t, sess, "#room"))

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:    "botty",
			ModelID: "test/model",
		})

		// Mark the channel +i so InvitedNicks gates the join. The
		// user is op (creator) and so can invite under +i.
		w, err := sess.loadChannelWindow(ctx, "#room")
		require.NoError(t, err)
		w.Modes.InviteOnly = true
		require.NoError(t, sess.persistChannelWindow(ctx, w))

		_, err = sess.inviteAs(ctx, userInstance(t, sess), botty.Nick(), "#room")
		require.NoError(t, err)

		w, err = sess.loadChannelWindow(ctx, "#room")
		require.NoError(t, err)
		require.True(t, w.InvitedNicks.Contains(botty.Nick()))

		// User parts → channel destroyed → invite evaporates.
		require.NoError(t, sess.partAs(ctx, userInstance(t, sess), "#room", "leaving"))

		_, err = s.GetWindow(ctx, "#room")
		require.ErrorIs(t, err, store.ErrNoSuchChannel)

		// Bot attempts to /join the now-fresh channel. With no
		// invite carried over, the freshly-created channel has no
		// `+i` either — so the join succeeds, but on `+i` semantics
		// the invite would not have helped: there is no
		// `InvitedNicks` because there is no channel-state to
		// carry it.
		require.NoError(t, sess.joinAs(ctx, botty, "#room", ""))

		w, err = sess.loadChannelWindow(ctx, "#room")
		require.NoError(t, err)
		require.False(t, w.Modes.InviteOnly,
			"the re-created channel starts fresh — no +i carried over")
		require.Empty(t, w.InvitedNicks,
			"the re-created channel starts fresh — no invites carried over")

		// Suppress unused if the protocol import drifts.
		_ = protocol.UserClientID
	})
}
