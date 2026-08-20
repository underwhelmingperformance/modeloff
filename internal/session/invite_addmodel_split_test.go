package session

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
)

// TestInviteAs_does_not_auto_attach_existing_model pins the design
// of `/invite` for already-attached models: emit `Invited` as
// a wire notification (and a `SystemNotice` for the channel log)
// while leaving channel membership untouched. The invited model
// takes a turn on the INVITE and chooses whether to issue its own
// `/join`, matching RFC 2812 §3.2.7's "you may now /join"
// semantics.
//
// botty is invited to `#random`. The fake passes on the INVITE
// turn. Afterwards: botty's `Channels` set carries only its
// existing memberships, the channel's member list does not carry
// botty, and the bus has carried exactly one `Invited` plus
// botty's dispatch lifecycle.
func TestInviteAs_does_not_auto_attach_existing_model(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()

		fake := &apitest.Fake{
			SendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
				return api.CompletionResult{}, nil
			},
		}

		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", userNick(t, sess), "botty")
		seedChannelWithMembers(t, sess, s, "#random", userNick(t, sess))

		event, err := sess.inviteAs(ctx, userInstance(t, sess), "botty", "#random")
		require.NoError(t, err)
		require.Equal(t, domain.Invited{
			Target:       "#random",
			Nick:         "botty",
			InstanceID:   botty.ID(),
			By:           userNick(t, sess),
			ByInstanceID: "",
			At:           fixedTime,
			Instance:     botty,
		}, event,
			"inviteAs returns the Invited envelope as the inviter's "+
				"RPL_INVITING-equivalent — the handler wraps it into Response.Events")

		synctest.Wait()

		// INVITE is scoped to inviter + invitee (RFC 2812 §3.2.7).
		// The user-client bus carries only botty's dispatch
		// lifecycle: the invite itself does not broadcast.
		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.ModelDispatchStarted{Instance: botty, At: fixedTime},
			domain.ModelDispatchDone{Instance: botty, At: fixedTime},
		}, collectEmittedEvents(t, sess))

		requireChannels(t, botty.Channels(), "#general")

		channel, err := sess.loadChannelWindow(ctx, "#random")
		require.NoError(t, err)
		require.False(t, channel.Members.HasInstance(botty),
			"INVITE must not add the invited model to the channel's member list — "+
				"the model decides whether to /join")
	})
}

// TestAddModel_emits_real_Join pins the wire shape of
// `/add-model`: the new instance is created, attached, and joined
// to the target channel via `joinAs`. The bus carries a `Join`
// event with the same shape any `/join` would produce, and the
// channel's member list contains the new instance afterwards.
func TestAddModel_emits_real_Join(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", userNick(t, sess))

		require.NoError(t, addModelViaWire(ctx, t, sess, "#general", "anthropic/claude-3-haiku", "Helpful"))
		synctest.Wait()

		inst, err := s.ResolveNick(ctx, "fakenick")
		require.NoError(t, err)

		events := collectEmittedEvents(t, sess)

		joinIdx := slices.IndexFunc(events, func(e domain.Event) bool {
			j, ok := e.(domain.Join)
			return ok && j.InstanceID == inst.ID() && j.Target == "#general"
		})
		require.GreaterOrEqual(t, joinIdx, 0,
			"the bus should carry a Join event for the new model with the same "+
				"wire shape a /join would produce")

		invitedIdx := slices.IndexFunc(events, func(e domain.Event) bool {
			_, ok := e.(domain.Invited)
			return ok
		})
		require.Less(t, invitedIdx, 0,
			"the add-model path emits Join; Invited belongs to the invite path")

		channel, err := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, err)
		require.True(t, channel.Members.HasInstance(inst),
			"the new instance is a member of the target channel")
	})
}

// TestAddModel_unwinds_a_client_that_could_not_connect pins what
// ADDMODEL leaves behind when the model-client cannot attach. A
// client that never connected can receive nothing, so a member with
// no client behind it is a nick the member list advertises, WHOIS
// answers for, and no message reaches. The command fails instead,
// and the unwind is the one a refused JOIN already runs: no member,
// no instance row, and the nick free for the next attempt.
func TestAddModel_unwinds_a_client_that_could_not_connect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", userNick(t, sess))

		factory, ok := sess.modelClientFactory.(*testModelClientFactory)
		require.True(t, ok)
		factory.attachErr = fmt.Errorf("upstream unavailable")

		err := addModelViaWire(ctx, t, sess, "#general", "anthropic/claude-3-haiku", "Helpful")
		require.ErrorContains(t, err, "upstream unavailable")

		synctest.Wait()

		_, resolveErr := s.ResolveNick(ctx, "fakenick")
		require.ErrorIs(t, resolveErr, storemod.ErrNoSuchNick,
			"the nick is free again, so the next attempt can take it")

		channel, loadErr := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, loadErr)
		require.Equal(t, []domain.Nick{userNick(t, sess)}, memberNicks(channel))
	})
}
