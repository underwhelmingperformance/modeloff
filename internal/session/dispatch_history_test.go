package session

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
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
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, history []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				captures = append(captures, capture{
					history: append([]protocol.IRCMessage(nil), history...),
					events:  append([]protocol.IRCMessage(nil), events...),
				})
				return api.CompletionResult{}, nil
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

// TestModelClient_history_contains_self_replies pins that a bot's
// own outbound messages land in its prompt history on subsequent
// turns. The bus's echo gate suppresses self-delivery of Message
// events (RFC 2812 §3.3.1), so the rolling history buffer needs
// to be fed at send time.
//
// Two user PRIVMSGs drive two dispatch turns. The fake captures
// each turn's history and replies with a stable body each time.
// Turn 1's history is empty; turn 2's history must include the
// bot's turn-1 reply.
func TestModelClient_history_contains_self_replies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var (
			mu       sync.Mutex
			captures [][]protocol.IRCMessage
		)

		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, history []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				mu.Lock()
				captures = append(captures, append([]protocol.IRCMessage(nil), history...))
				mu.Unlock()

				return msgToolCalls(t, domain.ChannelName(events[0].Target), "bot reply"), nil
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

		_, err := userSendMessage(ctx, t, sess, "#room", "first")
		require.NoError(t, err)
		synctest.Wait()

		_, err = userSendMessage(ctx, t, sess, "#room", "second")
		require.NoError(t, err)
		synctest.Wait()

		userTrigger1 := protocol.IRCMessage{
			Kind:   protocol.KindPrivMsg,
			From:   string(userNick(t, sess)),
			Target: "#room",
			Body:   "first",
			At:     fixedTime,
		}
		bottyReply1 := protocol.IRCMessage{
			Kind:       protocol.KindPrivMsg,
			From:       "botty",
			InstanceID: botty.ID(),
			Target:     "#room",
			Body:       "bot reply",
			At:         fixedTime,
		}

		require.Equal(t, [][]protocol.IRCMessage{
			nil,
			{userTrigger1, bottyReply1},
		}, captures,
			"turn 2's history carries botty's turn-1 reply; the model has "+
				"its own utterance available as ongoing context")
	})
}

// TestModelClient_reply_is_gated_by_channel_modes pins that a
// model's dispatch reply crosses `checkSendGates`. botty sits in a
// `+m` channel with no voice; the API stub returns a Reply; the
// gate rejects it and the event log carries only the user's
// trigger.
func TestModelClient_reply_is_gated_by_channel_modes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				return msgToolCalls(t, domain.ChannelName(events[0].Target), "i should be silenced"), nil
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
