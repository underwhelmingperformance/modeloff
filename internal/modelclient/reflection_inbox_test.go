package modelclient

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
)

type recordedReflectionInbox struct {
	Events []storemod.ReflectionEvent
	Status storemod.ReflectionInboxStatus
}

func TestModelClient_records_replay_and_live_activity_once_for_reflection(t *testing.T) {
	stored := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful and curious", nil,
	)
	require.NoError(t, stored.SaveInstance(t.Context(), instance))

	recordedAt := time.Date(2026, 8, 26, 16, 0, 0, 0, time.UTC)
	peerMessage := domain.Message{
		Source: domain.ClientSource("inst-alice", "alice"),
		Target: "#dev", Body: "the branch is ready", At: recordedAt.Add(-time.Minute),
	}
	peerHistory := protocol.HistoryRef{
		Kind: protocol.HistorySourceChannelScrollback, ID: 14,
		Window: protocol.ChannelWindowTarget("#dev"),
	}
	selfMessage := domain.Message{
		Source: domain.ClientSource(instance.ID(), instance.Nick()),
		Target: "#dev", Body: "I will review it", At: recordedAt,
	}
	selfHistory := protocol.HistoryRef{
		Kind: protocol.HistorySourceChannelScrollback, ID: 15,
		Window: protocol.ChannelWindowTarget("#dev"),
	}
	mc := New(Config{
		Instance: instance, Reflections: stored,
		Now: func() time.Time { return recordedAt },
	})

	mc.recordReflectionEntries(t.Context(), []protocol.ScrollbackEntry{{
		Event: peerMessage, History: peerHistory,
	}})
	mc.recordReflectionDeliveries(t.Context(), []protocol.Delivery{
		{Event: peerMessage, History: []protocol.HistoryRef{peerHistory}},
		{Event: selfMessage, History: []protocol.HistoryRef{selfHistory}, HistoryOnly: true},
	})

	status, err := stored.ReflectionInboxStatus(t.Context(), instance.ID())
	require.NoError(t, err)
	events, err := stored.ReflectionEvents(
		t.Context(), instance.ID(), 0, status.HighWaterMark, 10,
	)
	require.NoError(t, err)
	peerIRC, ok := protocol.FromChannelEvent(peerMessage)
	require.True(t, ok)
	selfIRC, ok := protocol.FromChannelEvent(selfMessage)
	require.True(t, ok)

	want := recordedReflectionInbox{
		Events: []storemod.ReflectionEvent{
			{
				Sequence: events[0].Sequence, InstanceID: instance.ID(),
				Source: peerHistory, Message: peerIRC, Substantive: true,
				CreatedAt: recordedAt,
			},
			{
				Sequence: events[1].Sequence, InstanceID: instance.ID(),
				Source: selfHistory, Message: selfIRC, CreatedAt: recordedAt,
			},
		},
		Status: storemod.ReflectionInboxStatus{
			HighWaterMark: status.HighWaterMark,
			PendingEvents: 2, SubstantiveEvents: 1,
		},
	}
	require.Equal(t, want, recordedReflectionInbox{Events: events, Status: status})
}

// reflectionOrderEffect is the bodies the stream recorded, in the order it
// recorded them.
type reflectionOrderEffect struct {
	Bodies []string
}

// TestModelClient_files_a_loaded_thread_before_the_burst_that_opened_it
// pins the order a first direct-message turn reaches the reflection
// stream in.
//
// Opening the batch is what loads the stored thread, so a burst recorded
// before that would land ahead of the conversation it answers. The prompt
// reads the thread first, and a stream ordered the other way describes a
// conversation that did not happen.
func TestModelClient_files_a_loaded_thread_before_the_burst_that_opened_it(t *testing.T) {
	stored := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful and curious", nil,
	)
	require.NoError(t, stored.SaveInstance(t.Context(), instance))
	at := time.Date(2026, 8, 26, 16, 0, 0, 0, time.UTC)

	earlier := inboundDM("this was said before the attach", at.Add(-time.Hour))
	sess := newFakeSession()
	sess.dmThreads = map[domain.InstanceID][]domain.StoredEvent{
		"inst-alice": {{ID: 7, Event: earlier}},
	}
	mc := newReflectingModelClient(t, sess, instance, stored, at)

	inbound := inboundDM("still there?", at)
	mc.fileBatch(t.Context(), []protocol.Delivery{{
		Event: inbound,
		History: []protocol.HistoryRef{{
			Kind: protocol.HistorySourceEvent, ID: 8,
			Window: protocol.DirectWindowTarget("inst-alice"),
		}},
	}})

	status, err := stored.ReflectionInboxStatus(t.Context(), instance.ID())
	require.NoError(t, err)
	events, err := stored.ReflectionEvents(
		t.Context(), instance.ID(), 0, status.HighWaterMark, 10,
	)
	require.NoError(t, err)
	bodies := make([]string, 0, len(events))
	for _, event := range events {
		bodies = append(bodies, event.Message.Body)
	}

	require.Equal(t, reflectionOrderEffect{Bodies: []string{
		"this was said before the attach", "still there?",
	}}, reflectionOrderEffect{Bodies: bodies})
}

func newReflectingModelClient(
	t *testing.T,
	sess *fakeSession,
	instance *domain.Instance,
	stored *storemod.SQLiteStore,
	at time.Time,
) *ModelClient {
	t.Helper()

	mc := New(Config{
		Instance:        instance,
		Attachment:      protocol.NewAttachment(),
		Session:         sess,
		APIClient:       func() api.Client { return nil },
		LifetimeContext: context.Background,
		Reflections:     stored,
		Now:             func() time.Time { return at },
	})
	mc.hist.bind(sess.sub)

	return mc
}
