package session

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

func storedReplyEvents(replies []domain.StoredEvent) []domain.PersistableEvent {
	events := make([]domain.PersistableEvent, len(replies))
	for i, r := range replies {
		events[i] = r.Event
	}
	return events
}

// TestSession_whois_persists_to_issuer_reply_log proves both actors
// write their WHOIS reply to their own private reply log, keyed by
// identity, and that the dispatcher stamps the reply's `Target` with
// the window the command was issued in. The model's reply is its
// durable memory of the lookup; the user's (keyed by the empty id) is
// the durable record a future restore would read.
func TestSession_whois_persists_to_issuer_reply_log(t *testing.T) {
	sess, store := newTestSession(t)
	ctx := t.Context()

	seedInstance(t, sess, store, instanceSpec{Nick: "target", ModelID: "test/model"})

	inst := domain.NewModelInstance("inst-asker", "asker", "test/model", "", nil)
	model := newPlainClient(protocol.ClientID(inst.ID()))
	_, err := sess.Subscribe(model, protocol.SubscribeOptions{Instance: inst})
	require.NoError(t, err)

	resp, err := sess.Handle(ctx, model, protocol.Whois{Nick: "target", Channel: "#dev"})
	require.NoError(t, err)
	require.NoError(t, resp.Err)
	require.Equal(t, []domain.ProtocolEvent{domain.Whois{
		Target:  "#dev",
		Nick:    "target",
		ModelID: "test/model",
		At:      fixedTime,
	}}, resp.Events)

	replies, err := store.InstanceRepliesBefore(ctx, inst.ID(), nil, 10)
	require.NoError(t, err)
	require.Equal(t, []domain.PersistableEvent{domain.Whois{
		Target:  "#dev",
		Nick:    "target",
		ModelID: "test/model",
		At:      fixedTime,
	}}, storedReplyEvents(replies))

	userResp, err := sess.Handle(ctx, userClient(t, sess), protocol.Whois{Nick: "target", Channel: "#ops"})
	require.NoError(t, err)
	require.NoError(t, userResp.Err)

	userReplies, err := store.InstanceRepliesBefore(ctx, "", nil, 10)
	require.NoError(t, err)
	require.Equal(t, []domain.PersistableEvent{domain.Whois{
		Target:  "#ops",
		Nick:    "target",
		ModelID: "test/model",
		At:      fixedTime,
	}}, storedReplyEvents(userReplies))
}

// TestSession_list_persists_to_model_issuer proves a model's LIST
// reply — every directory row plus the closing end marker — lands in
// its private reply log, not the shared channel log.
func TestSession_list_persists_to_model_issuer(t *testing.T) {
	sess, store := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, userJoin(ctx, t, sess, "#dev"))

	inst := domain.NewModelInstance("inst-asker", "asker", "test/model", "", nil)
	model := newPlainClient(protocol.ClientID(inst.ID()))
	_, err := sess.Subscribe(model, protocol.SubscribeOptions{Instance: inst})
	require.NoError(t, err)

	resp, err := sess.Handle(ctx, model, protocol.List{})
	require.NoError(t, err)
	require.NoError(t, resp.Err)

	replies, err := store.InstanceRepliesBefore(ctx, inst.ID(), nil, 10)
	require.NoError(t, err)
	require.Equal(t, []domain.PersistableEvent{
		domain.ListReply{Channel: "#dev", Members: 0, At: fixedTime},
	}, storedReplyEvents(replies))

	// The reply is private: it never reaches the shared channel log.
	require.Equal(t, []string{"join"}, channelEventTypes(t, store, "#dev"))
}

// TestSession_user_replies_do_not_pollute_channel_log proves that a
// user's point-to-point command replies (WHOIS, LIST) never reach the
// shared channel event log, even when issued from inside a channel.
// Only genuine channel activity belongs there.
func TestSession_user_replies_do_not_pollute_channel_log(t *testing.T) {
	sess, store := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, userJoin(ctx, t, sess, "#dev"))
	seedInstance(t, sess, store, instanceSpec{Nick: "target", ModelID: "test/model"})

	user := userClient(t, sess)

	whoisResp, err := sess.Handle(ctx, user, protocol.Whois{Nick: "target", Channel: "#dev"})
	require.NoError(t, err)
	require.NoError(t, whoisResp.Err)

	listResp, err := sess.Handle(ctx, user, protocol.List{})
	require.NoError(t, err)
	require.NoError(t, listResp.Err)

	require.Equal(t, []string{"join"}, channelEventTypes(t, store, "#dev"))
}

// TestSession_dispatch_replays_instance_replies_into_prompt proves a
// model re-experiences its own earlier reply: a WHOIS it ran before
// reappears in its prompt transcript on a later dispatch, as if its
// quit never happened.
func TestSession_dispatch_replays_instance_replies_into_prompt(t *testing.T) {
	var sawWhois bool
	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, history []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
			for _, h := range history {
				if h.Kind == protocol.KindServerReply && strings.Contains(h.Body, "whois target") {
					sawWhois = true
				}
			}
			return msgToolCalls(t, domain.ChannelName(events[0].Target), "ok"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	botty := seedInstance(t, sess, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

	// botty looked up "target" earlier; that reply is its own memory.
	_, err := s.AppendInstanceReply(ctx, botty.ID(), domain.Whois{
		Nick:    "target",
		ModelID: "test/model",
		At:      fixedTime,
	})
	require.NoError(t, err)

	_, ircMsg := seedUserMessage(t, s, "#general", "hi")
	require.NoError(t, dispatchToChannel(ctx, sess, "#general", []protocol.IRCMessage{ircMsg}))

	require.True(t, sawWhois, "botty's own whois reply should re-appear in its prompt transcript")
}
