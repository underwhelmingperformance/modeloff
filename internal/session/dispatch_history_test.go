package session

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// TestModelClient_dispatch_does_not_show_trigger_in_history_and_events
// pins the rule that a triggering wire event must appear in the LLM
// prompt exactly once. The dispatch loop files every incoming event
// into the model's rolling history buffer; the same event also
// becomes the turn's trigger argument. `buildMessages` lays history
// and trigger events into the chat completion request one after the
// other, so if the snapshot the turn reads still contains the
// trigger, the model sees the same line twice.
//
// The reproducer drives a single user PRIVMSG into a channel that
// has one model member and asserts the captured `(history, events)`
// pair contains the message in `events` only.
func TestModelClient_dispatch_does_not_show_trigger_in_history_and_events(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type capture struct {
			history []protocol.IRCMessage
			events  []protocol.IRCMessage
		}

		var captures []capture

		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, history []protocol.IRCMessage, events []protocol.IRCMessage) (protocol.ModelResponse, error) {
				captures = append(captures, capture{
					history: append([]protocol.IRCMessage(nil), history...),
					events:  append([]protocol.IRCMessage(nil), events...),
				})
				return protocol.ModelResponse{Kind: protocol.ResponseSilence, Reason: "test"}, nil
			},
		}

		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#room"),
		})
		seedChannelWithMembers(t, sess, s, "#room", userNick(t, sess), botty.Nick())

		_, err := userSendMessage(ctx, t, sess, "#room", "hello bot")
		require.NoError(t, err)

		synctest.Wait()

		trigger := protocol.IRCMessage{
			Kind:   protocol.KindPrivMsg,
			From:   string(userNick(t, sess)),
			Target: "#room",
			Body:   "hello bot",
			At:     fixedTime,
		}

		require.Equal(t, []capture{{
			history: nil,
			events:  []protocol.IRCMessage{trigger},
		}}, captures,
			"the trigger goes only in the events argument; placing it in history too "+
				"surfaces the same line twice in the LLM prompt because buildMessages "+
				"appends history then events")
	})
}
