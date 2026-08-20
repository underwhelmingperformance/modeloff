package session

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// seedPassiveInstance registers a model instance whose subscription
// nothing drains, so a test can read exactly what the session
// delivered to it. `seedInstance` attaches a model-client, whose
// dispatch goroutine would consume the subscription first.
func seedPassiveInstance(t *testing.T, sess *Session, nick domain.Nick, model domain.ModelID) (*domain.Instance, *passiveClient) {
	t.Helper()

	inst := domain.NewModelInstance(testMemberID(nick), nick, model, "", nil)
	require.NoError(t, sess.store.SaveInstance(t.Context(), inst))

	c := &passiveClient{id: protocol.ClientID(inst.ID())}

	sub, err := sess.Subscribe(c, protocol.SubscribeOptions{Instance: inst})
	require.NoError(t, err)
	c.sub = sub

	return inst, c
}

// TestSession_join_replays_names_and_topic_to_the_joiner covers RFC
// 2812 §3.2.1 / §3.2.4: a joining client is sent the member list
// (`RPL_NAMREPLY` / `RPL_ENDOFNAMES`) and the topic (`RPL_TOPIC`),
// point-to-point, right after its own JOIN.
func TestSession_join_replays_names_and_topic_to_the_joiner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#dev"))
		require.NoError(t, sess.setTopicAs(ctx, userInstance(t, sess), "#dev", "ongoing work"))

		botty, joiner := seedPassiveInstance(t, sess, "botty", "test/model")
		drainDeliveries(joiner)

		require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.Join{
				Target:     "#dev",
				Nick:       "botty",
				InstanceID: botty.ID(),
				At:         fixedTime,
				Instance:   botty,
			},
			domain.NamesReplyEvent{
				Channel: "#dev",
				Members: testMembers(t, sess, s, "testuser", "botty"),
				At:      fixedTime,
			},
			domain.NamesEnd{Channel: "#dev", At: fixedTime},
			domain.TopicInfo{
				Target:     "#dev",
				Topic:      "ongoing work",
				TopicSetBy: "testuser",
				TopicSetAt: fixedTime,
				At:         fixedTime,
			},
		}, drainDeliveries(joiner))
	})
}

// TestSession_kick_reaches_the_kicked_client covers RFC 2812 §3.2.8:
// the kicked client is told it was kicked. The KICK is broadcast
// while the target is still a member, which is the order PART
// already follows, because the membership filter is what carries the
// event to it.
func TestSession_kick_reaches_the_kicked_client(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#dev"))

		botty, victim := seedPassiveInstance(t, sess, "botty", "test/model")
		require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))
		drainDeliveries(victim)

		require.NoError(t, kickViaWire(ctx, t, sess, "#dev", "botty"))
		synctest.Wait()

		require.Equal(t, []domain.Event{domain.Kicked{
			Target:     "#dev",
			Nick:       "botty",
			InstanceID: botty.ID(),
			By:         "testuser",
			At:         fixedTime,
			Instance:   botty,
		}}, drainDeliveries(victim))

		window, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		require.False(t, window.Members.HasInstance(botty), "the kick still removes membership")
	})
}

// TestSession_invite_requires_the_inviter_to_be_on_the_channel
// covers RFC 2812 §3.2.7's 442 ERR_NOTONCHANNEL. An invitation is a
// member vouching for someone, so a client that is not on the
// channel has nothing to vouch with, and the channel records
// nothing.
func TestSession_invite_requires_the_inviter_to_be_on_the_channel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#dev"))

		outsider := seedInstance(t, sess, s, instanceSpec{Nick: "outsider", ModelID: "test/model"})
		helper := seedInstance(t, sess, s, instanceSpec{Nick: "helper", ModelID: "test/model-b"})

		_, err := sess.inviteAs(ctx, outsider, "helper", "#dev")
		require.Equal(t, domain.NotOnChannelError{Channel: "#dev", Command: "INVITE", At: fixedTime}, err)

		window, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		require.False(t, window.Invitations.Contains(helper.ID()))
	})
}

// TestSession_invite_records_nothing_for_an_unknown_nick pins that
// the target is resolved before the invitation is written, so an
// unknown nick leaves the channel holding no entry for a client that
// does not exist and that nothing would ever consume or clear.
func TestSession_invite_records_nothing_for_an_unknown_nick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#dev"))

		event, err := sess.inviteAs(ctx, userInstance(t, sess), "ghost", "#dev")
		require.NoError(t, err)
		require.Equal(t, domain.SystemNotice{
			Target: "#dev",
			Text:   "no such nick: ghost",
			At:     fixedTime,
		}, event)

		window, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		require.Empty(t, window.Invitations)
	})
}

// TestSession_invitation_survives_a_rename covers what keying the
// invitation set by instance id buys: a client invited under one
// nick and renamed before it joins is the same client, and the
// invitation it was granted still admits it.
func TestSession_invitation_survives_a_rename(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#dev"))
		setChannelModes(t, sess, "#dev", domain.ChannelModes{InviteOnly: true})

		botty := seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})

		_, err := sess.inviteAs(ctx, userInstance(t, sess), "botty", "#dev")
		require.NoError(t, err)

		require.NoError(t, sess.changeNickAs(ctx, botty, "renamed"))

		require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))

		window, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		require.True(t, window.Members.HasInstance(botty))
	})
}

// TestSession_invitation_is_consumed_by_any_join pins that an
// invitation is single-use whether or not `+i` was set at the time
// it was used. A client that walked in while the channel was open is
// not owed a second entry after `+i` goes up.
func TestSession_invitation_is_consumed_by_any_join(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#dev"))

		botty := seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})

		_, err := sess.inviteAs(ctx, userInstance(t, sess), "botty", "#dev")
		require.NoError(t, err)

		// The channel is `-i`, so the join needs no invitation, but it
		// spends the one that was outstanding.
		require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))
		require.NoError(t, sess.partAs(ctx, botty, "#dev", "back shortly"))

		setChannelModes(t, sess, "#dev", domain.ChannelModes{InviteOnly: true})

		err = joinAs(ctx, sess, botty, "#dev", "")
		require.Equal(t, domain.ChannelInviteOnlyError{Channel: "#dev", At: fixedTime}, err)
	})
}

// TestSession_add_model_admits_past_invite_only covers the one join
// that passes `+i` without an invitation. `ADDMODEL` is
// operator-gated at the dispatcher, so the authority the operator
// already exercised is what admits the new client, and the channel's
// invitation list is left holding only invitations somebody actually
// issued.
func TestSession_add_model_admits_past_invite_only(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#dev"))
		setChannelModes(t, sess, "#dev", domain.ChannelModes{InviteOnly: true})

		require.NoError(t, addModelViaWire(ctx, t, sess, "#dev", "test/model", ""))
		synctest.Wait()

		window, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		require.Equal(t, []domain.Nick{userNick(t, sess), "fakenick"}, memberNicks(window))
		require.Empty(t, window.Invitations,
			"ADDMODEL leaves no invitation behind, because it wrote none")
	})
}

// TestSession_join_is_refused_by_invite_only_for_an_operator pins
// the other side of that rule: `+i` is admission control over who
// may be in the channel, not a privilege among the members, so a
// client's own JOIN is refused even when it holds server-operator
// mode. The user-client holds `+o` from bootstrap.
func TestSession_join_is_refused_by_invite_only_for_an_operator(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		// A channel the user is not on, held open by a model.
		botty := seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
		require.NoError(t, joinAs(ctx, sess, botty, "#closed", ""))
		setChannelModes(t, sess, "#closed", domain.ChannelModes{InviteOnly: true})

		err := userJoin(ctx, t, sess, "#closed")
		require.Equal(t, domain.ChannelInviteOnlyError{Channel: "#closed", At: fixedTime}, err)
	})
}
