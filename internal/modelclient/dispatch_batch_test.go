package modelclient

import (
	"fmt"
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
// The table pins both the chronological current-event sequence sent
// to the model and the trigger subset used for dispatch authority.
// A non-trigger in a burst stays between the surrounding triggers.
func TestFileBatch(t *testing.T) {
	t.Parallel()

	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	msg := func(target domain.ChannelName, from domain.Nick, body string) domain.Message {
		return domain.Message{
			Source: domain.ClientSource(domain.InstanceID("inst-"+from), from),
			Target: target, Body: body, At: at,
		}
	}

	trigger := func(target domain.ChannelName, from domain.Nick, body string) protocol.IRCMessage {
		irc, _ := protocol.FromChannelEvent(msg(target, from, body))
		return irc
	}

	topic := func(target domain.ChannelName, body string) domain.TopicChange {
		return domain.TopicChange{Source: domain.ClientSource("inst-alice", "alice"), Target: target, Topic: body, At: at}
	}

	wire := func(ev domain.ChannelActivity) protocol.IRCMessage {
		message, _ := protocol.FromChannelEvent(ev)
		return message
	}

	causes := func(count int) []trace.SpanContext {
		return make([]trace.SpanContext, count)
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
				events:   []protocol.IRCMessage{trigger("#dev", "alice", "hi")},
				triggers: []protocol.IRCMessage{trigger("#dev", "alice", "hi")},
				causes:   causes(1),
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
				events: []protocol.IRCMessage{
					trigger("#dev", "alice", "one"),
					trigger("#dev", "bob", "two"),
					trigger("#dev", "alice", "three"),
				},
				triggers: []protocol.IRCMessage{
					trigger("#dev", "alice", "one"),
					trigger("#dev", "bob", "two"),
					trigger("#dev", "alice", "three"),
				},
				latestTrigger: 2,
				causes:        causes(3),
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
					events: []protocol.IRCMessage{
						trigger("#dev", "alice", "one"),
						trigger("#dev", "carol", "three"),
					},
					triggers: []protocol.IRCMessage{
						trigger("#dev", "alice", "one"),
						trigger("#dev", "carol", "three"),
					},
					latestTrigger: 1,
					causes:        causes(2),
				},
				{
					channel:  "#ops",
					events:   []protocol.IRCMessage{trigger("#ops", "bob", "two")},
					triggers: []protocol.IRCMessage{trigger("#ops", "bob", "two")},
					causes:   causes(1),
				},
			},
		},
		{
			name: "a non-triggering event is filed without raising a turn",
			deliveries: []domain.ProtocolEvent{
				domain.Part{Source: domain.ClientSource("inst-botty", "botty"), Target: "#dev", At: at},
			},
			want: nil,
		},
		{
			name: "a non-trigger between two triggers stays in the current sequence",
			deliveries: []domain.ProtocolEvent{
				msg("#dev", "alice", "one"),
				topic("#dev", "now discussing coalescing"),
				msg("#dev", "bob", "two"),
			},
			want: []turnBatch{{
				channel: "#dev",
				events: []protocol.IRCMessage{
					trigger("#dev", "alice", "one"),
					wire(topic("#dev", "now discussing coalescing")),
					trigger("#dev", "bob", "two"),
				},
				triggers: []protocol.IRCMessage{
					trigger("#dev", "alice", "one"),
					trigger("#dev", "bob", "two"),
				},
				latestTrigger: 2,
				causes:        causes(2),
			}},
		},
		{
			name: "a non-trigger can lead the current sequence",
			deliveries: []domain.ProtocolEvent{
				topic("#dev", "leading"),
				msg("#dev", "alice", "one"),
			},
			want: []turnBatch{{
				channel:       "#dev",
				events:        []protocol.IRCMessage{wire(topic("#dev", "leading")), trigger("#dev", "alice", "one")},
				triggers:      []protocol.IRCMessage{trigger("#dev", "alice", "one")},
				latestTrigger: 1,
				causes:        causes(1),
			}},
		},
		{
			name: "a non-trigger can trail the current sequence",
			deliveries: []domain.ProtocolEvent{
				msg("#dev", "alice", "one"),
				topic("#dev", "trailing"),
			},
			want: []turnBatch{{
				channel:  "#dev",
				events:   []protocol.IRCMessage{trigger("#dev", "alice", "one"), wire(topic("#dev", "trailing"))},
				triggers: []protocol.IRCMessage{trigger("#dev", "alice", "one")},
				causes:   causes(1),
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
				events:   []protocol.IRCMessage{trigger("#dev", "alice", "one")},
				triggers: []protocol.IRCMessage{trigger("#dev", "alice", "one")},
				causes:   causes(1),
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

			batches := derefBatches(mc.fileBatch(t.Context(), deliveries))
			require.Equal(t, tc.want, nonEmptyBatches(batches))
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
	earlier := domain.Message{Source: domain.ClientSource("inst-alice", "alice"), Target: "#dev", Body: "earlier", At: at.Add(-time.Minute)}

	mc := newTestModelClient(newFakeSession())
	mc.hist.seedChannel("#dev", []domain.StoredEvent{{ID: 1, Event: earlier}})

	first := domain.Message{Source: domain.ClientSource("inst-bob", "bob"), Target: "#dev", Body: "one", At: at}
	second := domain.Message{Source: domain.ClientSource("inst-bob", "bob"), Target: "#dev", Body: "two", At: at.Add(time.Second)}

	firstIRC, _ := protocol.FromChannelEvent(first)
	secondIRC, _ := protocol.FromChannelEvent(second)

	batches := mc.fileBatch(t.Context(), []protocol.Delivery{
		{Event: first},
		{Event: second},
	})

	require.Equal(t, []turnBatch{{
		channel:       "#dev",
		history:       []domain.StoredEvent{{ID: 1, Event: earlier}},
		events:        []protocol.IRCMessage{firstIRC, secondIRC},
		triggers:      []protocol.IRCMessage{firstIRC, secondIRC},
		latestTrigger: 1,
		causes:        []trace.SpanContext{{}, {}},
	}}, derefBatches(batches))

	require.Equal(t, []domain.StoredEvent{
		{ID: 1, Event: earlier},
		{Event: first},
		{Event: second},
	}, windowHistory(t, mc, "#dev"))
}

func TestFileBatch_preserves_causal_order_inside_the_current_turn(t *testing.T) {
	t.Parallel()

	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	first := domain.Message{
		Source: domain.ClientSource("inst-alice", "alice"), Target: "#dev",
		Body: "one", At: at,
	}
	topic := domain.TopicChange{
		Source: domain.ClientSource("inst-alice", "alice"), Target: "#dev",
		Topic: "between", At: at.Add(time.Second),
	}
	second := domain.Message{
		Source: domain.ClientSource("inst-bob", "bob"), Target: "#dev",
		Body: "two", At: at.Add(2 * time.Second),
	}
	firstIRC, _ := protocol.FromChannelEvent(first)
	topicIRC, _ := protocol.FromChannelEvent(topic)
	secondIRC, _ := protocol.FromChannelEvent(second)

	mc := newTestModelClient(newFakeSession())
	batches := mc.fileBatch(t.Context(), []protocol.Delivery{
		{Event: first}, {Event: topic}, {Event: second},
	})

	require.Equal(t, []turnBatch{{
		channel:       "#dev",
		events:        []protocol.IRCMessage{firstIRC, topicIRC, secondIRC},
		triggers:      []protocol.IRCMessage{firstIRC, secondIRC},
		latestTrigger: 2,
		causes:        []trace.SpanContext{{}, {}},
	}}, derefBatches(batches))
}

func TestDrain_caps_one_burst_and_leaves_the_remainder_queued(t *testing.T) {
	t.Parallel()

	events := make(chan protocol.Delivery, modelHistorySize+2)
	all := make([]protocol.Delivery, 0, modelHistorySize+2)
	for i := range modelHistorySize + 2 {
		delivery := protocol.Delivery{Event: domain.SystemNotice{Text: fmt.Sprintf("event-%d", i)}}
		all = append(all, delivery)
		events <- delivery
	}

	drained := drain(events)
	remaining := drain(events)

	require.Equal(t, struct {
		Drained   []protocol.Delivery
		Remaining []protocol.Delivery
	}{
		Drained:   all[:modelHistorySize-1],
		Remaining: all[modelHistorySize-1:],
	}, struct {
		Drained   []protocol.Delivery
		Remaining []protocol.Delivery
	}{
		Drained:   drained,
		Remaining: remaining,
	})
}

func TestFileBatch_discards_history_across_a_rejoin(t *testing.T) {
	t.Parallel()

	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	const channel = domain.ChannelName("#dev")

	mc := newTestModelClient(newFakeSession())
	old := domain.Message{Source: domain.ClientSource("inst-alice", "alice"), Target: channel, Body: "before part", At: at}
	mc.hist.seedChannel(channel, []domain.StoredEvent{{Event: old}})

	part := domain.Part{
		Source: domain.ClientSource(mc.instance.ID(), "botty"), Target: channel, At: at.Add(time.Second),
	}
	join := domain.Join{
		Source: domain.ClientSource(mc.instance.ID(), "botty"), Target: channel, At: at.Add(2 * time.Second),
	}
	message := domain.Message{
		Source: domain.ClientSource("inst-alice", "alice"), Target: channel,
		Body: "after rejoin", At: at.Add(3 * time.Second),
	}
	messageIRC, _ := protocol.FromChannelEvent(message)
	joinIRC, _ := protocol.FromChannelEvent(join)
	oldIRC, _ := protocol.FromChannelEvent(old)
	require.NotEqual(t, oldIRC, messageIRC)

	batches := mc.fileBatch(t.Context(), []protocol.Delivery{
		{Event: old},
		{Event: part},
		{Event: join},
		{Event: message},
	})

	require.Equal(t, []turnBatch{{
		channel:       channel,
		events:        []protocol.IRCMessage{joinIRC, messageIRC},
		triggers:      []protocol.IRCMessage{messageIRC},
		latestTrigger: 1,
		causes:        []trace.SpanContext{{}},
	}}, derefBatches(batches))
}

func TestClosedWindow_uses_projected_kick_subject_identity(t *testing.T) {
	t.Parallel()

	self := domain.NewModelInstance("inst-botty", "renamed", "test/model", "", nil)
	event := domain.Kicked{
		Target:        "#dev",
		Subject:       "nick-before-kick",
		SubjectIsSelf: true,
	}
	target, closed := closedWindow(self, event)
	got := struct {
		Target domain.ChannelName
		Closed bool
	}{Target: target, Closed: closed}

	require.Equal(t, struct {
		Target domain.ChannelName
		Closed bool
	}{Target: "#dev", Closed: true}, got)
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
