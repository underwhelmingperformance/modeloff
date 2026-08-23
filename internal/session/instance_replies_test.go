package session

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
)

func storedReplyEvents(replies []storemod.InstanceReplyRecord) []domain.PersistableEvent {
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

	inst := seedInstanceRow(t, store, instanceSpec{
		Nick: "asker", ModelID: "test/model", Channels: testChannels("#dev"),
	})
	seedChannelWithMembers(t, sess, store, "#dev", "asker")
	require.NoError(t, userJoin(ctx, t, sess, "#ops"))
	model := newPlainClient(protocol.ClientID(inst.ID()))
	_, err := subscribeTestClient(t.Context(), t, sess, model, protocol.SubscribeOptions{})
	require.NoError(t, err)

	resp, err := sess.Handle(ctx, model, protocol.Whois{Nick: "target", Window: protocol.ChannelWindowTarget("#dev")})
	require.NoError(t, err)
	require.NoError(t, resp.Err)
	require.Equal(t, []domain.ProtocolEvent{domain.Whois{
		Nick:    "target",
		ModelID: "test/model",
		At:      fixedTime,
	}}, resp.Events)

	replies, err := store.InstanceRepliesBefore(ctx, inst.ID(), nil, 10)
	require.NoError(t, err)
	require.Equal(t, []domain.PersistableEvent{domain.Whois{
		Nick:    "target",
		ModelID: "test/model",
		At:      fixedTime,
	}}, storedReplyEvents(replies))

	userResp, err := sess.Handle(ctx, userClient(t, sess), protocol.Whois{Nick: "target", Window: protocol.ChannelWindowTarget("#ops")})
	require.NoError(t, err)
	require.NoError(t, userResp.Err)

	userReplies, err := store.InstanceRepliesBefore(ctx, "", nil, 10)
	require.NoError(t, err)
	require.Equal(t, []domain.PersistableEvent{domain.Whois{
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

	inst := seedInstanceRow(t, store, instanceSpec{
		Nick: "asker", ModelID: "test/model", Channels: testChannels("#dev"),
	})
	seedChannelWithMembers(t, sess, store, "#dev", "testuser", "asker")
	model := newPlainClient(protocol.ClientID(inst.ID()))
	_, err := subscribeTestClient(t.Context(), t, sess, model, protocol.SubscribeOptions{})
	require.NoError(t, err)

	resp, err := sess.Handle(ctx, model, protocol.List{Window: protocol.ChannelWindowTarget("#dev")})
	require.NoError(t, err)
	require.NoError(t, resp.Err)

	replies, err := store.InstanceRepliesBefore(ctx, inst.ID(), nil, 10)
	require.NoError(t, err)
	require.Equal(t, []storemod.InstanceReplyRecord{{
		ID:     1,
		Window: protocol.ChannelWindowTarget("#dev"),
		Event:  domain.ListReply{Channel: "#dev", Members: 2, At: fixedTime},
	}}, replies)

	// The reply is private: it never reaches the shared channel log.
	require.Equal(t, []string{"join"}, channelEventTypes(t, store, "#dev"))
}

func TestSession_reply_commands_refuse_a_closed_issuing_window(t *testing.T) {
	tests := []struct {
		name    string
		command protocol.Command
	}{
		{name: "whois", command: protocol.Whois{
			Nick: "target", Window: protocol.ChannelWindowTarget("#dev"),
		}},
		{name: "list", command: protocol.List{
			Window: protocol.ChannelWindowTarget("#dev"),
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess, eventStore := newTestSession(t)
			ctx := t.Context()
			asker := seedInstanceRow(t, eventStore, instanceSpec{
				Nick: "asker", ModelID: "test/model", Channels: testChannels("#dev"),
			})
			seedInstance(t, sess, eventStore, instanceSpec{Nick: "target", ModelID: "test/model"})
			seedChannelWithMembers(t, sess, eventStore, "#dev", "testuser", "asker")

			client := newPlainClient(protocol.ClientID(asker.ID()))
			_, err := subscribeTestClient(ctx, t, sess, client, protocol.SubscribeOptions{})
			require.NoError(t, err)
			partResponse, err := sess.Handle(ctx, client, protocol.Part{Channel: "#dev"})
			require.NoError(t, err)
			require.Equal(t, protocol.Response{}, partResponse)

			response, err := sess.Handle(ctx, client, tt.command)
			require.NoError(t, err)
			replies, repliesErr := eventStore.InstanceRepliesBefore(ctx, asker.ID(), nil, 10)

			require.Equal(t, struct {
				Response     protocol.Response
				Replies      []storemod.InstanceReplyRecord
				RepliesError error
			}{
				Response: protocol.Response{Err: domain.NotOnChannelError{
					Channel: "#dev", Command: tt.command.Name(), At: fixedTime,
				}},
			}, struct {
				Response     protocol.Response
				Replies      []storemod.InstanceReplyRecord
				RepliesError error
			}{
				Response: response, Replies: replies, RepliesError: repliesErr,
			})
		})
	}
}

// TestSession_failed_invite_persists_notice_to_issuer proves that a
// refused INVITE returns its typed error and files its
// [domain.SystemNotice] to the issuer's private reply log, so a model
// re-experiences the refusal on replay. The notice never reaches the
// shared channel log.
func TestSession_failed_invite_persists_notice_to_issuer(t *testing.T) {
	sess, store := newTestSession(t)
	ctx := t.Context()

	inst := seedInstanceRow(t, store, instanceSpec{Nick: "asker", ModelID: "test/model"})
	seedChannelWithMembers(t, sess, store, "#Dev", "testuser", "asker")

	model := newPlainClient(protocol.ClientID(inst.ID()))
	_, err := subscribeTestClient(t.Context(), t, sess, model, protocol.SubscribeOptions{})
	require.NoError(t, err)

	resp, err := sess.Handle(ctx, model, protocol.Invite{Nick: "ghost", Channel: "#DEV"})
	require.NoError(t, err)
	require.Equal(t, protocol.Response{
		Events: []protocol.Event{domain.SystemNotice{
			Target: "#Dev", Text: "no such nick: ghost", At: fixedTime,
		}},
		Err: domain.UnknownNickError{Nick: "ghost", At: fixedTime},
	}, resp)

	replies, err := store.InstanceRepliesBefore(ctx, inst.ID(), nil, 10)
	require.NoError(t, err)
	require.Equal(t, []domain.PersistableEvent{domain.SystemNotice{
		Target: "#Dev",
		Text:   "no such nick: ghost",
		At:     fixedTime,
	}}, storedReplyEvents(replies))

	resp, err = sess.Handle(ctx, model, protocol.Part{Channel: "#dev"})
	require.NoError(t, err)
	require.NoError(t, resp.Err)
	replies, err = store.InstanceRepliesBefore(ctx, inst.ID(), nil, 10)
	require.NoError(t, err)
	require.Empty(t, replies)
}

func TestSession_cross_channel_invite_persists_the_confirmation_in_the_issuing_window(t *testing.T) {
	sess, store := newTestSession(t)
	ctx := t.Context()

	inst := seedInstanceRow(t, store, instanceSpec{
		Nick: "asker", ModelID: "test/model", Channels: testChannels("#other", "#dev"),
	})
	target := seedInstance(t, sess, store, instanceSpec{Nick: "target", ModelID: "test/model"})
	seedChannelWithMembers(t, sess, store, "#other", "testuser", "asker")
	seedChannelWithMembers(t, sess, store, "#dev", "testuser", "asker")

	model := newPlainClient(protocol.ClientID(inst.ID()))
	_, err := subscribeTestClient(t.Context(), t, sess, model, protocol.SubscribeOptions{})
	require.NoError(t, err)

	resp, err := sess.Handle(ctx, model, protocol.Invite{
		Nick: "target", Channel: "#dev",
		Window: protocol.ChannelWindowTarget("#other"),
	})
	require.NoError(t, err)
	require.NoError(t, resp.Err)

	replies, err := store.InstanceRepliesBefore(ctx, inst.ID(), nil, 10)
	require.NoError(t, err)
	window, windowErr := store.GetWindow(ctx, "#dev")
	channel, ok := window.(*domain.ChannelWindow)
	require.Equal(t, struct {
		Response    protocol.Response
		Replies     []storemod.InstanceReplyRecord
		WindowError error
		Invited     bool
		ChannelOK   bool
	}{
		Response: protocol.Response{Events: []protocol.Event{domain.Inviting{
			Target: "#dev", Invitee: "target", At: fixedTime,
		}}},
		Replies: []storemod.InstanceReplyRecord{{
			ID:     1,
			Window: protocol.ChannelWindowTarget("#other"),
			Event: domain.Inviting{
				Target: "#dev", Invitee: "target", At: fixedTime,
			},
		}},
		Invited:   true,
		ChannelOK: true,
	}, struct {
		Response    protocol.Response
		Replies     []storemod.InstanceReplyRecord
		WindowError error
		Invited     bool
		ChannelOK   bool
	}{
		Response:    resp,
		Replies:     replies,
		WindowError: windowErr,
		Invited:     ok && channel.Invitations.Contains(target.ID()),
		ChannelOK:   ok,
	})
}

func TestSession_invite_refuses_a_foreign_issuing_window(t *testing.T) {
	sess, eventStore := newTestSession(t)
	ctx := t.Context()

	asker := seedInstanceRow(t, eventStore, instanceSpec{
		Nick: "asker", ModelID: "test/model", Channels: testChannels("#dev"),
	})
	target := seedInstance(t, sess, eventStore, instanceSpec{
		Nick: "target", ModelID: "test/model",
	})
	seedChannelWithMembers(t, sess, eventStore, "#dev", "testuser", "asker")
	seedChannelWithMembers(t, sess, eventStore, "#other", "testuser")

	client := newPlainClient(protocol.ClientID(asker.ID()))
	_, err := subscribeTestClient(ctx, t, sess, client, protocol.SubscribeOptions{})
	require.NoError(t, err)

	response, err := sess.Handle(ctx, client, protocol.Invite{
		Nick: "target", Channel: "#dev",
		Window: protocol.ChannelWindowTarget("#other"),
	})
	require.NoError(t, err)
	replies, repliesErr := eventStore.InstanceRepliesBefore(ctx, asker.ID(), nil, 10)
	window, windowErr := eventStore.GetWindow(ctx, "#dev")
	channel, channelOK := window.(*domain.ChannelWindow)

	require.Equal(t, struct {
		Response     protocol.Response
		Replies      []storemod.InstanceReplyRecord
		RepliesError error
		WindowError  error
		ChannelOK    bool
		Invited      bool
	}{
		Response: protocol.Response{Err: domain.NotOnChannelError{
			Channel: "#other", Command: "INVITE", At: fixedTime,
		}},
		ChannelOK: true,
	}, struct {
		Response     protocol.Response
		Replies      []storemod.InstanceReplyRecord
		RepliesError error
		WindowError  error
		ChannelOK    bool
		Invited      bool
	}{
		Response:     response,
		Replies:      replies,
		RepliesError: repliesErr,
		WindowError:  windowErr,
		ChannelOK:    channelOK,
		Invited:      channelOK && channel.Invitations.Contains(target.ID()),
	})
}

func TestSession_part_removes_only_the_closed_window_replies(t *testing.T) {
	sess, eventStore := newTestSession(t)
	ctx := t.Context()

	asker := seedInstance(t, sess, eventStore, instanceSpec{
		Nick:     "asker",
		ModelID:  "test/model",
		Channels: testChannels("#dev", "#other"),
	})
	seedChannelWithMembers(t, sess, eventStore, "#dev", "testuser", "asker")
	seedChannelWithMembers(t, sess, eventStore, "#other", "testuser", "asker")

	for _, reply := range []struct {
		Window protocol.WindowTarget
		Event  domain.IssuerReply
	}{
		{Window: protocol.ChannelWindowTarget("#dev"), Event: domain.TopicInfo{Target: "#dev", Topic: "private topic", At: fixedTime}},
		{Window: protocol.ChannelWindowTarget("#other"), Event: domain.TopicInfo{Target: "#other", Topic: "other topic", At: fixedTime}},
		{Window: protocol.ChannelWindowTarget("#dev"), Event: domain.ListReply{Channel: "#other", Members: 2, At: fixedTime}},
		{Window: protocol.ChannelWindowTarget("#other"), Event: domain.ListReply{Channel: "#dev", Members: 2, At: fixedTime}},
		{Event: domain.Whois{Nick: "target", ModelID: "test/model", At: fixedTime}},
	} {
		_, err := eventStore.AppendInstanceReply(ctx, asker.ID(), reply.Window, reply.Event)
		require.NoError(t, err)
	}

	require.NoError(t, sess.partAs(ctx, asker, "#dev", "leaving"))

	replies, err := eventStore.InstanceRepliesBefore(ctx, asker.ID(), nil, 10)
	require.NoError(t, err)
	require.Equal(t, []domain.PersistableEvent{
		domain.TopicInfo{Target: "#other", Topic: "other topic", At: fixedTime},
		domain.ListReply{Channel: "#dev", Members: 2, At: fixedTime},
		domain.Whois{Nick: "target", ModelID: "test/model", At: fixedTime},
	}, storedReplyEvents(replies))

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

	whoisResp, err := sess.Handle(ctx, user, protocol.Whois{Nick: "target", Window: protocol.ChannelWindowTarget("#dev")})
	require.NoError(t, err)
	require.NoError(t, whoisResp.Err)

	listResp, err := sess.Handle(ctx, user, protocol.List{Window: protocol.ChannelWindowTarget("#dev")})
	require.NoError(t, err)
	require.NoError(t, listResp.Err)

	require.Equal(t, []string{"join"}, channelEventTypes(t, store, "#dev"))
}

// TestSession_dispatch_replays_instance_replies_into_prompt proves a
// model re-experiences its own earlier reply: a WHOIS it ran before
// reappears in its prompt transcript on a later dispatch, as if its
// quit never happened.
func TestSession_dispatch_replays_instance_replies_into_prompt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var sawWhois bool
		fake := &apitest.Fake{
			SendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ api.SystemPrompt, history []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				for _, h := range history {
					if h.Kind == protocol.KindServerReply && h.Body == "whois target: test/model" {
						sawWhois = true
					}
				}
				return msgToolCalls(t, domain.ChannelName(events[0].Target), "ok"), nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		// botty looked up "target" in an earlier connection; that reply
		// is its own memory, and the write lands before the attach that
		// restores it.
		_, err := s.AppendInstanceReply(ctx, testMemberID("botty"), protocol.ChannelWindowTarget("#general"), domain.Whois{
			Nick:    "target",
			ModelID: "test/model",
			At:      fixedTime,
		})
		require.NoError(t, err)

		botty := seedInstanceRow(t, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
		attachModelClient(t, sess, botty)

		dispatchUserMessage(ctx, t, sess, "#general", "hi")

		require.True(t, sawWhois, "botty's own whois reply should re-appear in its prompt transcript")
	})
}
