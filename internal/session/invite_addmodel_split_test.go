package session

import (
	"context"
	"errors"
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
// of `/invite` for already-attached models: deliver `Invited` to the
// target and return `Inviting` to the issuer while leaving channel
// membership untouched. The invited model
// takes a turn on the INVITE and chooses whether to issue its own
// `/join`, matching RFC 2812 §3.2.7's "you may now /join"
// semantics.
//
// botty is invited to `#random`. The fake passes on the INVITE
// turn. Afterwards: botty's `Channels` set carries only its
// existing memberships, the channel's member list does not carry
// botty, and botty's delivery produces one dispatch lifecycle.
func TestInviteAs_does_not_auto_attach_existing_model(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()

		fake := &apitest.Fake{
			SendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ api.SystemPrompt, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
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
			Source:  domain.ClientSource(userInstance(t, sess).ID(), userNick(t, sess)),
			Target:  "#random",
			Invitee: "botty",
			At:      fixedTime,
		}, event,
			"inviteAs returns the INVITE delivered to the target; the handler constructs RPL_INVITING separately")

		synctest.Wait()

		// INVITE is scoped to inviter + invitee (RFC 2812 §3.2.7).
		// The user-client bus carries only botty's dispatch
		// lifecycle: the invite itself does not broadcast.
		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.ModelDispatchStarted{Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime},
			domain.ModelDispatchDone{Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime},
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
			id, identified := j.Source.InstanceID()
			return ok && identified && id == inst.ID() && j.Target == "#general"
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
		attachErr := errors.New("upstream unavailable")
		factory.attachErr = attachErr

		err := addModelViaWire(ctx, t, sess, "#general", "anthropic/claude-3-haiku", "Helpful")
		require.ErrorIs(t, err, attachErr)

		synctest.Wait()

		_, resolveErr := s.ResolveNick(ctx, "fakenick")
		require.ErrorIs(t, resolveErr, storemod.ErrNoSuchNick,
			"the nick is free again, so the next attempt can take it")

		channel, loadErr := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, loadErr)
		require.Equal(t, []domain.Nick{userNick(t, sess)}, memberNicks(channel))
	})
}

func TestAddModel_does_not_admit_a_client_killed_after_attach(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", userNick(t, sess))

		factory, ok := sess.modelClientFactory.(*testModelClientFactory)
		require.True(t, ok)
		attached := make(chan struct{})
		proceed := make(chan struct{})
		factory.afterAttach = func() {
			close(attached)
			<-proceed
		}

		added := make(chan error, 1)
		go func() {
			added <- addModelViaWire(ctx, t, sess, "#general", "test/model", "Helpful")
		}()

		<-attached
		resp, err := userClient(t, sess).Send(ctx, protocol.Kill{Nick: "fakenick", Reason: "cancelled"})
		require.NoError(t, err)
		require.NoError(t, resp.Err)
		close(proceed)

		require.ErrorIs(t, <-added, protocol.ErrSubscriptionClosed)
		_, err = s.ResolveNick(ctx, "fakenick")
		require.ErrorIs(t, err, storemod.ErrNoSuchNick)

		channel, err := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, err)
		require.Equal(t, []domain.Nick{userNick(t, sess)}, memberNicks(channel))
	})
}

func TestAddModel_does_not_cross_the_issuers_connection_generation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", userNick(t, sess))

		factory, ok := sess.modelClientFactory.(*testModelClientFactory)
		require.True(t, ok)
		attached := make(chan struct{})
		proceed := make(chan struct{})
		factory.afterAttach = func() {
			close(attached)
			<-proceed
		}

		added := make(chan error, 1)
		go func() {
			added <- addModelViaWire(
				ctx, t, sess, "#general", "test/model", "Helpful",
			)
		}()

		<-attached
		require.NoError(t, userQuitViaWire(ctx, t, sess, "reconnecting"))
		synctest.Wait()
		require.NoError(t, sess.Connect(ctx))
		resp, err := userClient(t, sess).Send(ctx, protocol.Join{
			Channels: []domain.ChannelName{"#general"},
		})
		require.NoError(t, err)
		require.NoError(t, resp.Err)
		close(proceed)

		addErr := <-added
		_, resolveErr := s.ResolveNick(ctx, "fakenick")
		channel, channelErr := sess.loadChannelWindow(ctx, "#general")
		type assertionSnapshot struct {
			SubscriptionClosed bool
			ModelAbsent        bool
			ChannelError       error
			Members            []domain.Nick
		}

		require.Equal(t, assertionSnapshot{
			SubscriptionClosed: true,
			ModelAbsent:        true,
			Members:            []domain.Nick{userNick(t, sess)},
		}, assertionSnapshot{
			SubscriptionClosed: errors.Is(addErr, protocol.ErrSubscriptionClosed),
			ModelAbsent:        errors.Is(resolveErr, storemod.ErrNoSuchNick),
			ChannelError:       channelErr,
			Members:            memberNicks(channel),
		})
	})
}

func TestAddModel_does_not_recreate_a_channel_removed_during_attach(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", userNick(t, sess))

		factory, ok := sess.modelClientFactory.(*testModelClientFactory)
		require.True(t, ok)
		attached := make(chan struct{})
		proceed := make(chan struct{})
		factory.afterAttach = func() {
			close(attached)
			<-proceed
		}

		added := make(chan error, 1)
		go func() {
			added <- addModelViaWire(ctx, t, sess, "#general", "test/model", "Helpful")
		}()

		<-attached
		require.NoError(t, userPart(ctx, t, sess, "#general", "leaving"))
		close(proceed)

		var notOnChannel domain.NotOnChannelError
		require.ErrorAs(t, <-added, &notOnChannel)
		require.Equal(t, domain.NotOnChannelError{
			Channel: "#general", Command: "ADDMODEL", At: fixedTime,
		}, notOnChannel)
		_, err := s.GetWindow(ctx, "#general")
		require.ErrorIs(t, err, storemod.ErrNoSuchChannel)
		_, err = s.ResolveNick(ctx, "fakenick")
		require.ErrorIs(t, err, storemod.ErrNoSuchNick)
	})
}
