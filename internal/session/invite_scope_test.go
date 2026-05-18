package session

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// TestInviteAs_delivery_is_scoped_to_inviter_and_invitee pins the
// RFC 2812 §3.2.7 scope rule: the inviter receives RPL_INVITING
// (in modeloff terms, the [domain.ModelInvited] envelope returned
// in `Response.Events`), the invitee receives the INVITE message
// on its own subscription, and no other channel member is told
// anything. The channel event log is not touched — INVITE is a
// transient notification, not channel chat.
//
// Fixture: the user invites botty to #room. helper is a model
// already in #room (so it would be in the broadcast set under
// the old fan-out shape). The test asserts:
//
//   - The Send response carries the ModelInvited.
//   - botty's dispatch loop fires an INVITE turn — its fake
//     records the trigger, proving the event reached it.
//   - helper's dispatch loop does not fire.
//   - The channel events log has no ModelInvited row.
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

		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (protocol.ModelResponse, error) {
				mu.Lock()
				calls = append(calls, call{
					modelID:  modelID,
					triggers: append([]protocol.IRCMessage(nil), events...),
				})
				mu.Unlock()

				return protocol.ModelResponse{Kind: protocol.ResponseSilence, Reason: "pass"}, nil
			},
		}

		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
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

		wantInvited := domain.ModelInvited{
			Target:       "#room",
			Nick:         "botty",
			InstanceID:   botty.ID(),
			By:           userNick(t, sess),
			ByInstanceID: "",
			At:           fixedTime,
			Instance:     botty,
		}

		require.Equal(t, []protocol.Event{wantInvited}, resp.Events,
			"the Send response carries the ModelInvited envelope — the inviter's RPL_INVITING")

		require.Equal(t, []call{
			{
				modelID: "test/model-botty",
				triggers: []protocol.IRCMessage{{
					Kind:       protocol.KindInvite,
					From:       string(userNick(t, sess)),
					InstanceID: "",
					Target:     "#room",
					At:         fixedTime,
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

		var storedInvites []domain.ModelInvited
		for _, se := range events {
			if inv, ok := se.Event.(domain.ModelInvited); ok {
				storedInvites = append(storedInvites, inv)
			}
		}
		require.Empty(t, storedInvites,
			"INVITE is not channel chat; the channel events log should not carry it")
	})
}
