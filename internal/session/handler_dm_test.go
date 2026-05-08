package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

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
// The test asserts each model's `sendEventsFn` is or isn't invoked
// for the round, capturing the trigger events to confirm the
// reachability shape. It also asserts the events log persists the
// message addressable by the counterpart's instance id, which is
// the DM's channel name on the wire.
func TestSession_PrivMsg_to_model_routes_DM_to_counterpart_only(t *testing.T) {
	type call struct {
		modelID domain.ModelID
		trigger []protocol.IRCMessage
	}

	var calls []call

	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (protocol.ModelResponse, error) {
			calls = append(calls, call{modelID: modelID, trigger: append([]protocol.IRCMessage(nil), events...)})
			return protocol.ModelResponse{Kind: protocol.ResponseSilence, Reason: "test"}, nil
		},
	}

	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	a := seedInstance(t, sess, s, instanceSpec{Nick: "alpha", ModelID: "test/model-a"})
	b := seedInstance(t, sess, s, instanceSpec{Nick: "beta", ModelID: "test/model-b"})
	seedInstance(t, sess, s, instanceSpec{Nick: "gamma", ModelID: "test/model-c"})

	aClient := sess.Model(ctx, protocol.ClientID(a.ID()))
	require.NotNil(t, aClient, "model client for alpha must exist")

	resp, err := aClient.Send(ctx, protocol.PrivMsg{
		Target: domain.ChannelName(b.ID()),
		Body:   "private to beta",
	})
	require.NoError(t, err)
	require.NoError(t, resp.Err)

	// Response.Events carries the canonical persisted message back
	// to the issuing client so the chat-screen renders against the
	// session's clock rather than its own.
	require.Equal(t, []protocol.Event{domain.Message{
		Target:     domain.ChannelName(b.ID()),
		From:       "alpha",
		InstanceID: a.ID(),
		Body:       "private to beta",
		At:         fixedTime,
	}}, resp.Events)

	// Drain through `DispatchDoneEvent` so B's dispatch goroutine
	// has returned before we read `calls`. The channel-send chain
	// from the goroutine to this drain provides the happens-before
	// edge: the closure's append to `calls` completes before
	// `DispatchDoneEvent` is emitted.
	//
	// Three events on the user-client's bus complete the round:
	// the original `Message` (the user-client sees every protocol
	// event), then `DispatchStartedEvent`, then `DispatchDoneEvent`.
	// `extras` empty pins the no-other-events invariant — A and C
	// never run a turn for this message.
	_, extras := drainUntilMatched(t, sess,
		matchEvent[domain.Message](),
		matchEvent[domain.DispatchStartedEvent](),
		matchEvent[domain.DispatchDoneEvent](),
	)
	require.Empty(t, extras, "no other events should fire during a single PrivMsg→DM round")

	expectedTrigger := protocol.IRCMessage{
		Kind:       protocol.KindPrivMsg,
		From:       "alpha",
		InstanceID: a.ID(),
		Target:     string(b.ID()),
		Body:       "private to beta",
		At:         fixedTime,
	}

	require.Equal(t, []call{{
		modelID: "test/model-b",
		trigger: []protocol.IRCMessage{expectedTrigger},
	}}, calls,
		"only B's dispatch turn should fire; A is suppressed by the echo gate, "+
			"C by the membership filter (no channel overlap with A or B, and "+
			"the DM target is B's id, not C's)")

	// The events log carries the message under the DM's channel name
	// (B's instance id). Either party can read the conversation back
	// from this single key — DMs are stateless on the server side.
	persisted := channelMessages(t, s, domain.ChannelName(b.ID()))
	require.Equal(t, []domain.Message{{
		Target:     domain.ChannelName(b.ID()),
		From:       "alpha",
		InstanceID: a.ID(),
		Body:       "private to beta",
		At:         fixedTime,
	}}, persisted)
}
