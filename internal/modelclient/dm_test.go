package modelclient

import (
	"context"
	"errors"
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

	events, err := mc.hist.snapshot(t.Context(), window)
	require.NoError(t, err)

	return events
}

// inboundDM is a DM from alice to the model-client under test. A
// message addressed to a client carries that client's id as its
// target, which is what the wire form says: `:alice PRIVMSG botty`.
func inboundDM(body string, at time.Time) domain.Message {
	return domain.Message{
		Source: domain.ClientSource("inst-alice", "alice"),
		Target: "inst-botty", Body: body, At: at,
	}
}

// outboundDM is the model-client's own answer to alice.
func outboundDM(body string, at time.Time) domain.Message {
	return domain.Message{
		Source: domain.ClientSource("inst-botty", "botty"),
		Target: "inst-alice", Body: body, At: at,
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
	inboundIRC, _ := protocol.FromChannelEvent(inbound)
	answerIRC, _ := protocol.FromChannelEvent(answer)

	batches := mc.fileBatch(t.Context(), []protocol.Delivery{
		{Event: inbound},
		{Event: answer, HistoryOnly: true},
	})

	require.Equal(t, []turnBatch{{
		channel:  "inst-alice",
		events:   []protocol.IRCMessage{inboundIRC, answerIRC},
		triggers: []protocol.IRCMessage{inboundIRC},
		causes:   []trace.SpanContext{{}},
	}}, derefBatches(batches))

	require.Equal(t, []domain.StoredEvent{
		{Event: inbound},
		{Event: answer},
	}, windowHistory(t, mc, "inst-alice"))

	require.Empty(t, windowHistory(t, mc, "inst-botty"),
		"a model's own id names the conversation, not a buffer of its own")
}

func TestModelClient_history_only_DM_recovers_after_a_history_load_failure(t *testing.T) {
	t.Parallel()

	historyErr := errors.New("read DM history")
	answer := outboundDM("sent once", dmAt)
	unavailable := true
	sess := newFakeSession()
	sess.sub.scrollback = func(
		context.Context,
		protocol.WindowTarget,
		int,
	) ([]protocol.ScrollbackEntry, error) {
		if unavailable {
			return nil, historyErr
		}

		return nil, nil
	}
	mc := newTestModelClient(sess)

	batches := mc.fileBatch(t.Context(), []protocol.Delivery{{
		Event:       answer,
		HistoryOnly: true,
	}})
	require.Empty(t, batches)

	unavailable = false
	loaded, err := mc.hist.snapshot(t.Context(), "inst-alice")
	require.NoError(t, err)
	require.Equal(t, []domain.StoredEvent{{Event: answer}}, loaded)
}

func TestModelClient_history_only_DM_files_without_starting_a_turn(t *testing.T) {
	t.Parallel()

	answer := outboundDM("sent once", dmAt)
	sess := newFakeSession()
	mc := newTestModelClient(sess)

	batches := mc.fileBatch(t.Context(), []protocol.Delivery{{
		Event:       answer,
		HistoryOnly: true,
	}})
	loaded := windowHistory(t, mc, "inst-alice")

	type assertionSnapshot struct {
		Batches []*turnBatch
		History []domain.StoredEvent
	}

	require.Equal(t, assertionSnapshot{
		History: []domain.StoredEvent{{Event: answer}},
	}, assertionSnapshot{
		Batches: batches,
		History: loaded,
	})
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
		history:  []domain.StoredEvent{{Event: earlier}},
		events:   []protocol.IRCMessage{inboundIRC},
		triggers: []protocol.IRCMessage{inboundIRC},
		causes:   []trace.SpanContext{{}},
	}}, derefBatches(batches))

	require.Equal(t, []dmRead{{self: "inst-botty", peer: "inst-alice"}}, sess.dmReadsSoFar())
}

func TestModelClient_first_DM_keeps_an_identical_earlier_message(t *testing.T) {
	t.Parallel()

	message := inboundDM("same words", dmAt)
	sess := newFakeSession()
	sess.dmThreads = map[domain.InstanceID][]domain.StoredEvent{
		"inst-alice": {{ID: 7, Event: message}},
	}
	mc := newTestModelClient(sess)

	trigger, _ := protocol.FromChannelEvent(message)
	batches := mc.fileBatch(t.Context(), []protocol.Delivery{{Event: message}})

	require.Equal(t, []turnBatch{{
		channel:  "inst-alice",
		history:  []domain.StoredEvent{{Event: message}},
		events:   []protocol.IRCMessage{trigger},
		triggers: []protocol.IRCMessage{trigger},
		causes:   []trace.SpanContext{{}},
	}}, derefBatches(batches))
}

// TestDispatchWindowFor covers the window a turn runs in. A DM window
// names the counterpart, which is the routing peer and so the key the
// turn is already running under: the system prompt introduces the
// model to the client it is talking to, and the tools are handed a
// window with somebody in it to answer.
func TestDispatchWindowFor(t *testing.T) {
	t.Parallel()

	t.Run("a direct context names only the counterpart", func(t *testing.T) {
		t.Parallel()

		want := testDirectContext("inst-alice")
		window, err := dispatchWindowFor(t.Context(), validWindowGuard{window: want}, "inst-alice")
		require.NoError(t, err)

		require.Equal(t, want, window)
	})

	t.Run("a channel window is loaded by name", func(t *testing.T) {
		t.Parallel()
		window := testChannelContext(domain.NewChannelWindow("#dev", time.Time{}))

		got, err := dispatchWindowFor(t.Context(), validWindowGuard{window: window}, "#dev")
		require.NoError(t, err)

		require.Equal(t, window, got)
	})

	t.Run("a departed channel is not dispatched", func(t *testing.T) {
		t.Parallel()
		_, err := dispatchWindowFor(t.Context(), nil, "#gone")
		require.ErrorIs(t, err, errDispatchWindowClosed)
	})

	t.Run("a counterpart the store does not hold fails the turn", func(t *testing.T) {
		t.Parallel()

		_, err := dispatchWindowFor(t.Context(), validWindowGuard{err: errors.New("counterpart gone")}, "inst-gone")
		require.Error(t, err)
	})

	// Only a channel has a topic, and the window a DM turn runs in is
	// a channel topic, so the turn's context lines carry none.
	t.Run("a DM window carries no topic line", func(t *testing.T) {
		t.Parallel()

		window, err := dispatchWindowFor(t.Context(), validWindowGuard{window: testDirectContext("inst-alice")}, "inst-alice")
		require.NoError(t, err)

		require.Empty(t, contextReplies(window, nil, nil))
	})
}
