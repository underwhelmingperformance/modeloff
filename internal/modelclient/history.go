package modelclient

import (
	"context"
	"log/slog"
	"reflect"
	"sync"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// modelHistorySize caps the per-(model-client, channel) rolling
// history buffer at 500 events. The LLM's context window dictates
// this bound regardless of where the events come from.
const modelHistorySize = 500

// bytesPerEstimatedToken approximates prompt tokens from rendered
// text length using a widely-cited rule of thumb for English text
// (~4 bytes per token). It is a cheap heuristic, not a real
// tokenizer — precise enough to keep a turn's transcript within a
// comfortable band under the model's context window, not to predict
// the exact token count the API will bill.
const bytesPerEstimatedToken = 4

// turnHeadroomTokens is reserved out of a model's context window for
// the system prompt, tool schemas and the model's own reply, so the
// transcript budget [tokenBudgetForContextLen] derives leaves real
// room for the turn to complete: only the window left over after
// this reservation goes to history.
const turnHeadroomTokens = 4000

// minTranscriptTokenBudget is the smallest transcript budget
// [tokenBudgetForContextLen] returns for a model whose context
// length is known. A model with a genuinely tiny context window
// still gets a usable, positive slice of it for history, even once
// the headroom reservation is subtracted.
const minTranscriptTokenBudget = 1000

// tokenBudgetForContextLen derives the per-turn transcript token
// budget from a model's context length, reserving turnHeadroomTokens
// for everything else the turn needs and never returning less than
// minTranscriptTokenBudget.
func tokenBudgetForContextLen(contextLen int) int {
	budget := contextLen - turnHeadroomTokens
	if budget < minTranscriptTokenBudget {
		return minTranscriptTokenBudget
	}

	return budget
}

// estimateEventTokens estimates the prompt token cost of `se` from
// the size of its rendered IRC form (see [bytesPerEstimatedToken]).
// An event with no IRC rendering (e.g. one [protocol.FromChannelEvent]
// does not support) costs nothing — it will not appear in the
// assembled transcript either.
func estimateEventTokens(se domain.StoredEvent) int {
	msg, ok := protocol.FromChannelEvent(se.Event)
	if !ok {
		return 0
	}

	return estimateMessageTokens(msg)
}

// estimateMessageTokens estimates the prompt token cost of an
// already-rendered IRC message (see [bytesPerEstimatedToken]). This
// is the same estimate [estimateEventTokens] derives from a
// [domain.StoredEvent]; a dispatch turn's triggers arrive pre-
// rendered as [protocol.IRCMessage] with no stored event behind
// them, so trimming that block needs its own entry point onto the
// same heuristic.
func estimateMessageTokens(msg protocol.IRCMessage) int {
	chars := len(msg.From) + len(msg.Target) + len(msg.Subject) + len(msg.Body)

	return chars / bytesPerEstimatedToken
}

// sumEventTokens totals [estimateEventTokens] over events.
func sumEventTokens(events []domain.StoredEvent) int {
	total := 0
	for _, se := range events {
		total += estimateEventTokens(se)
	}

	return total
}

// sumMessageTokens totals [estimateMessageTokens] over msgs.
func sumMessageTokens(msgs []protocol.IRCMessage) int {
	total := 0
	for _, msg := range msgs {
		total += estimateMessageTokens(msg)
	}

	return total
}

// trimToTokenBudget drops the oldest of events until the estimated
// token cost of what remains fits within budget, always keeping at
// least the single most recent event — a transcript trimmed to
// nothing would leave the model blind to what just happened, which
// is worse than the turn running slightly over budget. budget can be
// zero or negative: that is not a sentinel for "disabled", it is a
// share so tight that only the mandatory newest event survives, and
// the loop below reaches exactly that outcome on its own (the first
// older candidate always fails the fits-in-budget check). The
// composition-wide "no budget known for this turn at all" case is
// handled by [composeTranscriptBudget], the only caller, before this
// function ever runs — this function only ever sees a genuine
// (possibly tight) allocation.
func trimToTokenBudget(events []domain.StoredEvent, budget int) []domain.StoredEvent {
	if len(events) == 0 {
		return events
	}

	kept := 1
	total := estimateEventTokens(events[len(events)-1])

	for i := len(events) - 2; i >= 0; i-- {
		cost := estimateEventTokens(events[i])
		if total+cost > budget {
			break
		}

		total += cost
		kept++
	}

	return events[len(events)-kept:]
}

// trimMessagesToTokenBudget is [trimToTokenBudget] for a slice of
// already-rendered [protocol.IRCMessage] — the shape a dispatch
// turn's trigger block arrives in. Same newest-first retention, same
// always-keep-the-newest floor, same treatment of a zero or negative
// budget as a hard-but-not-empty allocation.
func trimMessagesToTokenBudget(msgs []protocol.IRCMessage, budget int) []protocol.IRCMessage {
	if len(msgs) == 0 {
		return msgs
	}

	kept := 1
	total := estimateMessageTokens(msgs[len(msgs)-1])

	for i := len(msgs) - 2; i >= 0; i-- {
		cost := estimateMessageTokens(msgs[i])
		if total+cost > budget {
			break
		}

		total += cost
		kept++
	}

	return msgs[len(msgs)-kept:]
}

// composeTranscriptBudget spends a turn's transcript token budget
// once, across the three pieces that make it up. Trimming each of
// them independently, each against the full budget, is how a channel
// ring at 4185 tokens, a replies ring at another 4185, and an
// unbounded trigger block together overran an 8192-token model's
// entire context, even though each individual piece looked correctly
// bounded in isolation.
//
// Triggers are what the turn is about, so they are costed and
// trimmed first, against the full budget — a backstop for a
// pathological burst (see [drain], which can coalesce up to a
// channel's full send-queue allowance into one trigger block). What
// triggers leave unspent is split between replies and history:
// replies get up to half of it, and history gets whatever neither of
// the other two claimed. That remainder can be zero or negative once
// a large trigger block has spent most or all of the budget;
// [trimToTokenBudget] and [trimMessagesToTokenBudget] both handle
// that on their own, by keeping each piece's mandatory newest item —
// no special case is needed here for it.
//
// A non-positive budget for the whole turn disables every trim,
// leaving each piece's own [modelHistorySize] event-count cap as the
// only bound — the legacy behaviour for a model whose context length
// is unknown. This is the one place that check belongs: nothing
// downstream re-applies it per piece.
func composeTranscriptBudget(
	history []domain.StoredEvent,
	replies []domain.StoredEvent,
	triggers []protocol.IRCMessage,
	budget int,
) (trimmedHistory, trimmedReplies []domain.StoredEvent, trimmedTriggers []protocol.IRCMessage) {
	if budget <= 0 {
		return history, replies, triggers
	}

	trimmedTriggers = trimMessagesToTokenBudget(triggers, budget)
	remaining := budget - sumMessageTokens(trimmedTriggers)

	trimmedReplies = trimToTokenBudget(replies, remaining/2)
	remaining -= sumEventTokens(trimmedReplies)

	trimmedHistory = trimToTokenBudget(history, remaining)

	return trimmedHistory, trimmedReplies, trimmedTriggers
}

// history holds the local memory a model uses to construct each
// dispatch turn's prompt. It has two parts, both following the same
// lifecycle of load-at-attach, append-live, read-local:
//
//   - per-channel rolling buffers of the shared channel transcript.
//     Channel buffers are loaded at attach, join-scoped, by
//     [ModelClient.loadHistory]; DM targets are lazy-seeded on first
//     event arrival.
//   - a single rolling buffer of the model's own point-to-point
//     replies (its `/whois` and `/list` results). These are not
//     channel traffic and are never broadcast, so they carry no
//     channel key.
//
// All access is under `mu` so no concurrent appender can interleave
// with a seed.
type history struct {
	mu      sync.Mutex
	buf     map[domain.ChannelName][]domain.StoredEvent
	replies []domain.StoredEvent

	// maxContextTokens is the turn's transcript token budget, spent
	// once across history, replies and triggers together by
	// [composeTranscriptBudget] — not by [history.snapshot] or
	// [history.snapshotReplies] individually, which return their
	// buffer's full contents (bounded only by [modelHistorySize]) and
	// leave composition to whoever holds all three pieces. Zero — the
	// value newHistory leaves it at — means no budget is in effect
	// until [history.SetContextLen] is called.
	maxContextTokens int
}

func newHistory() *history {
	return &history{buf: make(map[domain.ChannelName][]domain.StoredEvent)}
}

// SetContextLen derives a transcript token budget from a model's
// context length (see [tokenBudgetForContextLen]) for
// [history.TokenBudget] to report. A non-positive contextLen leaves
// the current budget untouched — "unknown" is not the same as
// "tiny", and should not clamp the transcript to nothing.
func (h *history) SetContextLen(contextLen int) {
	if contextLen <= 0 {
		return
	}

	h.setTokenBudget(tokenBudgetForContextLen(contextLen))
}

// TokenBudget returns the turn's current transcript token budget —
// zero if [history.SetContextLen] has never been called with a known
// context length. The dispatch turn passes this to
// [composeTranscriptBudget] alongside the three pieces it bounds.
func (h *history) TokenBudget() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.maxContextTokens
}

// setTokenBudget sets the raw transcript token budget
// [history.TokenBudget] reports. [history.SetContextLen] is the
// production entry point; this lower-level setter exists so tests can
// pin composition behaviour against a fixed budget without going
// through the headroom and floor [tokenBudgetForContextLen] applies.
func (h *history) setTokenBudget(tokens int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.maxContextTokens = tokens
}

// seedChannel populates the buffer for `ch` with a pre-fetched slice
// of stored events. Used by [ModelClient.loadHistory] at attach to
// fill channel buffers from the event log.
func (h *history) seedChannel(ch domain.ChannelName, events []domain.StoredEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.buf[ch] = events
}

// seedReplies populates the model's own private-replies buffer with
// a pre-fetched slice. Used by [ModelClient.loadHistory] at attach to
// fill the buffer from the instance-reply log.
func (h *history) seedReplies(events []domain.StoredEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.replies = events
}

// appendReply records `ev` against the private-replies buffer. These
// are the model's `/whois` and `/list` results: its own point-to-point
// replies, kept so the model re-experiences them. The buffer trims to
// [modelHistorySize] from the older end so a chatty lookup history
// cannot grow it without bound.
func (h *history) appendReply(ev domain.StoredEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.replies = append(h.replies, ev)
	if len(h.replies) > modelHistorySize {
		h.replies = h.replies[len(h.replies)-modelHistorySize:]
	}
}

// snapshotReplies returns a defensive copy of the private-replies
// buffer, bounded only by the ring's own [modelHistorySize] event-
// count cap. The token budget is not applied here: it is spent once,
// across this buffer, [history.snapshot]'s and the turn's trigger
// block together, by [composeTranscriptBudget] — trimming each
// independently against the full budget would let the three
// overrun the model's context between them. The dispatch turn
// iterates the slice without holding the lock, so the snapshot must
// not alias the live backing array.
func (h *history) snapshotReplies() []domain.StoredEvent {
	h.mu.Lock()
	replies := h.replies
	h.mu.Unlock()

	if len(replies) == 0 {
		return nil
	}

	dst := make([]domain.StoredEvent, len(replies))
	copy(dst, replies)

	return dst
}

// snapshot returns a defensive copy of the buffer for `target`,
// bounded only by the ring's own [modelHistorySize] event-count cap
// — see [history.snapshotReplies] for where the token budget is
// actually spent. The dispatch turn iterates the slice without
// holding the lock, so the snapshot must not alias the live backing
// array.
func (h *history) snapshot(target domain.ChannelName) []domain.StoredEvent {
	h.mu.Lock()
	src := h.buf[target]
	h.mu.Unlock()

	if len(src) == 0 {
		return nil
	}

	dst := make([]domain.StoredEvent, len(src))
	copy(dst, src)

	return dst
}

// append records `ev` against `target` in the rolling buffer. The
// feeder admits only [domain.ChannelActivity], so the buffer holds
// the conversation a turn assembles its prompt from.
//
// On first sight of a DM target — `target` is a counterpart
// `InstanceID` and the buffer has no entry for it yet — the method
// lazy-seeds from the store under the same lock the live append
// takes, so no concurrent appender can interleave between seed and
// append. Channel targets are loaded at attach time; the lazy-seed
// branch is DM-only.
//
// Skips a duplicate if the incoming event matches the buffer's
// most-recent entry by concrete type and timestamp; protects
// against the seed-then-live-emit race where a producer persists
// and is mid-fan-out while a concurrent registration's seed reads
// the event from the store and then receives the same event again
// via fan-out.
//
// The buffer trims to [modelHistorySize] from the older end on
// every append so a chatty target cannot grow it without bound.
func (h *history) append(
	ctx context.Context,
	sess Session,
	selfID domain.InstanceID,
	ev domain.StoredEvent,
	target domain.ChannelName,
) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, ok := h.buf[target]; !ok && domain.InferChannelKind(target) == domain.KindDM {
		seed, err := sess.DMEventsBefore(ctx, selfID, domain.InstanceID(target), nil, modelHistorySize)
		if err != nil {
			slog.Default().ErrorContext(ctx, "lazy-seed DM history",
				"component", "modelclient",
				"instance_id", selfID,
				"peer", target,
				"error", err,
			)
			h.buf[target] = nil
		} else {
			h.buf[target] = seed
		}
	}

	if buf := h.buf[target]; len(buf) > 0 && sameStoredEvent(buf[len(buf)-1], ev) {
		return
	}

	h.buf[target] = append(h.buf[target], ev)
	if len(h.buf[target]) > modelHistorySize {
		h.buf[target] = h.buf[target][len(h.buf[target])-modelHistorySize:]
	}
}

// sameStoredEvent reports whether `a` and `b` represent the same
// persisted event. The match handles two shapes:
//
//   - Both carry an ID (both loaded from the store): the row id
//     is the canonical identity.
//   - Exactly one carries an ID: this is the seed-then-fanout
//     race shape — a registering consumer's seed read the event
//     from the store while the producer's fan-out was still in
//     flight, then the same event arrived again ID-less over the
//     bus. Same concrete type + same timestamp identifies the
//     pair.
//
// When both ids are zero the events arrived through separate
// append paths (one from the dispatch loop, one from the model-
// client's own send) and are kept as distinct entries.
func sameStoredEvent(a, b domain.StoredEvent) bool {
	if a.ID != 0 && b.ID != 0 {
		return a.ID == b.ID
	}

	if a.ID == 0 && b.ID == 0 {
		return false
	}

	if a.Event == nil || b.Event == nil {
		return false
	}

	if reflect.TypeOf(a.Event) != reflect.TypeOf(b.Event) {
		return false
	}

	return domain.EventTime(a.Event).Equal(domain.EventTime(b.Event))
}
