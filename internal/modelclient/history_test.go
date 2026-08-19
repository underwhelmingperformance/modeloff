package modelclient

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// tokenSizedEvents builds n domain.Message-backed StoredEvents, each
// with a rendered From+Target+Body length of exactly
// tokensEach*bytesPerEstimatedToken bytes (so [estimateEventTokens]
// costs each one at exactly tokensEach), at one-second intervals
// starting at `at` — oldest first, matching the order a ring holds
// its contents in.
func tokenSizedEvents(n, tokensEach int, at time.Time) []domain.StoredEvent {
	const from, target = domain.Nick("a"), domain.ChannelName("#room")

	bodyLen := tokensEach*bytesPerEstimatedToken - len(from) - len(target)

	out := make([]domain.StoredEvent, n)
	for i := range out {
		out[i] = domain.StoredEvent{Event: domain.Message{
			Target: target,
			From:   from,
			Body:   strings.Repeat("x", bodyLen),
			At:     at.Add(time.Duration(i) * time.Second),
		}}
	}

	return out
}

// tokenSizedMessages is [tokenSizedEvents] for a dispatch turn's
// pre-rendered trigger block.
func tokenSizedMessages(n, tokensEach int, at time.Time) []protocol.IRCMessage {
	const from, target = "a", "#room"

	bodyLen := tokensEach*bytesPerEstimatedToken - len(from) - len(target)

	out := make([]protocol.IRCMessage, n)
	for i := range out {
		out[i] = protocol.IRCMessage{
			Kind:   protocol.KindPrivMsg,
			From:   from,
			Target: target,
			Body:   strings.Repeat("x", bodyLen),
			At:     at.Add(time.Duration(i) * time.Second),
		}
	}

	return out
}

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
		h.appendReply(domain.StoredEvent{Event: domain.SystemNotice{At: base.Add(time.Duration(i) * time.Second)}})
	}

	want := make([]domain.StoredEvent, modelHistorySize)
	for i := range want {
		at := base.Add(time.Duration(i+3) * time.Second)
		want[i] = domain.StoredEvent{Event: domain.SystemNotice{At: at}}
	}

	require.Equal(t, want, h.snapshotReplies())
}

// TestTokenBudgetForContextLen covers deriving a per-turn transcript
// token budget from a model's context length: enough headroom is
// reserved for the system prompt, tool schemas and the model's own
// reply that the budget is never the entire window, and a model with
// a very small context window still gets a usable, positive floor.
func TestTokenBudgetForContextLen(t *testing.T) {
	tests := []struct {
		name       string
		contextLen int
		want       int
	}{
		{"large context reserves headroom", 128_000, 128_000 - turnHeadroomTokens},
		{"small context still gets the floor", 2_000, minTranscriptTokenBudget},
		{"context below the floor still gets the floor", 500, minTranscriptTokenBudget},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tokenBudgetForContextLen(tc.contextLen))
		})
	}
}

// TestHistory_snapshot_is_never_token_trimmed pins that neither
// snapshot method applies the token budget on its own: composing the
// budget across history, replies and triggers is
// [composeTranscriptBudget]'s job, done once the dispatch turn holds
// all three. A snapshot method trimming independently is exactly the
// bug this design fixes — each buffer could look correctly bounded
// on its own while the three together still overran the model's
// context.
func TestHistory_snapshot_is_never_token_trimmed(t *testing.T) {
	t.Parallel()

	const target = domain.ChannelName("#room")

	at := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	older := domain.Message{Target: target, From: "alice", Body: "an older message", At: at}
	newer := domain.Message{Target: target, From: "alice", Body: "a newer message", At: at.Add(time.Second)}

	h := newHistory()
	h.seedChannel(target, []domain.StoredEvent{{Event: older}, {Event: newer}})
	h.seedReplies([]domain.StoredEvent{{Event: older}, {Event: newer}})
	h.setTokenBudget(3) // tight enough that a per-buffer trim would drop `older`

	require.Equal(t, []domain.StoredEvent{{Event: older}, {Event: newer}}, h.snapshot(target))
	require.Equal(t, []domain.StoredEvent{{Event: older}, {Event: newer}}, h.snapshotReplies())
}

// TestHistory_TokenBudget covers the getter [composeTranscriptBudget]
// reads: zero until [history.SetContextLen] sees a known context
// length, and a non-positive contextLen leaves it untouched —
// "unknown" is not the same as "tiny", and should not clamp the
// transcript to nothing.
func TestHistory_TokenBudget(t *testing.T) {
	t.Parallel()

	h := newHistory()
	require.Equal(t, 0, h.TokenBudget())

	h.SetContextLen(0)
	h.SetContextLen(-1)
	require.Equal(t, 0, h.TokenBudget())

	h.SetContextLen(128_000)
	require.Equal(t, tokenBudgetForContextLen(128_000), h.TokenBudget())
}

// TestTrimToTokenBudget covers the transcript trim itself: events
// are kept newest-first, and a zero or negative budget is a hard
// allocation (keep only the mandatory newest event), not a sentinel
// that disables the trim — [composeTranscriptBudget] is the only
// place that distinction is made, for the turn as a whole.
func TestTrimToTokenBudget(t *testing.T) {
	at := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	tiny := domain.StoredEvent{Event: domain.Message{Target: "#room", From: "a", Body: "hi", At: at}}
	big := domain.StoredEvent{Event: domain.Message{Target: "#room", From: "a", Body: "a much longer message body", At: at.Add(time.Second)}}

	tests := []struct {
		name   string
		events []domain.StoredEvent
		budget int
		want   []domain.StoredEvent
	}{
		{"zero budget keeps only the newest", []domain.StoredEvent{tiny, big}, 0, []domain.StoredEvent{big}},
		{"negative budget keeps only the newest", []domain.StoredEvent{tiny, big}, -100, []domain.StoredEvent{big}},
		{"empty input stays empty", nil, 100, nil},
		{"everything fits", []domain.StoredEvent{tiny, big}, 1000, []domain.StoredEvent{tiny, big}},
		{"a tight budget keeps only the newest", []domain.StoredEvent{tiny, big}, 3, []domain.StoredEvent{big}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, trimToTokenBudget(tc.events, tc.budget))
		})
	}
}

// TestComposeTranscriptBudget_disabled_without_a_known_context_len
// covers the one place the "no budget in effect" escape hatch
// belongs: a non-positive whole-turn budget returns all three pieces
// completely untouched, leaving each buffer's own [modelHistorySize]
// event-count cap as the only bound.
func TestComposeTranscriptBudget_disabled_without_a_known_context_len(t *testing.T) {
	t.Parallel()

	at := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	history := tokenSizedEvents(3, 100, at)
	replies := tokenSizedEvents(3, 100, at)
	triggers := tokenSizedMessages(3, 100, at)

	gotHistory, gotReplies, gotTriggers := composeTranscriptBudget(history, replies, triggers, 0)

	require.Equal(t, history, gotHistory)
	require.Equal(t, replies, gotReplies)
	require.Equal(t, triggers, gotTriggers)
}

// TestComposeTranscriptBudget_backstop_then_split pins the
// composition order by hand: triggers are costed and trimmed first
// against the full budget (a backstop against a burst that would
// otherwise crowd out everything else), then replies get up to half
// of what triggers left, then history gets whatever neither of the
// other two claimed — even down to zero or negative, where
// [trimToTokenBudget] still keeps that piece's mandatory single
// newest entry, never an empty result.
func TestComposeTranscriptBudget_backstop_then_split(t *testing.T) {
	t.Parallel()

	at := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	// Three messages at 4 tokens each per piece; a budget of 10
	// leaves triggers only enough room for the newest two (8 tokens),
	// and what little is left (2, then negative) still guarantees
	// replies and history their one mandatory newest entry each.
	history := tokenSizedEvents(3, 4, at)
	replies := tokenSizedEvents(3, 4, at)
	triggers := tokenSizedMessages(3, 4, at)

	gotHistory, gotReplies, gotTriggers := composeTranscriptBudget(history, replies, triggers, 10)

	require.Equal(t, triggers[1:], gotTriggers, "triggers: newest two of three, trimmed against the full budget")
	require.Equal(t, replies[2:], gotReplies, "replies: only the mandatory newest, once triggers spent nearly everything")
	require.Equal(t, history[2:], gotHistory, "history: only the mandatory newest, once replies also spent its share")
}

// TestComposeTranscriptBudget_invariant_at_floor_context is the
// invariant the composition exists to hold: history + replies +
// triggers, once composed, fit within the turn's budget — asserted
// at the floor case, the smallest budget a model with a known
// context length ever gets ([minTranscriptTokenBudget]).
func TestComposeTranscriptBudget_invariant_at_floor_context(t *testing.T) {
	t.Parallel()

	budget := tokenBudgetForContextLen(1_500)
	require.Equal(t, minTranscriptTokenBudget, budget, "1_500 - turnHeadroomTokens is below the floor")

	at := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	history := tokenSizedEvents(20, 15, at)
	replies := tokenSizedEvents(20, 15, at)
	triggers := tokenSizedMessages(50, 15, at) // 750 tokens: fits the floor budget outright

	gotHistory, gotReplies, gotTriggers := composeTranscriptBudget(history, replies, triggers, budget)

	total := sumEventTokens(gotHistory) + sumEventTokens(gotReplies) + sumMessageTokens(gotTriggers)
	require.LessOrEqual(t, total, budget)

	require.Equal(t, triggers, gotTriggers, "the whole trigger block fits the budget on its own")
	require.Equal(t, replies[len(replies)-8:], gotReplies, "the newest 8 of 20 fit what triggers left")
	require.Equal(t, history[len(history)-8:], gotHistory, "the newest 8 of 20 fit what replies left")
}

// TestComposeTranscriptBudget_full_rings_large_burst_at_8192 is the
// audit's own reproduction: full 500-event history and replies rings
// or ordinary-sized chat lines, and a 257-message burst — the size
// [drain] can coalesce from a full send-queue allowance into one
// trigger block — against an 8192-token model, the smallest context
// length in real use. Composed independently (the bug this fixes),
// the three pieces measured at 4185 + 4185 + 7967 = 16337 tokens,
// twice the model's entire window. Composed once, they fit it
// exactly.
func TestComposeTranscriptBudget_full_rings_large_burst_at_8192(t *testing.T) {
	t.Parallel()

	budget := tokenBudgetForContextLen(8_192)
	require.Equal(t, 8_192-turnHeadroomTokens, budget)

	at := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	history := tokenSizedEvents(modelHistorySize, 2, at)
	replies := tokenSizedEvents(modelHistorySize, 2, at)
	triggers := tokenSizedMessages(257, 29, at)

	gotHistory, gotReplies, gotTriggers := composeTranscriptBudget(history, replies, triggers, budget)

	total := sumEventTokens(gotHistory) + sumEventTokens(gotReplies) + sumMessageTokens(gotTriggers)
	require.LessOrEqual(t, total, budget)
	require.Equal(t, budget, total, "the chosen sizes fill the budget exactly, with nothing to spare")

	require.Equal(t, triggers[257-144:], gotTriggers, "144 of 257 triggers fit the full budget")
	require.Equal(t, replies[modelHistorySize-4:], gotReplies, "4 of 500 replies fit what triggers left")
	require.Equal(t, history[modelHistorySize-4:], gotHistory, "4 of 500 history events fit what replies left")
}

// TestHistory_snapshotReplies_is_defensive_copy proves the dispatch
// turn's snapshot does not alias the live backing array, so a
// concurrent append cannot mutate a snapshot already handed out.
func TestHistory_snapshotReplies_is_defensive_copy(t *testing.T) {
	t.Parallel()

	at := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHistory()
	h.appendReply(domain.StoredEvent{Event: domain.SystemNotice{At: at}})

	snap := h.snapshotReplies()
	h.appendReply(domain.StoredEvent{Event: domain.SystemNotice{At: at.Add(time.Second)}})

	require.Equal(t, []domain.StoredEvent{{Event: domain.SystemNotice{At: at}}}, snap)
}
