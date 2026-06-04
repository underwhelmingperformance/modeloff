package modelclient

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

// TestHistory_append_dedupes_seed_then_live_emit covers the seed-
// then-live-emit race: a registering goroutine seeds the buffer
// from the store while the producer that wrote the seeded row is
// mid-fan-out. The fan-out copy of the same event reaches the new
// client and would otherwise duplicate the most-recent entry. The
// wire layer drops the row ID, so the dedupe must match on
// (concrete type, timestamp).
func TestHistory_append_dedupes_seed_then_live_emit(t *testing.T) {
	t.Parallel()

	const target = domain.ChannelName("#room")

	at := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	msg := domain.Message{
		Target: target,
		From:   domain.Nick("alice"),
		Body:   "hello",
		At:     at,
	}

	h := newHistory()
	h.seedChannel(target, []domain.StoredEvent{{ID: 42, Event: msg}})

	h.append(context.Background(), nil, "self", domain.StoredEvent{Event: msg}, target)

	require.Equal(t, []domain.StoredEvent{{ID: 42, Event: msg}}, h.snapshot(target))
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
		Target: target,
		From:   domain.Nick("alice"),
		Body:   "first",
		At:     time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC),
	}
	second := domain.Message{
		Target: target,
		From:   domain.Nick("alice"),
		Body:   "second",
		At:     time.Date(2025, 1, 1, 12, 0, 1, 0, time.UTC),
	}

	h := newHistory()
	h.seedChannel(target, []domain.StoredEvent{{Event: first}})

	h.append(context.Background(), nil, "self", domain.StoredEvent{Event: second}, target)

	require.Equal(t, []domain.StoredEvent{{Event: first}, {Event: second}}, h.snapshot(target))
}

// TestHistory_replies_load_append_read covers the private-replies
// ring's lifecycle: seeded at attach, appended live, read by
// snapshot. The replies are `/whois` results — `!ModelVisible` by
// design — so the ring must keep them where the channel buffer
// would drop them.
func TestHistory_replies_load_append_read(t *testing.T) {
	t.Parallel()

	seeded := domain.Whois{
		Nick:    "target",
		ModelID: "test/model",
		At:      time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC),
	}
	live := domain.ListEnd{
		At: time.Date(2025, 1, 1, 12, 0, 1, 0, time.UTC),
	}

	require.False(t, seeded.ModelVisible(),
		"a whois reply is not model-visible; the replies ring must keep it anyway")

	h := newHistory()
	h.seedReplies([]domain.StoredEvent{{ID: 7, Event: seeded}})
	h.appendReply(domain.StoredEvent{Event: live})

	require.Equal(t, []domain.StoredEvent{
		{ID: 7, Event: seeded},
		{Event: live},
	}, h.snapshotReplies())
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
		h.appendReply(domain.StoredEvent{Event: domain.ListEnd{At: base.Add(time.Duration(i) * time.Second)}})
	}

	want := make([]domain.StoredEvent, modelHistorySize)
	for i := range want {
		at := base.Add(time.Duration(i+3) * time.Second)
		want[i] = domain.StoredEvent{Event: domain.ListEnd{At: at}}
	}

	require.Equal(t, want, h.snapshotReplies())
}

// TestHistory_snapshotReplies_is_defensive_copy proves the dispatch
// turn's snapshot does not alias the live backing array, so a
// concurrent append cannot mutate a snapshot already handed out.
func TestHistory_snapshotReplies_is_defensive_copy(t *testing.T) {
	t.Parallel()

	at := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHistory()
	h.appendReply(domain.StoredEvent{Event: domain.ListEnd{At: at}})

	snap := h.snapshotReplies()
	h.appendReply(domain.StoredEvent{Event: domain.ListEnd{At: at.Add(time.Second)}})

	require.Equal(t, []domain.StoredEvent{{Event: domain.ListEnd{At: at}}}, snap)
}
