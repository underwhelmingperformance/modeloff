package modelclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/protocol"
)

// tokenSizedEvents builds n domain.Message-backed StoredEvents that
// [estimateEventTokens] costs at exactly tokensEach. Events start at
// `at` and advance by one second, oldest first, which matches the
// order in a history ring.
func tokenSizedEvents(n, tokensEach int, at time.Time) []domain.StoredEvent {
	const from, target = domain.Nick("a"), domain.ChannelName("#room")

	out := make([]domain.StoredEvent, n)
	for i := range out {
		event := domain.StoredEvent{Event: domain.Message{
			Source: domain.LegacyClientSource(from), Target: target,
			At: at.Add(time.Duration(i) * time.Second),
		}}
		for estimateEventTokens(event) < tokensEach {
			message := event.Event.(domain.Message)
			message.Body += "x"
			event.Event = message
		}
		if estimateEventTokens(event) != tokensEach {
			panic("requested token cost is smaller than the rendered event")
		}

		out[i] = event
	}

	return out
}

func unscopedReplies(events []domain.StoredEvent) []storedReply {
	replies := make([]storedReply, len(events))
	for i, event := range events {
		replies[i] = storedReply{event: event}
	}

	return replies
}

// tokenSizedMessages is [tokenSizedEvents] for a dispatch turn's
// pre-rendered trigger block.
func tokenSizedMessages(n, tokensEach int, at time.Time) []protocol.IRCMessage {
	const from, target = "a", "#room"

	out := make([]protocol.IRCMessage, n)
	for i := range out {
		message := protocol.IRCMessage{
			Kind: protocol.KindPrivMsg, Source: domain.LegacyClientSource(from),
			Target: target, At: at.Add(time.Duration(i) * time.Second),
		}
		for estimateMessageTokens(message) < tokensEach {
			message.Body += "x"
		}
		if estimateMessageTokens(message) != tokensEach {
			panic("requested token cost is smaller than the rendered message")
		}

		out[i] = message
	}

	return out
}

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
	require.Equal(t, []storedReply{
		{window: protocol.ChannelWindowTarget("#dev"), event: domain.StoredEvent{Event: windowReply}},
	}, trimRepliesToTokenBudget(replies, 0))
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

	older := domain.Message{Source: domain.LegacyClientSource("alice"), Target: target, Body: "an older message", At: at}
	newer := domain.Message{Source: domain.LegacyClientSource("alice"), Target: target, Body: "a newer message", At: at.Add(time.Second)}
	olderReply := domain.SystemNotice{Target: target, Text: "an older reply", At: at}
	newerReply := domain.SystemNotice{Target: target, Text: "a newer reply", At: at.Add(time.Second)}

	h := newHistory()
	h.seedChannel(target, []domain.StoredEvent{{Event: older}, {Event: newer}})
	h.seedReplies(nil, []protocol.ReplyEntry{{Event: olderReply}, {Event: newerReply}})
	h.setTokenBudget(3) // tight enough that a per-buffer trim would drop `older`

	snapshot, err := h.snapshot(context.Background(), target)
	require.NoError(t, err)

	require.Equal(t, []domain.StoredEvent{{Event: older}, {Event: newer}}, snapshot)
	require.Equal(t, []storedReply{
		{event: domain.StoredEvent{Event: olderReply}},
		{event: domain.StoredEvent{Event: newerReply}},
	}, h.snapshotRepliesFor(target))
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

	tiny := domain.StoredEvent{Event: domain.Message{Source: domain.LegacyClientSource("a"), Target: "#room", Body: "hi", At: at}}
	big := domain.StoredEvent{Event: domain.Message{Source: domain.LegacyClientSource("a"), Target: "#room", Body: "a much longer message body", At: at.Add(time.Second)}}

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
	replies := unscopedReplies(tokenSizedEvents(3, 100, at))
	triggers := tokenSizedMessages(3, 100, at)

	gotHistory, gotReplies, gotTriggers := composeTranscriptBudget(
		nil, history, replies, triggers, len(triggers)-1, 0,
	)

	require.Equal(t, history, gotHistory)
	require.Equal(t, replies, gotReplies)
	require.Equal(t, triggers, gotTriggers)
}

func TestComposeTranscriptBudget_keeps_state_after_the_mandatory_trigger(t *testing.T) {
	t.Parallel()

	at := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	trigger := tokenSizedMessages(1, 40, at)[0]
	part := protocol.IRCMessage{
		Kind: protocol.KindPart, Source: domain.LegacyClientSource("alice"),
		Target: "#room", At: at.Add(time.Second),
	}
	events := []protocol.IRCMessage{trigger, part}

	history, replies, current := composeTranscriptBudget(
		nil, nil, nil, events, 0, estimateMessageTokens(trigger),
	)

	require.Equal(t, struct {
		History []domain.StoredEvent
		Replies []storedReply
		Current []protocol.IRCMessage
	}{
		Current: events,
	}, struct {
		History []domain.StoredEvent
		Replies []storedReply
		Current []protocol.IRCMessage
	}{
		History: history,
		Replies: replies,
		Current: current,
	})
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

	// Three messages at 40 tokens each per piece; a budget of 100
	// leaves triggers only enough room for the newest two (80 tokens),
	// and what little is left (20, then negative) still guarantees
	// replies and history their one mandatory newest entry each.
	history := tokenSizedEvents(3, 40, at)
	replies := unscopedReplies(tokenSizedEvents(3, 40, at))
	triggers := tokenSizedMessages(3, 40, at)

	gotHistory, gotReplies, gotTriggers := composeTranscriptBudget(
		nil, history, replies, triggers, len(triggers)-1, 100,
	)

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

	history := tokenSizedEvents(20, 40, at)
	replies := unscopedReplies(tokenSizedEvents(20, 40, at))
	triggers := tokenSizedMessages(20, 45, at) // 900 tokens: fits the floor budget outright

	gotHistory, gotReplies, gotTriggers := composeTranscriptBudget(
		nil, history, replies, triggers, len(triggers)-1, budget,
	)

	total := sumEventTokens(gotHistory) + sumReplyTokens(gotReplies) + sumMessageTokens(gotTriggers)
	require.LessOrEqual(t, total, budget)

	require.Equal(t, triggers, gotTriggers, "the whole trigger block fits the budget on its own")
	require.Equal(t, replies[len(replies)-1:], gotReplies, "the newest reply fits its share")
	require.Equal(t, history[len(history)-1:], gotHistory, "the newest history event fits what replies left")
}

func TestComposeTranscriptBudget_covers_the_rendered_provider_body(t *testing.T) {
	t.Parallel()

	const contextLength = 8192
	const historySize = 250

	at := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	history := tokenSizedEvents(historySize, 40, at)
	current := tokenSizedMessages(1, 40, at.Add(historySize*time.Second))

	history, _, current = composeTranscriptBudget(
		nil,
		history,
		nil,
		current,
		0,
		tokenBudgetForContextLen(contextLength),
	)

	window := domain.NewChannelWindow("#room", at)
	instance := domain.NewModelInstance(
		"inst-botty",
		"botty",
		"test/model",
		"",
		nil,
	)
	prompt := buildSystemPrompt(testChannelContext(window), instance.Nick(), instance.Persona())
	renderedHistory := renderTurnHistory(
		history,
		nil,
		nil,
		providerTargetProjection{},
	)
	request, err := api.RenderEventRequest(
		instance.ModelID,
		instance.ID(),
		prompt,
		renderedHistory,
		current,
	)
	require.NoError(t, err)

	var body struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(request.Body, &body))

	contentBytes := 0
	for _, message := range body.Messages {
		var text string
		if json.Unmarshal(message.Content, &text) == nil {
			contentBytes += len(text)
			continue
		}

		var parts []struct {
			Text string `json:"text"`
		}
		require.NoError(t, json.Unmarshal(message.Content, &parts))

		for _, part := range parts {
			contentBytes += len(part.Text)
		}
	}

	require.LessOrEqual(
		t,
		contentBytes,
		contextLength*bytesPerEstimatedToken,
	)
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

	history := tokenSizedEvents(modelHistorySize, 40, at)
	replies := unscopedReplies(tokenSizedEvents(modelHistorySize, 40, at))
	triggers := tokenSizedMessages(257, 100, at)

	gotHistory, gotReplies, gotTriggers := composeTranscriptBudget(
		nil, history, replies, triggers, len(triggers)-1, budget,
	)

	total := sumEventTokens(gotHistory) + sumReplyTokens(gotReplies) + sumMessageTokens(gotTriggers)
	require.LessOrEqual(t, total, budget)
	require.Equal(t, 4_180, total)

	require.Equal(t, triggers[257-41:], gotTriggers, "41 of 257 triggers fit the full budget")
	require.Equal(t, replies[modelHistorySize-1:], gotReplies, "the newest reply fits what triggers left")
	require.Equal(t, history[modelHistorySize-1:], gotHistory, "the newest history event fits what replies left")
}

func TestComposeTranscriptBudget_accounts_for_projected_dm_nicks(t *testing.T) {
	t.Parallel()

	at := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	userNick := domain.Nick(strings.Repeat("u", domain.NickMaxLen))
	history := make([]domain.StoredEvent, modelHistorySize-1)
	for i := range history {
		history[i] = domain.StoredEvent{ID: int64(i + 1), Event: domain.Message{
			Source: domain.ClientSource("inst-botty", "botty"),
			Target: domain.ChannelName(protocol.UserClientID), Body: strings.Repeat("m", 27),
			At: at.Add(time.Duration(i) * time.Second),
		}}
	}
	events := []protocol.IRCMessage{{
		Kind: protocol.KindPrivMsg, Source: domain.ClientSource(protocol.UserClientID, userNick),
		Target: "inst-botty", Body: strings.Repeat("m", 27),
		At: at.Add(modelHistorySize * time.Second),
	}}
	projection := providerTargetProjection{
		direct: true,
		selfID: "inst-botty", selfNick: "botty",
		peerID: protocol.UserClientID, peerNick: userNick,
	}
	budget := tokenBudgetForContextLen(8_192)

	gotHistory, gotReplies, gotEvents := composeProjectedTranscriptBudget(
		projection, nil, history, nil, events, 0, budget,
	)
	projectedHistory := make([]protocol.IRCMessage, len(gotHistory))
	for i, event := range gotHistory {
		message, ok := protocol.FromChannelEvent(event.Event)
		require.True(t, ok)
		projectedHistory[i] = projection.message(message)
	}
	projectedEvents := projection.messages(gotEvents)
	projectedTokens := sumMessageTokens(projectedHistory) + sumMessageTokens(projectedEvents)

	require.Equal(t, struct {
		History         []domain.StoredEvent
		Replies         []storedReply
		Events          []protocol.IRCMessage
		ProjectedTokens int
		Budget          int
		Fits            bool
	}{
		History:         history[len(history)-101:],
		Events:          events,
		ProjectedTokens: 4_182,
		Budget:          4_192,
		Fits:            true,
	}, struct {
		History         []domain.StoredEvent
		Replies         []storedReply
		Events          []protocol.IRCMessage
		ProjectedTokens int
		Budget          int
		Fits            bool
	}{
		History: gotHistory, Replies: gotReplies, Events: gotEvents,
		ProjectedTokens: projectedTokens,
		Budget:          budget,
		Fits:            projectedTokens <= budget,
	})
}

// fullContextReplies renders the largest context block
// [contextReplies] can produce: a 300-character topic, and the whole
// memory allowance [capMemoriesForPrompt] admits (50 entries whose
// key and content together come to exactly maxMemoryBytes, so
// nothing is truncated).
func fullContextReplies(t *testing.T) []protocol.IRCMessage {
	t.Helper()

	cw := domain.NewChannelWindow("#dev", time.Time{})
	cw.Topic = strings.Repeat("t", 300)
	cw.TopicSetBy = "alice"

	const keyLen = 3

	memories := make([]memory.Entry, maxMemoryEntries)
	for i := range memories {
		memories[i] = memory.Entry{
			Key:     fmt.Sprintf("k%02d", i),
			Content: strings.Repeat("x", maxMemoryBytes/maxMemoryEntries-keyLen),
		}
	}

	return contextReplies(testChannelContext(cw), memories)
}

// TestComposeTranscriptBudget_charges_the_context_lines_first covers
// the turn's one unbounded-by-position piece: the context lines are
// never trimmed, so a turn stays inside its budget only if their
// cost is taken off the top before the other three pieces are
// apportioned. Added to the composed total afterwards, a full memory
// block is roughly a quarter of an 8192-token model's transcript
// allowance, spent out of the headroom reserved for the system
// prompt, the tool schemas and the model's reply.
func TestComposeTranscriptBudget_charges_the_context_lines_first(t *testing.T) {
	t.Parallel()

	budget := tokenBudgetForContextLen(8_192)
	require.Equal(t, 8_192-turnHeadroomTokens, budget)

	at := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	contextLines := fullContextReplies(t)
	require.Equal(t, 1184, sumMessageTokens(contextLines), "a 300-character topic plus the full memory allowance")

	// Both rings hold more than the budget can take, so what each
	// one keeps is decided by the share left to it.
	history := tokenSizedEvents(modelHistorySize, 40, at)
	replies := unscopedReplies(tokenSizedEvents(modelHistorySize, 40, at))
	triggers := tokenSizedMessages(20, 50, at) // 1000 tokens: fits what the context lines left

	gotHistory, gotReplies, gotTriggers := composeTranscriptBudget(
		contextLines, history, replies, triggers, len(triggers)-1, budget,
	)

	total := sumMessageTokens(contextLines) +
		sumEventTokens(gotHistory) +
		sumReplyTokens(gotReplies) +
		sumMessageTokens(gotTriggers)
	require.LessOrEqual(t, total, budget, "everything the turn carries fits the budget, context lines included")

	// 4192 - 1184 context - 1000 triggers leaves 2008; replies take
	// half of that (25 events at 40 tokens = 1000), and history takes
	// the 1008 remaining (25 events = 1000).
	require.Equal(t, triggers, gotTriggers, "the whole trigger block fits what the context lines left")
	require.Equal(t, replies[modelHistorySize-25:], gotReplies, "the newest 25 fit half of what triggers left")
	require.Equal(t, history[modelHistorySize-25:], gotHistory, "the newest 25 fit what replies left")
}

// TestComposeTranscriptBudget_context_lines_crowd_out_the_floor is
// the same charge at the floor budget, where a full memory block
// costs more than the whole transcript allowance
// ([minTranscriptTokenBudget] is 1000 tokens and
// [maxMemoryBytes] alone is 4000 bytes). Every other piece is cut to
// the single newest entry [trimToTokenBudget] always keeps, so a
// tiny-context model spends its transcript on what it remembers and
// sees almost nothing of the channel. That is a visible consequence
// of one allowance rather than a silent overrun of it: the two caps
// are set independently and the floor is where they meet.
func TestComposeTranscriptBudget_context_lines_crowd_out_the_floor(t *testing.T) {
	t.Parallel()

	budget := tokenBudgetForContextLen(1_500)
	require.Equal(t, minTranscriptTokenBudget, budget, "1_500 - turnHeadroomTokens is below the floor")

	at := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	contextLines := fullContextReplies(t)
	require.Greater(t, sumMessageTokens(contextLines), budget, "the context lines outrun the floor budget on their own")

	history := tokenSizedEvents(20, 40, at)
	replies := unscopedReplies(tokenSizedEvents(20, 40, at))
	triggers := tokenSizedMessages(20, 40, at)

	gotHistory, gotReplies, gotTriggers := composeTranscriptBudget(
		contextLines, history, replies, triggers, len(triggers)-1, budget,
	)

	require.Equal(t, triggers[19:], gotTriggers, "triggers: the mandatory newest and nothing more")
	require.Equal(t, replies[19:], gotReplies, "replies: the mandatory newest and nothing more")
	require.Equal(t, history[19:], gotHistory, "history: the mandatory newest and nothing more")
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
