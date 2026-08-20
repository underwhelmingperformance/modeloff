package modelclient

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// dmAt is the timestamp the DM fixtures below are built around.
var dmAt = time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

// windowHistory reads a model-client's transcript for one window,
// through the same call the dispatch path takes.
func windowHistory(t *testing.T, mc *ModelClient, window domain.ChannelName) []domain.StoredEvent {
	t.Helper()

	return mc.hist.snapshot(t.Context(), mc.sess, mc.instance.ID(), window)
}

// inboundDM is a DM from alice to the model-client under test. A
// message addressed to a client carries that client's id as its
// target, which is what the wire form says: `:alice PRIVMSG botty`.
func inboundDM(body string, at time.Time) domain.Message {
	return domain.Message{
		Target:     "inst-botty",
		From:       "alice",
		InstanceID: "inst-alice",
		Body:       body,
		At:         at,
	}
}

// outboundDM is the model-client's own answer to alice.
func outboundDM(body string, at time.Time) domain.Message {
	return domain.Message{
		Target:     "inst-alice",
		From:       "botty",
		InstanceID: "inst-botty",
		Body:       body,
		At:         at,
	}
}

// TestModelClient_DM_is_one_buffer_keyed_by_the_counterpart pins the
// conversation a model reads back from a DM. The two directions carry
// different targets, because a message names its recipient, so keying
// a buffer by the raw target would split one conversation in two:
// what alice said under the model's own id, what the model answered
// under alice's, and a prompt built from the first showing the model
// nothing it had said itself.
//
// Both directions key by [domain.Message.RoutingKey], the
// counterpart, so one buffer holds the conversation in the order it
// happened.
func TestModelClient_DM_is_one_buffer_keyed_by_the_counterpart(t *testing.T) {
	t.Parallel()

	sess := newFakeSession()
	mc := newTestModelClient(sess)

	inbound := inboundDM("you around?", dmAt)
	answer := outboundDM("yep", dmAt.Add(time.Second))

	sess.handleFn = func(protocol.Command) protocol.Response {
		return protocol.Response{Events: []protocol.Event{answer}}
	}

	inboundIRC, _ := protocol.FromChannelEvent(inbound)

	batches := mc.fileBatch(t.Context(), []protocol.Delivery{{Event: inbound}})

	require.Equal(t, []turnBatch{{
		channel:  "inst-alice",
		triggers: []protocol.IRCMessage{inboundIRC},
		causes:   []trace.SpanContext{{}},
	}}, derefBatches(batches))

	_, err := mc.Send(t.Context(), protocol.PrivMsg{
		Target: protocol.NickTarget("alice"),
		Body:   "yep",
	})
	require.NoError(t, err)

	require.Equal(t, []domain.StoredEvent{
		{Event: inbound},
		{Event: answer},
	}, windowHistory(t, mc, "inst-alice"))

	require.Empty(t, windowHistory(t, mc, "inst-botty"),
		"a model's own id names the conversation, not a buffer of its own")
}

// TestModelClient_first_DM_turn_reads_the_persisted_thread pins that
// the first DM turn of a connection is prompted from the conversation
// as it already stood, so the model does not answer as though it had
// just started. Two things have to hold for that: the load runs
// before the turn's transcript is snapshotted, and it asks for the
// thread with the counterpart, which is the only one with anything in
// it.
func TestModelClient_first_DM_turn_reads_the_persisted_thread(t *testing.T) {
	t.Parallel()

	earlier := inboundDM("this was said before the attach", dmAt.Add(-time.Hour))

	sess := newFakeSession()
	sess.dmThreads = map[domain.InstanceID][]domain.StoredEvent{
		"inst-alice": {{ID: 7, Event: earlier}},
	}

	mc := newTestModelClient(sess)

	inbound := inboundDM("still there?", dmAt)
	inboundIRC, _ := protocol.FromChannelEvent(inbound)

	batches := mc.fileBatch(t.Context(), []protocol.Delivery{{Event: inbound}})

	require.Equal(t, []turnBatch{{
		channel:  "inst-alice",
		history:  []domain.StoredEvent{{ID: 7, Event: earlier}},
		triggers: []protocol.IRCMessage{inboundIRC},
		causes:   []trace.SpanContext{{}},
	}}, derefBatches(batches))

	require.Equal(t, []dmRead{{self: "inst-botty", peer: "inst-alice"}}, sess.dmReadsSoFar())
}

// TestDispatchWindowFor covers the window a turn runs in. A DM window
// names the counterpart, which is the routing peer and so the key the
// turn is already running under: the system prompt introduces the
// model to the client it is talking to, and the tools are handed a
// window with somebody in it to answer.
func TestDispatchWindowFor(t *testing.T) {
	t.Parallel()

	alice := domain.NewModelInstance("inst-alice", "alice", "test/model-a", "", nil)
	botty := domain.NewModelInstance("inst-botty", "botty", "test/model-b", "", nil)

	sess := newFakeSession()
	sess.instances = map[domain.InstanceID]*domain.Instance{"inst-alice": alice}

	t.Run("a DM window names the counterpart", func(t *testing.T) {
		t.Parallel()

		window, err := dispatchWindowFor(t.Context(), sess, "inst-alice", botty)
		require.NoError(t, err)

		require.Equal(t, domain.NewDMWindow(alice, sess.Now()), window)
	})

	t.Run("a channel window is loaded by name", func(t *testing.T) {
		t.Parallel()

		window, err := dispatchWindowFor(t.Context(), sess, "#dev", botty)
		require.NoError(t, err)

		require.Equal(t, domain.NewChannelWindow("#dev", sess.Now()), window)
	})

	t.Run("a counterpart the store does not hold fails the turn", func(t *testing.T) {
		t.Parallel()

		_, err := dispatchWindowFor(t.Context(), sess, "inst-gone", botty)
		require.Error(t, err)
	})

	// Only a channel has a topic, and the window a DM turn runs in is
	// a `*DMWindow`, so the turn's context lines carry none.
	t.Run("a DM window carries no topic line", func(t *testing.T) {
		t.Parallel()

		window, err := dispatchWindowFor(t.Context(), sess, "inst-alice", botty)
		require.NoError(t, err)

		require.Empty(t, contextReplies(window, nil))
	})
}
