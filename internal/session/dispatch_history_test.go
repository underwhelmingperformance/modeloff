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

// TestModelClient_reply_is_gated_by_channel_modes pins that a model's
// dispatch reply is routed through the same gated send path as a
// user-issued PRIVMSG. Today buildReplies persists Messages by
// calling Session.AppendEvent directly, which skips checkSendGates
// and silently lets a `+m`-without-voice reply through. The
// reproducer puts botty into a moderated channel with no voice,
// drives a dispatch turn whose API stub returns a Reply, and asserts
// the reply does NOT land in the event log.
func TestModelClient_reply_is_gated_by_channel_modes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (protocol.ModelResponse, error) {
				return protocol.Reply("i should be silenced"), nil
			},
		}

		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#chan"),
		})
		seedChannelWithMembers(t, sess, s, "#chan", userNick(t, sess), botty.Nick())

		w, err := sess.loadChannelWindow(ctx, "#chan")
		require.NoError(t, err)
		w.Modes.Moderated = true
		w.Members.SetMode(botty, domain.ModeNone)
		require.NoError(t, sess.persistChannelWindow(ctx, w))

		userMsg, err := userSendMessage(ctx, t, sess, "#chan", "speak up")
		require.NoError(t, err)

		synctest.Wait()

		require.Equal(t, []domain.Message{userMsg}, channelMessages(t, s, "#chan"),
			"a +m channel with no voice must reject botty's reply; the only persisted "+
				"message should be the user's trigger")
	})
}
