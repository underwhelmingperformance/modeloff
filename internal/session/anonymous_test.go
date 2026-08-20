package session

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// TestSession_quit_on_an_anonymous_channel_arrives_as_a_part covers
// RFC 2811 §4.2.1: on a `+a` channel the server sends a PART where
// it would otherwise send a QUIT, so a member sees somebody leave
// the channel and cannot tell that they left the server. The
// departing nick is the `+a` mask, the same origin every message in
// the channel already carried.
func TestSession_quit_on_an_anonymous_channel_arrives_as_a_part(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#anon"))
		setChannelModes(t, sess, "#anon", domain.ChannelModes{Anonymous: true})

		botty, _ := seedPassiveInstance(t, sess, "botty", "test/model")
		require.NoError(t, joinAs(ctx, sess, botty, "#anon", ""))

		collectEmittedEvents(t, sess)

		require.NoError(t, sess.quitAs(ctx, botty, "gone"))
		synctest.Wait()

		require.Equal(t, []domain.Event{domain.Part{
			Target:  "#anon",
			Nick:    domain.AnonymousNick,
			Message: "gone",
			At:      fixedTime,
		}}, collectEmittedEvents(t, sess),
			"the channel is told somebody left it, and not who or that they left the server")
	})
}

// TestSession_quit_reaches_named_channels_unmasked pins the other
// half of the split: a client that shares a `+a` channel and an
// ordinary one with the quitter already knows who it is from the
// ordinary channel, so it receives the QUIT there as well as the
// masked PART on the anonymous one.
func TestSession_quit_reaches_named_channels_unmasked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#anon"))
		require.NoError(t, userJoin(ctx, t, sess, "#open"))
		setChannelModes(t, sess, "#anon", domain.ChannelModes{Anonymous: true})

		botty, _ := seedPassiveInstance(t, sess, "botty", "test/model")
		require.NoError(t, joinAs(ctx, sess, botty, "#anon", ""))
		require.NoError(t, joinAs(ctx, sess, botty, "#open", ""))

		collectEmittedEvents(t, sess)

		require.NoError(t, sess.quitAs(ctx, botty, "gone"))
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.Quit{
				Nick:       "botty",
				InstanceID: botty.ID(),
				Message:    "gone",
				At:         fixedTime,
				Instance:   botty,
			},
			domain.Part{
				Target:  "#anon",
				Nick:    domain.AnonymousNick,
				Message: "gone",
				At:      fixedTime,
			},
		}, collectEmittedEvents(t, sess))
	})
}

// TestSession_dispatch_events_are_masked_on_an_anonymous_channel
// covers the thinking indicator: a dispatch-lifecycle event carries
// the instance handle, which names the client running the turn. On a
// channel where every message is attributed to the mask, that would
// say out loud who is about to speak, so the handle is stripped for
// a recipient that shares only anonymous channels with the actor.
func TestSession_dispatch_events_are_masked_on_an_anonymous_channel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#anon"))
		setChannelModes(t, sess, "#anon", domain.ChannelModes{Anonymous: true})

		botty, _ := seedPassiveInstance(t, sess, "botty", "test/model")
		require.NoError(t, joinAs(ctx, sess, botty, "#anon", ""))

		collectEmittedEvents(t, sess)

		sess.Emit(ctx, domain.ModelDispatchStarted{Instance: botty, At: fixedTime})
		sess.Emit(ctx, domain.ModelDispatchDone{Instance: botty, At: fixedTime})
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.ModelDispatchStarted{At: fixedTime},
			domain.ModelDispatchDone{At: fixedTime},
		}, collectEmittedEvents(t, sess))
	})
}

// TestSession_dispatch_events_name_the_actor_on_a_named_channel
// pins that the masking is scoped to `+a`: an ordinary channel sees
// the handle, which is what the thinking indicator renders from.
func TestSession_dispatch_events_name_the_actor_on_a_named_channel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#open"))

		botty, _ := seedPassiveInstance(t, sess, "botty", "test/model")
		require.NoError(t, joinAs(ctx, sess, botty, "#open", ""))

		collectEmittedEvents(t, sess)

		sess.Emit(ctx, domain.ModelDispatchStarted{Instance: botty, At: fixedTime})
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.ModelDispatchStarted{Instance: botty, At: fixedTime},
		}, collectEmittedEvents(t, sess))
	})
}

// TestSession_join_replay_masks_names_on_an_anonymous_channel
// covers the NAMES half of `+a`. The replay a joiner receives is the
// one path that would hand over the membership verbatim, and it is
// reachable by anyone, because an anonymous channel is joinable: a
// client that wanted the member list would join and read the reply.
// RFC 2811 §4.2.1 answers a NAMES on such a channel with the mask
// alone, privileges included.
func TestSession_join_replay_masks_names_on_an_anonymous_channel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#anon"))
		setChannelModes(t, sess, "#anon", domain.ChannelModes{Anonymous: true})

		botty, joiner := seedPassiveInstance(t, sess, "botty", "test/model")
		drainDeliveries(joiner)

		require.NoError(t, joinAs(ctx, sess, botty, "#anon", ""))
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.Join{
				Target:     "#anon",
				Nick:       "botty",
				InstanceID: botty.ID(),
				At:         fixedTime,
				Instance:   botty,
			},
			domain.NamesReplyEvent{
				Channel: "#anon",
				Members: domain.AnonymousMembers(),
				At:      fixedTime,
			},
			domain.NamesEnd{Channel: "#anon", At: fixedTime},
		}, drainDeliveries(joiner))
	})
}

// TestSession_join_replay_names_the_members_on_a_named_channel pins
// that the masking is scoped to `+a`: an ordinary channel answers
// with its real membership, which is what the joiner's nick list is
// built from.
func TestSession_join_replay_names_the_members_on_a_named_channel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#open"))

		botty, joiner := seedPassiveInstance(t, sess, "botty", "test/model")
		drainDeliveries(joiner)

		require.NoError(t, joinAs(ctx, sess, botty, "#open", ""))
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.Join{
				Target:     "#open",
				Nick:       "botty",
				InstanceID: botty.ID(),
				At:         fixedTime,
				Instance:   botty,
			},
			domain.NamesReplyEvent{
				Channel: "#open",
				Members: testMembers(t, sess, s, "testuser", "botty"),
				At:      fixedTime,
			},
			domain.NamesEnd{Channel: "#open", At: fixedTime},
		}, drainDeliveries(joiner))
	})
}

// TestSession_nick_reaches_a_client_in_no_channels covers RFC 2812
// §3.1.2: a client is always told its own NICK succeeded. The
// broadcast carries it back through the membership filter for a
// client that is on a channel; a client on none has no channel to
// reach it through, so the session delivers it point-to-point.
func TestSession_nick_reaches_a_client_in_no_channels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
		drainDeliveries(client)

		require.NoError(t, sess.changeNickAs(ctx, botty, "renamed"))
		synctest.Wait()

		require.Equal(t, []domain.Event{domain.NickChange{
			OldNick:    "botty",
			NewNick:    "renamed",
			InstanceID: botty.ID(),
			At:         fixedTime,
			Instance:   botty,
		}}, drainDeliveries(client))
	})
}

// TestSession_channel_mode_change_names_its_issuer_by_id covers the
// identity half of a MODE broadcast. A nick is display state a
// client may change, so a recipient reading `By` alone cannot tell
// its own MODE from a peer's after either of them renames;
// `ByInstanceID` is the stable answer, as it already is on KICK and
// INVITE.
func TestSession_channel_mode_change_names_its_issuer_by_id(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#dev"))

		botty, _ := seedPassiveInstance(t, sess, "botty", "test/model")
		require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))

		// Give botty `@` so it can issue a MODE of its own.
		resp, err := userClient(t, sess).Send(ctx, protocol.ChannelMode{
			Channel: "#dev",
			Changes: []protocol.ChannelModeChange{{Flag: domain.ModeOperator, Add: true, Target: "botty"}},
		})
		require.NoError(t, err)
		require.NoError(t, resp.Err)

		collectEmittedEvents(t, sess)

		resp, err = sess.Handle(ctx, sess.LookupClient(protocol.ClientID(botty.ID())), protocol.ChannelMode{
			Channel: "#dev",
			Changes: []protocol.ChannelModeChange{{Flag: domain.ModeModerated, Add: true}},
		})
		require.NoError(t, err)
		require.NoError(t, resp.Err)
		synctest.Wait()

		require.Equal(t, []domain.Event{domain.ChannelModeChange{
			Target:       "#dev",
			Flag:         domain.ModeModerated,
			Add:          true,
			By:           "botty",
			ByInstanceID: botty.ID(),
			At:           fixedTime,
		}}, collectEmittedEvents(t, sess))
	})
}
