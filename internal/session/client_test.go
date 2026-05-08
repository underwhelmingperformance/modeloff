package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

// TestServerClient_appendHistory_dedupes_seed_then_live_emit covers
// the seed-then-live-emit race: a registering goroutine seeds the
// buffer from the store while the producer that wrote the seeded
// row is mid-fan-out. The fan-out copy of the same event reaches
// the new client and would otherwise duplicate the most-recent
// entry. The wire layer drops the row ID, so the dedupe must match
// on (concrete type, timestamp).
func TestServerClient_appendHistory_dedupes_seed_then_live_emit(t *testing.T) {
	t.Parallel()

	const target = domain.ChannelName("#room")

	at := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	msg := domain.Message{
		Target: target,
		From:   domain.Nick("alice"),
		Body:   "hello",
		At:     at,
	}

	c := &serverClient{
		history: map[domain.ChannelName][]domain.StoredEvent{
			target: {{ID: 42, Event: msg}},
		},
	}

	c.appendHistory(context.Background(), domain.StoredEvent{Event: msg}, target)

	require.Equal(t, []domain.StoredEvent{{ID: 42, Event: msg}}, c.history[target])
}

// TestServerClient_appendHistory_distinct_events_both_appended
// guards against an over-eager dedupe that would collapse two
// distinct events of the same concrete type at the same nanosecond
// (vanishingly unlikely in production, but the test asserts the
// dedupe does not collapse events that share neither row ID nor
// timestamp).
func TestServerClient_appendHistory_distinct_events_both_appended(t *testing.T) {
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

	c := &serverClient{
		history: map[domain.ChannelName][]domain.StoredEvent{
			target: {{Event: first}},
		},
	}

	c.appendHistory(context.Background(), domain.StoredEvent{Event: second}, target)

	require.Equal(t, []domain.StoredEvent{{Event: first}, {Event: second}}, c.history[target])
}
