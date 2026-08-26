package modelclient

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// TestHistory_append_preserves_an_identical_later_DM ensures value
// equality does not collapse two messages. The session orders the
// scrollback snapshot before its live queue; the local history can
// therefore append every delivery without guessing its provenance.
func TestHistory_append_preserves_an_identical_later_DM(t *testing.T) {
	t.Parallel()

	const target = domain.ChannelName("inst-alice")

	at := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	msg := domain.Message{
		Source: domain.ClientSource("inst-alice", "alice"), Target: "inst-botty",
		Body: "hello", At: at,
	}
	h := newHistory()
	h.bind(newFakeSubscription(func(
		context.Context,
		protocol.WindowTarget,
		int,
	) ([]protocol.ScrollbackEntry, error) {
		return []protocol.ScrollbackEntry{{Event: msg}}, nil
	}))

	require.NoError(t, h.append(context.Background(), domain.StoredEvent{Event: msg}, target))
	snapshot, err := h.snapshot(context.Background(), target)
	require.NoError(t, err)

	require.Equal(t, []domain.StoredEvent{{Event: msg}, {Event: msg}}, snapshot)
}

// TestHistory_append_distinct_events_both_appended guards against
// an over-eager dedupe that would collapse two distinct events of
// the same concrete type at the same nanosecond (vanishingly
// unlikely in production, but the test asserts the dedupe does not
// collapse events that share neither row ID nor timestamp).
func TestHistory_append_distinct_events_both_appended(t *testing.T) {
	t.Parallel()

	const target = domain.ChannelName("#room")

	first := domain.Message{
		Source: domain.LegacyClientSource("alice"), Target: target,
		Body: "first", At: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC),
	}
	second := domain.Message{
		Source: domain.LegacyClientSource("alice"), Target: target,
		Body: "second", At: time.Date(2025, 1, 1, 12, 0, 1, 0, time.UTC),
	}

	h := newHistory()
	h.seedChannel(target, []domain.StoredEvent{{Event: first}})

	require.NoError(t, h.append(context.Background(), domain.StoredEvent{Event: second}, target))
	snapshot, err := h.snapshot(context.Background(), target)
	require.NoError(t, err)

	require.Equal(t, []domain.StoredEvent{{Event: first}, {Event: second}}, snapshot)
}

// TestHistory_replies_load_append_read covers the private-replies
// ring's lifecycle: seeded at attach, appended live, read by
// snapshot. The replies are `/whois` results — `PersistableEvent`
// but not `domain.ChannelActivity` — so the channel buffer's feeder
// never admits them; the replies ring is where the model keeps them.
func TestHistory_replies_load_append_read(t *testing.T) {
	t.Parallel()

	seeded := domain.Whois{
		Nick:    "target",
		ModelID: "test/model",
		At:      time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC),
	}
	live := domain.SystemNotice{
		Text: "poke interval changed",
		At:   time.Date(2025, 1, 1, 12, 0, 1, 0, time.UTC),
	}

	h := newHistory()
	h.seedReplies(protocol.ChannelWindowTarget("#dev"), []protocol.ReplyEntry{{Event: seeded}})
	h.appendReply(nil, domain.StoredEvent{Event: live})

	require.Equal(t, []storedReply{
		{window: protocol.ChannelWindowTarget("#dev"), event: domain.StoredEvent{Event: seeded}},
		{event: domain.StoredEvent{Event: live}},
	}, h.snapshotRepliesFor("#dev"))
}

func TestHistory_replies_follow_the_issuing_window(t *testing.T) {
	t.Parallel()

	devReply := domain.ListReply{Channel: "#other"}
	otherReply := domain.ListReply{Channel: "#dev"}
	globalReply := domain.PersonasList{}

	h := newHistory()
	h.seedReplies(protocol.ChannelWindowTarget("#dev"), []protocol.ReplyEntry{{Event: devReply}})
	h.seedReplies(protocol.ChannelWindowTarget("#other"), []protocol.ReplyEntry{{Event: otherReply}})
	h.seedReplies(nil, []protocol.ReplyEntry{{Event: globalReply}})

	require.Equal(t, []storedReply{
		{window: protocol.ChannelWindowTarget("#dev"), event: domain.StoredEvent{Event: devReply}},
		{event: domain.StoredEvent{Event: globalReply}},
	}, h.snapshotRepliesFor("#dev"))

	h.forget("#dev")
	require.Equal(t, []storedReply{
		{window: protocol.ChannelWindowTarget("#other"), event: domain.StoredEvent{Event: otherReply}},
		{event: domain.StoredEvent{Event: globalReply}},
	}, h.snapshotRepliesFor("#other"))
}

func TestHistory_replies_remain_chronological_across_scopes(t *testing.T) {
	t.Parallel()

	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	globalReply := domain.SystemNotice{Text: "older global state", At: base}
	windowReply := domain.SystemNotice{Target: "#dev", Text: "newer window state", At: base.Add(time.Minute)}

	h := newHistory()
	h.seedReplies(protocol.ChannelWindowTarget("#dev"), []protocol.ReplyEntry{{Event: windowReply}})
	h.seedReplies(nil, []protocol.ReplyEntry{{Event: globalReply}})

	replies := h.snapshotRepliesFor("#dev")
	require.Equal(t, []storedReply{
		{event: domain.StoredEvent{Event: globalReply}},
		{window: protocol.ChannelWindowTarget("#dev"), event: domain.StoredEvent{Event: windowReply}},
	}, replies)
}

// TestHistory_replies_trim_from_older_end pins that the replies ring
// trims to modelHistorySize from the older end, dropping the oldest
// entries first.
func TestHistory_replies_trim_from_older_end(t *testing.T) {
	t.Parallel()

	h := newHistory()

	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	total := modelHistorySize + 3
	for i := range total {
		h.appendReply(nil, domain.StoredEvent{Event: domain.SystemNotice{At: base.Add(time.Duration(i) * time.Second)}})
	}

	want := make([]storedReply, modelHistorySize)
	for i := range want {
		at := base.Add(time.Duration(i+3) * time.Second)
		want[i] = storedReply{event: domain.StoredEvent{Event: domain.SystemNotice{At: at}}}
	}

	require.Equal(t, want, h.snapshotRepliesFor("#dev"))
}

func TestHistory_reply_limit_is_independent_for_each_window(t *testing.T) {
	t.Parallel()

	h := newHistory()
	devReply := domain.SystemNotice{Target: "#dev", Text: "keep me"}
	h.appendReply(protocol.ChannelWindowTarget("#dev"), domain.StoredEvent{Event: devReply})

	for range modelHistorySize + 1 {
		h.appendReply(protocol.ChannelWindowTarget("#other"), domain.StoredEvent{
			Event: domain.SystemNotice{Target: "#other", Text: "other"},
		})
	}

	require.Equal(t, []storedReply{{
		window: protocol.ChannelWindowTarget("#dev"),
		event:  domain.StoredEvent{Event: devReply},
	}}, h.snapshotRepliesFor("#dev"))
}

// TestHistory_snapshotRepliesFor_is_defensive_copy proves the dispatch
// turn's snapshot does not alias the live backing array, so a
// concurrent append cannot mutate a snapshot already handed out.
func TestHistory_snapshotRepliesFor_is_defensive_copy(t *testing.T) {
	t.Parallel()

	at := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHistory()
	h.appendReply(nil, domain.StoredEvent{Event: domain.SystemNotice{At: at}})

	snap := h.snapshotRepliesFor("#dev")
	h.appendReply(nil, domain.StoredEvent{Event: domain.SystemNotice{At: at.Add(time.Second)}})

	require.Equal(t, []storedReply{{event: domain.StoredEvent{Event: domain.SystemNotice{At: at}}}}, snap)
}
