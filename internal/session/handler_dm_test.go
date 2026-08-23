package session

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/protocol"
)

func TestSession_model_channel_tool_is_refused_in_a_DM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var results []api.ToolResult
		fake := &apitest.Fake{
			SendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, api.SystemPrompt, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
				return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{
					{ID: "topic-1", Name: "topic", Args: json.RawMessage(`{}`)},
				}}, nil
			},
			ContinueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, got []api.ToolResult) (api.CompletionResult, error) {
				results = append(results, got...)

				return api.CompletionResult{}, nil
			},
		}

		sess, store := newTestSessionWithAPI(t, fake)
		botty := seedInstance(t, sess, store, instanceSpec{Nick: "botty", ModelID: "test/model"})

		dispatchUserMessage(t.Context(), t, sess, domain.ChannelName(botty.ID()), "you there?")

		type observedToolResult struct {
			ToolCallID   string
			OK           bool
			Summary      string
			Data         any
			ErrorPresent bool
		}
		observed := make([]observedToolResult, 0, len(results))
		for _, result := range results {
			var payload modelclient.ToolResultPayload
			require.NoError(t, json.Unmarshal([]byte(result.Content), &payload))
			observed = append(observed, observedToolResult{
				ToolCallID:   result.ToolCallID,
				OK:           payload.OK,
				Summary:      payload.Summary,
				Data:         payload.Data,
				ErrorPresent: payload.Error != "",
			})
		}
		require.Equal(t, []observedToolResult{{
			ToolCallID:   "topic-1",
			ErrorPresent: true,
		}}, observed)
	})
}

// TestSession_model_action_in_a_DM_reaches_the_counterpart covers a
// model's `/me` inside a DM. The action goes to the window the turn
// is running in, and for a DM that window is the conversation with
// the counterpart, and not the model's own id, which is only how the
// incoming message was addressed. Addressed at its own id the action
// would be reported as delivered and filed where the person it
// answers cannot read it.
func TestSession_model_action_in_a_DM_reaches_the_counterpart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &apitest.Fake{
			SendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, api.SystemPrompt, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
				return meToolCall(t, "waves"), nil
			},
		}

		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})

		dispatchUserMessage(ctx, t, sess, domain.ChannelName(botty.ID()), "you there?")

		require.Equal(t, []domain.Message{
			{
				Source: domain.ClientSource(protocol.UserClientID, "testuser"),
				Target: domain.ChannelName(botty.ID()),
				Body:   "you there?",
				At:     fixedTime,
			},
			{
				Source: domain.ClientSource(botty.ID(), "botty"),
				Target: "",
				Body:   "waves",
				Action: true,
				At:     fixedTime,
			},
		}, dmThreadMessages(t, s, "", botty.ID()))
	})
}

func TestSession_model_DM_reply_keeps_the_projected_peer_identity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		providerStarted := make(chan struct{})
		resumeProvider := make(chan struct{})
		fake := &apitest.Fake{
			SendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, api.SystemPrompt, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
				close(providerStarted)
				<-resumeProvider

				return msgToolCalls(t, "shared", "reply to the original peer"), nil
			},
		}

		sess, dataStore := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		responder := seedInstance(t, sess, dataStore, instanceSpec{
			Nick: "responder", ModelID: "test/responder",
		})
		original := seedInstanceRow(t, dataStore, instanceSpec{
			InstanceID: "original-peer", Nick: "shared", ModelID: "test/peer",
		})
		originalClient := &passiveClient{id: protocol.ClientID(original.ID())}
		originalSub, err := subscribeTestClient(ctx, t, sess, originalClient, protocol.SubscribeOptions{})
		require.NoError(t, err)
		originalClient.sub = originalSub

		replacement := seedInstanceRow(t, dataStore, instanceSpec{
			InstanceID: "replacement-peer", Nick: "replacement", ModelID: "test/peer",
		})
		replacementClient := &passiveClient{id: protocol.ClientID(replacement.ID())}
		replacementSub, err := subscribeTestClient(ctx, t, sess, replacementClient, protocol.SubscribeOptions{})
		require.NoError(t, err)
		replacementClient.sub = replacementSub

		response, err := sess.Handle(ctx, originalClient, protocol.PrivMsg{
			Target: protocol.ClientTarget(responder.ID()),
			Body:   "question for responder",
		})
		require.NoError(t, err)
		require.NoError(t, response.Err)
		<-providerStarted

		response, err = sess.Handle(ctx, originalClient, protocol.Nick{New: "renamed"})
		require.NoError(t, err)
		require.NoError(t, response.Err)
		response, err = sess.Handle(ctx, replacementClient, protocol.Nick{New: "shared"})
		require.NoError(t, err)
		require.NoError(t, response.Err)
		synctest.Wait()
		drainDeliveries(originalClient)
		drainDeliveries(replacementClient)

		close(resumeProvider)
		synctest.Wait()

		reply := domain.Message{
			Source: domain.ClientSource(responder.ID(), "responder"),
			Target: domain.ChannelName(original.ID()),
			Body:   "reply to the original peer",
			At:     fixedTime,
		}
		require.Equal(t, []domain.Event{
			reply,
			domain.ModelDispatchDone{
				Source: domain.ClientSource(responder.ID(), "responder"),
				At:     fixedTime,
			},
		}, drainDeliveries(originalClient))
		require.Empty(t, drainDeliveries(replacementClient))

		require.Equal(t, []domain.Message{
			{
				Source: domain.ClientSource(original.ID(), "shared"),
				Target: domain.ChannelName(responder.ID()),
				Body:   "question for responder",
				At:     fixedTime,
			},
			reply,
		}, dmThreadMessages(t, dataStore, responder.ID(), original.ID()))

		require.Empty(t, dmThreadMessages(t, dataStore, responder.ID(), replacement.ID()))
	})
}

// TestSession_PrivMsg_to_model_routes_DM_to_counterpart_only is the
// capability-parity test for the protocol redesign. Three model
// instances exist; A sends a `protocol.PrivMsg` to B's instance id
// (the wire shape for a DM). The test pins three properties that
// must hold for the redesign to be correct:
//
//  1. Capability parity: model A uses the same `Send → PrivMsg`
//     call as the user-client. There is no model-only or user-only
//     code path.
//  2. Membership filter: only B's dispatch goroutine receives the
//     trigger event and runs an LLM turn for it. C's goroutine —
//     no channel overlap with A or B — does not.
//  3. Echo gate: A's dispatch goroutine does not see its own
//     outbound message, so a chatty model can't trip itself into
//     an echo loop.
//
// The test asserts each model's `SendEventsFn` is or isn't invoked
// for the round, capturing the trigger events to confirm the
// reachability shape. It also asserts the events log persists the
// message addressable by the counterpart's instance id, which is
// the DM's channel name on the wire.
func TestSession_PrivMsg_to_model_routes_DM_to_counterpart_only(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()

		type call struct {
			modelID domain.ModelID
			trigger []protocol.IRCMessage
		}

		var calls []call

		fake := &apitest.Fake{
			SendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ api.SystemPrompt, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				calls = append(calls, call{modelID: modelID, trigger: append([]protocol.IRCMessage(nil), events...)})
				return api.CompletionResult{}, nil
			},
		}

		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		a := seedInstance(t, sess, s, instanceSpec{Nick: "alpha", ModelID: "test/model-a"})
		b := seedInstance(t, sess, s, instanceSpec{Nick: "beta", ModelID: "test/model-b"})
		seedInstance(t, sess, s, instanceSpec{Nick: "gamma", ModelID: "test/model-c"})

		aClient := attachModelClient(t, sess, a)
		require.NotNil(t, aClient, "model client for alpha must exist")

		resp, err := aClient.Send(ctx, protocol.PrivMsg{
			Target: protocol.NickTarget("beta"),
			Body:   "private to beta",
		})
		require.NoError(t, err)
		require.NoError(t, resp.Err)

		// Response.Events carries the canonical persisted message back
		// to the issuing client so the chat-screen renders against the
		// session's clock rather than its own.
		require.Equal(t, []protocol.Event{domain.Message{Source: domain.ClientSource(

			a.ID(), "alpha"), Target: domain.ChannelName(b.ID()), Body: "private to beta", At: fixedTime}}, resp.Events)

		synctest.Wait()

		// The user is not party to the alpha↔beta DM, so the DM round
		// does not reach the user-client; it sees only its bootstrap
		// OPER promotion. Routing is verified below via the dispatch
		// calls and the persisted DM.
		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
		}, collectEmittedEvents(t, sess))

		expectedTrigger := protocol.IRCMessage{
			Kind:   protocol.KindPrivMsg,
			Source: domain.ClientSource(a.ID(), "alpha"),
			Target: string(b.Nick()),
			Body:   "private to beta",
			At:     fixedTime,
		}

		require.Equal(t, []call{{
			modelID: "test/model-b",
			trigger: []protocol.IRCMessage{expectedTrigger},
		}}, calls,
			"only B's dispatch turn should fire; A is suppressed by the echo gate, "+
				"C by the membership filter; the provider receives B's current nick "+
				"after the session routes the DM by B's stable id")

		// The events log carries the message under the DM's channel name
		// (B's instance id). Either party can read the conversation back
		// from this single key — DMs are stateless on the server side.
		persisted := channelMessages(t, s, domain.ChannelName(b.ID()))
		require.Equal(t, []domain.Message{{
			Source: domain.ClientSource(a.ID(), "alpha"),
			Target: domain.ChannelName(b.ID()),
			Body:   "private to beta",
			At:     fixedTime,
		}}, persisted)
	})
}
