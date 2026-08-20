package modelclient

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// TestFileBatch covers how a burst of deliveries is turned into
// turns. A model that was busy while several events landed reads
// them all at once, and each window it shares gets one turn carrying
// every trigger that arrived for it — a channel's worth of catching
// up in one prompt, not one round-trip per line.
//
// The split the table pins is the one between a turn's triggers and
// its history: the events the model is being asked about, and the
// events it needs to read them against. A burst's non-triggers
// belong to the second, in the order they arrived, so the model is
// told the topic changed between two messages it is answering
// together.
func TestFileBatch(t *testing.T) {
	t.Parallel()

	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	msg := func(target domain.ChannelName, from domain.Nick, body string) domain.Message {
		return domain.Message{
			Target:     target,
			From:       from,
			InstanceID: domain.InstanceID("inst-" + from),
			Body:       body,
			At:         at,
		}
	}

	trigger := func(target domain.ChannelName, from domain.Nick, body string) protocol.IRCMessage {
		irc, _ := protocol.FromChannelEvent(msg(target, from, body))
		return irc
	}

	topic := func(target domain.ChannelName, body string) domain.TopicChange {
		return domain.TopicChange{Target: target, Topic: body, By: "alice", At: at}
	}

	filed := func(ev domain.ChannelActivity) domain.StoredEvent {
		return domain.StoredEvent{Event: ev}
	}

	tests := []struct {
		name       string
		deliveries []domain.ProtocolEvent
		want       []turnBatch
	}{
		{
			name:       "a single message is one turn with one trigger",
			deliveries: []domain.ProtocolEvent{msg("#dev", "alice", "hi")},
			want: []turnBatch{{
				channel:  "#dev",
				triggers: []protocol.IRCMessage{trigger("#dev", "alice", "hi")},
			}},
		},
		{
			name: "a burst in one window is one turn carrying every trigger",
			deliveries: []domain.ProtocolEvent{
				msg("#dev", "alice", "one"),
				msg("#dev", "bob", "two"),
				msg("#dev", "alice", "three"),
			},
			want: []turnBatch{{
				channel: "#dev",
				triggers: []protocol.IRCMessage{
					trigger("#dev", "alice", "one"),
					trigger("#dev", "bob", "two"),
					trigger("#dev", "alice", "three"),
				},
			}},
		},
		{
			name: "a burst spanning two windows is one turn each, first-seen order",
			deliveries: []domain.ProtocolEvent{
				msg("#dev", "alice", "one"),
				msg("#ops", "bob", "two"),
				msg("#dev", "carol", "three"),
			},
			want: []turnBatch{
				{
					channel: "#dev",
					triggers: []protocol.IRCMessage{
						trigger("#dev", "alice", "one"),
						trigger("#dev", "carol", "three"),
					},
				},
				{
					channel:  "#ops",
					triggers: []protocol.IRCMessage{trigger("#ops", "bob", "two")},
				},
			},
		},
		{
			name: "a non-triggering event is filed without raising a turn",
			deliveries: []domain.ProtocolEvent{
				domain.Part{Target: "#dev", Nick: "botty", InstanceID: "inst-botty", At: at},
			},
			want: nil,
		},
		{
			name: "a non-trigger between two triggers is history for the turn they share",
			deliveries: []domain.ProtocolEvent{
				msg("#dev", "alice", "one"),
				topic("#dev", "now discussing coalescing"),
				msg("#dev", "bob", "two"),
			},
			want: []turnBatch{{
				channel: "#dev",
				history: []domain.StoredEvent{filed(topic("#dev", "now discussing coalescing"))},
				triggers: []protocol.IRCMessage{
					trigger("#dev", "alice", "one"),
					trigger("#dev", "bob", "two"),
				},
			}},
		},
		{
			name: "a non-trigger leading the burst is history for the turn behind it",
			deliveries: []domain.ProtocolEvent{
				topic("#dev", "leading"),
				msg("#dev", "alice", "one"),
			},
			want: []turnBatch{{
				channel:  "#dev",
				history:  []domain.StoredEvent{filed(topic("#dev", "leading"))},
				triggers: []protocol.IRCMessage{trigger("#dev", "alice", "one")},
			}},
		},
		{
			name: "a non-trigger trailing the burst is history for the turn ahead of it",
			deliveries: []domain.ProtocolEvent{
				msg("#dev", "alice", "one"),
				topic("#dev", "trailing"),
			},
			want: []turnBatch{{
				channel:  "#dev",
				history:  []domain.StoredEvent{filed(topic("#dev", "trailing"))},
				triggers: []protocol.IRCMessage{trigger("#dev", "alice", "one")},
			}},
		},
		{
			name: "a non-trigger for a window with no turn reaches no batch",
			deliveries: []domain.ProtocolEvent{
				msg("#dev", "alice", "one"),
				topic("#ops", "elsewhere"),
			},
			want: []turnBatch{{
				channel:  "#dev",
				triggers: []protocol.IRCMessage{trigger("#dev", "alice", "one")},
			}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mc := newTestModelClient(newFakeSession())

			deliveries := make([]protocol.Delivery, 0, len(tc.deliveries))
			for _, ev := range tc.deliveries {
				deliveries = append(deliveries, protocol.Delivery{Event: ev})
			}

			got := make([]turnBatch, 0, len(tc.want))
			for _, batch := range mc.fileBatch(t.Context(), deliveries) {
				// The ring starts empty and the causes are the zero
				// span context, so a turn's identity here is its
				// window, its triggers, and whatever of the burst it
				// carries as history.
				got = append(got, turnBatch{
					channel:  batch.channel,
					history:  batch.history,
					triggers: batch.triggers,
				})
			}

			require.Equal(t, tc.want, nonEmptyBatches(got))
		})
	}
}

// nonEmptyBatches normalises an empty result to nil so a table entry
// expecting no turns reads as `nil`.
func nonEmptyBatches(batches []turnBatch) []turnBatch {
	if len(batches) == 0 {
		return nil
	}

	return batches
}

// TestFileBatch_snapshot_precedes_the_triggers_it_files pins the
// ordering a coalesced turn depends on: the window's transcript is
// snapshotted before any of the burst is filed, so a trigger the
// prompt lists explicitly is not also sitting in the transcript
// above it. The ring keeps everything, so the next turn reads the
// triggers this one answered.
func TestFileBatch_snapshot_precedes_the_triggers_it_files(t *testing.T) {
	t.Parallel()

	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	earlier := domain.Message{Target: "#dev", From: "alice", Body: "earlier", At: at.Add(-time.Minute)}

	mc := newTestModelClient(newFakeSession())
	mc.hist.seedChannel("#dev", []domain.StoredEvent{{ID: 1, Event: earlier}})

	first := domain.Message{Target: "#dev", From: "bob", InstanceID: "inst-bob", Body: "one", At: at}
	second := domain.Message{Target: "#dev", From: "bob", InstanceID: "inst-bob", Body: "two", At: at.Add(time.Second)}

	firstIRC, _ := protocol.FromChannelEvent(first)
	secondIRC, _ := protocol.FromChannelEvent(second)

	batches := mc.fileBatch(t.Context(), []protocol.Delivery{
		{Event: first},
		{Event: second},
	})

	require.Equal(t, []turnBatch{{
		channel:  "#dev",
		history:  []domain.StoredEvent{{ID: 1, Event: earlier}},
		triggers: []protocol.IRCMessage{firstIRC, secondIRC},
		causes:   []trace.SpanContext{{}, {}},
	}}, derefBatches(batches))

	require.Equal(t, []domain.StoredEvent{
		{ID: 1, Event: earlier},
		{Event: first},
		{Event: second},
	}, windowHistory(t, mc, "#dev"))
}

// derefBatches flattens the returned pointers so a whole batch can
// be compared by value.
func derefBatches(batches []*turnBatch) []turnBatch {
	out := make([]turnBatch, 0, len(batches))
	for _, b := range batches {
		out = append(out, *b)
	}

	return out
}
