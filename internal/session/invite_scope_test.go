package session

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// TestInviteAs_delivery_is_scoped_to_inviter_and_invitee pins the
// RFC 2812 §3.2.7 scope rule: the inviter receives RPL_INVITING
// (in modeloff terms, the [domain.Invited] envelope returned
// in `Response.Events`), the invitee receives the INVITE message
// on its own subscription, and no other channel member is told
// anything. The channel event log is not touched — INVITE is a
// transient notification, not channel chat.
//
// Fixture: the user invites botty to #room. helper is a model
// already in #room (so it would be in the broadcast set under
// the old fan-out shape). The test asserts:
//
//   - The Send response carries the Invited.
//   - botty's dispatch loop fires an INVITE turn — its fake
//     records the trigger, proving the event reached it.
//   - helper's dispatch loop does not fire.
//   - The channel events log has no Invited row.
func TestInviteAs_delivery_is_scoped_to_inviter_and_invitee(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type call struct {
			modelID  domain.ModelID
			triggers []protocol.IRCMessage
		}

		var (
			mu    sync.Mutex
			calls []call
		)

		fake := &apitest.Fake{
			SendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ api.SystemPrompt, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				mu.Lock()
				calls = append(calls, call{
					modelID:  modelID,
					triggers: append([]protocol.IRCMessage(nil), events...),
				})
				mu.Unlock()

				return api.CompletionResult{}, nil
			},
		}

		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedInstance(t, sess, s, instanceSpec{
			Nick:    "botty",
			ModelID: "test/model-botty",
		})
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "helper",
			ModelID:  "test/model-helper",
			Channels: testChannels("#room"),
		})
		seedChannelWithMembers(t, sess, s, "#room", userNick(t, sess), "helper")

		resp, err := userClient(t, sess).Send(ctx, protocol.Invite{Nick: "botty", Channel: "#room"})
		require.NoError(t, err)
		require.NoError(t, resp.Err)

		synctest.Wait()

		wantInviting := domain.Inviting{
			Target: "#room", Invitee: "botty", At: fixedTime,
		}

		require.Equal(t, []protocol.Event{wantInviting}, resp.Events,
			"the Send response carries RPL_INVITING; the target receives INVITE")

		require.Equal(t, []call{
			{
				modelID: "test/model-botty",
				triggers: []protocol.IRCMessage{{
					Kind:   protocol.KindInvite,
					Source: domain.ClientSource(userInstance(t, sess).ID(), userNick(t, sess)),
					Target: "#room",
					At:     fixedTime,
				}},
			},
		}, calls,
			"botty's dispatch fires once with the INVITE trigger (delivery succeeded); "+
				"helper's dispatch does not fire (it is neither inviter nor invitee, so "+
				"per RFC it must not see the invite). The trigger's From + InstanceID "+
				"identify the inviter (the actor); the invitee is implicit (it's the "+
				"receiving model itself).")

		events, err := s.EventsBefore(ctx, "#room", nil, 50)
		require.NoError(t, err)

		var storedInvites []domain.Invited
		for _, se := range events {
			if inv, ok := se.Event.(domain.Invited); ok {
				storedInvites = append(storedInvites, inv)
			}
		}
		require.Empty(t, storedInvites,
			"INVITE is not channel chat; the channel events log should not carry it")
	})
}

func TestInviteAs_destroyed_channel_revokes_the_invitation_turn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		started := make(chan struct{})
		proceed := make(chan struct{})
		fake := &apitest.Fake{
			SendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, api.SystemPrompt, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
				calls.Add(1)
				close(started)
				<-proceed

				return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{{
					ID: "rejoin", Name: "join", Args: []byte(`{"channel":"#room"}`),
				}}}, nil
			},
		}

		sess, store := newTestSessionWithAPI(t, fake)
		ctx := t.Context()
		seedInstance(t, sess, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
		seedChannelWithMembers(t, sess, store, "#room", userNick(t, sess))

		resp, err := userClient(t, sess).Send(ctx, protocol.Invite{Nick: "botty", Channel: "#room"})
		require.NoError(t, err)
		require.NoError(t, resp.Err)
		<-started
		require.NoError(t, userPart(ctx, t, sess, "#room", "done"))
		close(proceed)

		synctest.Wait()

		require.Equal(t, int32(1), calls.Load())
		_, err = sess.loadChannelWindow(ctx, "#room")
		require.Error(t, err)
	})
}

func TestInviteAs_recreated_channel_does_not_revive_an_old_invitation_turn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		started := make(chan struct{})
		proceed := make(chan struct{})
		fake := &apitest.Fake{
			SendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, api.SystemPrompt, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
				if calls.Add(1) != 1 {
					return api.CompletionResult{}, nil
				}

				close(started)
				<-proceed

				return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{{
					ID: "stale-rejoin", Name: "join", Args: []byte(`{"channel":"#room"}`),
				}}}, nil
			},
		}

		sess, store := newTestSessionWithAPI(t, fake)
		ctx := t.Context()
		botty := seedInstance(t, sess, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
		seedChannelWithMembers(t, sess, store, "#room", userNick(t, sess))

		resp, err := userClient(t, sess).Send(ctx, protocol.Invite{Nick: "botty", Channel: "#room"})
		require.NoError(t, err)
		require.NoError(t, resp.Err)
		<-started

		require.NoError(t, userPart(ctx, t, sess, "#room", "done"))
		require.NoError(t, userJoin(ctx, t, sess, "#room"))
		resp, err = userClient(t, sess).Send(ctx, protocol.Invite{Nick: "botty", Channel: "#room"})
		require.NoError(t, err)
		require.NoError(t, resp.Err)
		close(proceed)

		synctest.Wait()

		window, err := sess.loadChannelWindow(ctx, "#room")
		require.NoError(t, err)
		require.False(t, window.Members.HasInstance(botty))
		require.Equal(t, int32(2), calls.Load())
	})
}
