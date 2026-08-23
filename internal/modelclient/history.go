package modelclient

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
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
	msg.Source = msg.Source.WithoutInstanceID()
	rendered, _ := json.Marshal(msg)

	return len(rendered) / bytesPerEstimatedToken
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
// handled before either trimming helper runs. This function only
// receives a genuine, possibly tight, allocation.
func trimToTokenBudget(events []domain.StoredEvent, budget int) []domain.StoredEvent {
	return trimToTokenBudgetWith(events, budget, estimateEventTokens)
}

func trimToTokenBudgetWith(
	events []domain.StoredEvent,
	budget int,
	estimate func(domain.StoredEvent) int,
) []domain.StoredEvent {
	if len(events) == 0 {
		return events
	}

	kept := 1
	total := estimate(events[len(events)-1])

	for i := len(events) - 2; i >= 0; i-- {
		cost := estimate(events[i])
		if total+cost > budget {
			break
		}

		total += cost
		kept++
	}

	return events[len(events)-kept:]
}

// trimMessagesToTokenBudget keeps the newest part of the current
// burst within budget. It normally retains a contiguous suffix. When
// that suffix would omit the newest dispatch trigger, it prepends the
// trigger to the suffix. The provider then sees why the turn ran and
// the newest state reached after it, even when those two mandatory
// entries exceed the remaining budget.
func trimMessagesToTokenBudgetWith(
	msgs []protocol.IRCMessage,
	latestTrigger int,
	budget int,
	estimate func(protocol.IRCMessage) int,
) []protocol.IRCMessage {
	if len(msgs) == 0 {
		return msgs
	}
	if latestTrigger < 0 || latestTrigger >= len(msgs) {
		latestTrigger = len(msgs) - 1
	}

	kept := 1
	total := estimate(msgs[len(msgs)-1])

	for i := len(msgs) - 2; i >= 0; i-- {
		cost := estimate(msgs[i])
		if total+cost > budget {
			break
		}

		total += cost
		kept++
	}

	start := len(msgs) - kept
	if start <= latestTrigger {
		return msgs[start:]
	}

	trimmed := make([]protocol.IRCMessage, 0, 1+kept)
	trimmed = append(trimmed, msgs[latestTrigger])

	return append(trimmed, msgs[start:]...)
}

func trimRepliesToTokenBudget(replies []storedReply, budget int) []storedReply {
	return trimRepliesToTokenBudgetWith(replies, budget, func(reply storedReply) int {
		return estimateEventTokens(reply.event)
	})
}

func trimRepliesToTokenBudgetWith(
	replies []storedReply,
	budget int,
	estimate func(storedReply) int,
) []storedReply {
	if len(replies) == 0 {
		return replies
	}

	kept := 1
	total := estimate(replies[len(replies)-1])

	for i := len(replies) - 2; i >= 0; i-- {
		cost := estimate(replies[i])
		if total+cost > budget {
			break
		}

		total += cost
		kept++
	}

	return replies[len(replies)-kept:]
}

func sumReplyTokens(replies []storedReply) int {
	return sumReplyTokensWith(replies, func(reply storedReply) int {
		return estimateEventTokens(reply.event)
	})
}

func sumReplyTokensWith(replies []storedReply, estimate func(storedReply) int) int {
	total := 0
	for _, reply := range replies {
		total += estimate(reply)
	}

	return total
}

// composeTranscriptBudget spends a turn's transcript token budget
// once, across the four pieces that make it up. Trimming each of
// them independently, each against the full budget, is how a channel
// ring at 4185 tokens, a replies ring at another 4185, and an
// unbounded current-event block together overran an 8192-token model's
// entire context, even though each individual piece looked correctly
// bounded in isolation.
//
// `contextLines` is what [contextReplies] renders: the channel topic
// and the instance's memories. The turn always carries them whole,
// so they are never trimmed; their cost comes off the top and the
// other three are apportioned out of what is left.
// [capMemoriesForPrompt] is what keeps that first charge bounded.
//
// Current events are what the turn is about, so they are costed and
// trimmed next, against everything the context lines left. The newest
// dispatch trigger is mandatory even when sender history or a state
// change follows it. This is a backstop for a pathological burst (see
// [drain], which admits at most [modelHistorySize] queued deliveries
// to one batch). What current events leave unspent is split between
// replies and history: replies get up to half of it, and history gets
// whatever neither of the other two claimed. Any of those shares can
// be zero or negative once an earlier piece has spent most or all of
// the budget; [trimToTokenBudget] and [trimMessagesToTokenBudget]
// both handle that on their own. Each historical piece retains its
// newest item, and the current block retains its newest dispatch
// trigger.
//
// A non-positive budget for the whole turn disables every trim,
// leaving each piece's own [modelHistorySize] event-count cap as the
// only bound — the legacy behaviour for a model whose context length
// is unknown. This is the one place that check belongs: nothing
// downstream re-applies it per piece.
func composeTranscriptBudget(
	contextLines []protocol.IRCMessage,
	history []domain.StoredEvent,
	replies []storedReply,
	events []protocol.IRCMessage,
	latestTrigger int,
	budget int,
) ([]domain.StoredEvent, []storedReply, []protocol.IRCMessage) {
	return composeProjectedTranscriptBudget(
		providerTargetProjection{}, contextLines, history, replies, events, latestTrigger, budget,
	)
}

func composeProjectedTranscriptBudget(
	projection providerTargetProjection,
	contextLines []protocol.IRCMessage,
	history []domain.StoredEvent,
	replies []storedReply,
	events []protocol.IRCMessage,
	latestTrigger int,
	budget int,
) ([]domain.StoredEvent, []storedReply, []protocol.IRCMessage) {
	if budget <= 0 {
		return history, replies, events
	}

	estimateMessage := func(message protocol.IRCMessage) int {
		return estimateMessageTokens(projection.message(message))
	}
	estimateEvent := func(event domain.StoredEvent) int {
		message, ok := protocol.FromChannelEvent(event.Event)
		if !ok {
			return 0
		}

		return estimateMessage(message)
	}
	estimateReply := func(reply storedReply) int {
		message, ok := protocol.FromChannelEvent(reply.event.Event)
		if !ok {
			return 0
		}

		return estimateMessageTokens(projection.reply(reply.window, message))
	}

	contextCost := 0
	for _, message := range contextLines {
		contextCost += estimateMessage(message)
	}
	budget -= contextCost

	trimmedEvents := trimMessagesToTokenBudgetWith(events, latestTrigger, budget, estimateMessage)
	remaining := budget
	for _, message := range trimmedEvents {
		remaining -= estimateMessage(message)
	}

	trimmedReplies := trimRepliesToTokenBudgetWith(replies, remaining/2, estimateReply)
	remaining -= sumReplyTokensWith(trimmedReplies, estimateReply)

	trimmedHistory := trimToTokenBudgetWith(history, remaining, estimateEvent)

	return trimmedHistory, trimmedReplies, trimmedEvents
}

// history holds the local memory a model uses to construct each
// dispatch turn's prompt. It has two parts, both following the same
// lifecycle of load-at-attach, append-live, read-local:
//
//   - per-channel rolling buffers of the shared channel transcript.
//     Channel buffers are loaded at attach, join-scoped, by
//     [ModelClient.loadHistory]; DM targets are lazy-seeded on first
//     event arrival.
//   - a rolling buffer of the model's own point-to-point replies
//     (its `/whois` and `/list` results). Each reply records the
//     window in which the model issued the command so closing that
//     window removes the derived context.
//
// All access is under `mu` so no concurrent appender can interleave
// with a seed.
type history struct {
	mu           sync.Mutex
	buf          map[domain.ChannelName][]domain.StoredEvent
	unseeded     map[domain.ChannelName][]domain.StoredEvent
	replies      []storedReply
	subscription protocol.Subscription

	// maxContextTokens is the turn's transcript token budget, spent
	// once across history, replies and triggers together by
	// [composeTranscriptBudget] — not by [history.snapshot] or
	// [history.snapshotRepliesFor] individually, which return their
	// buffer's full contents (bounded only by [modelHistorySize]) and
	// leave composition to whoever holds all three pieces. Zero — the
	// value newHistory leaves it at — means no budget is in effect
	// until [history.SetContextLen] is called.
	maxContextTokens int
}

type storedReply struct {
	window protocol.WindowTarget
	event  domain.StoredEvent
}

func newHistory() *history {
	return &history{
		buf:      make(map[domain.ChannelName][]domain.StoredEvent),
		unseeded: make(map[domain.ChannelName][]domain.StoredEvent),
	}
}

func (h *history) bind(subscription protocol.Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.subscription = subscription
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

func (h *history) forget(target domain.ChannelName) {
	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.buf, target)
	delete(h.unseeded, target)
	window := protocol.WindowTargetForKey(target)
	h.replies = slices.DeleteFunc(h.replies, func(reply storedReply) bool {
		return protocol.EqualWindowTarget(reply.window, window)
	})
}

// seedReplies populates the model's own private-replies buffer with
// a pre-fetched slice. Used by [ModelClient.loadHistory] at attach to
// fill the buffer from the instance-reply log.
func (h *history) seedReplies(window protocol.WindowTarget, entries []protocol.ReplyEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seedRepliesLocked(window, entries)
}

func (h *history) seedRepliesLocked(window protocol.WindowTarget, entries []protocol.ReplyEntry) {
	h.replies = slices.DeleteFunc(h.replies, func(reply storedReply) bool {
		return protocol.EqualWindowTarget(reply.window, window)
	})
	for _, entry := range entries {
		h.replies = append(h.replies, storedReply{
			window: window,
			event:  domain.StoredEvent{Event: entry.Event},
		})
	}
	h.sortRepliesLocked()
}

func (h *history) sortRepliesLocked() {
	slices.SortStableFunc(h.replies, func(a, b storedReply) int {
		return domain.EventTime(a.event.Event).Compare(domain.EventTime(b.event.Event))
	})
}

// appendReply records `ev` against the private-replies buffer. These
// are the model's `/whois` and `/list` results: its own point-to-point
// replies, kept so the model re-experiences them. Each issuing window
// trims to [modelHistorySize] from its older end. Traffic in another
// window therefore cannot evict the replies this window needs.
func (h *history) appendReply(window protocol.WindowTarget, ev domain.StoredEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.replies = append(h.replies, storedReply{window: window, event: ev})
	h.sortRepliesLocked()
	matching := 0
	for i, reply := range slices.Backward(h.replies) {
		if !protocol.EqualWindowTarget(reply.window, window) {
			continue
		}
		matching++
		if matching <= modelHistorySize {
			continue
		}

		h.replies = slices.Delete(h.replies, i, i+1)
		break
	}
}

// snapshotRepliesFor returns a defensive copy of the replies `target`
// may read: the ones issued in that window and the ones issued in no
// window at all. Each issuing window has its own [modelHistorySize]
// event cap. The dispatch turn iterates the slice without holding the
// lock, so the snapshot must not alias the live backing array.
func (h *history) snapshotRepliesFor(target domain.ChannelName) []storedReply {
	h.mu.Lock()
	defer h.mu.Unlock()

	window := protocol.WindowTargetForKey(target)
	replies := make([]storedReply, 0, len(h.replies))
	for _, reply := range h.replies {
		if reply.window != nil && !protocol.EqualWindowTarget(reply.window, window) {
			continue
		}

		replies = append(replies, reply)
	}
	if len(replies) == 0 {
		return nil
	}

	return replies
}

// snapshot returns a defensive copy of the buffer for `target`,
// bounded only by the ring's own [modelHistorySize] event-count cap
// — see [history.snapshotRepliesFor] for where the token budget is
// actually spent. The dispatch turn iterates the slice without
// holding the lock, so the snapshot must not alias the live backing
// array.
//
// A DM buffer this client has not seen yet is loaded from the store
// before the copy is taken, under the one lock, so a snapshot never
// reports a conversation as empty because its load had not run.
func (h *history) snapshot(
	ctx context.Context,
	target domain.ChannelName,
) ([]domain.StoredEvent, error) {
	h.mu.Lock()
	if err := h.seedDM(ctx, target); err != nil {
		h.mu.Unlock()
		return nil, err
	}
	dst := slices.Clone(h.buf[target])
	h.mu.Unlock()
	if len(dst) == 0 {
		return nil, nil
	}

	return dst, nil
}

// append records `ev` against `target` in the rolling buffer. The
// feeder admits only [domain.ChannelActivity], so the buffer holds
// the conversation a turn assembles its prompt from.
//
// A DM buffer this client has not seen yet is loaded from the store
// first, under the same lock the live append takes, so no concurrent
// appender can interleave between the load and the append.
//
// The buffer trims to [modelHistorySize] from the older end on
// every append so a chatty target cannot grow it without bound.
func (h *history) append(
	ctx context.Context,
	ev domain.StoredEvent,
	target domain.ChannelName,
) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if err := h.seedDM(ctx, target); err != nil {
		h.appendUnseededLocked(target, ev)
		return err
	}

	h.buf[target] = appendAndTrim(h.buf[target], ev)

	return nil
}

// appendAfterSeedFailure retains another live event from a batch
// whose first event could not seed its DM history. The next batch
// retries the seed and merges these events after the stored prefix.
func (h *history) appendAfterSeedFailure(
	target domain.ChannelName,
	ev domain.StoredEvent,
) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, seeded := h.buf[target]; seeded {
		h.buf[target] = appendAndTrim(h.buf[target], ev)
		return
	}

	h.appendUnseededLocked(target, ev)
}

func (h *history) appendUnseededLocked(
	target domain.ChannelName,
	ev domain.StoredEvent,
) {
	h.unseeded[target] = appendAndTrim(h.unseeded[target], ev)
}

func appendAndTrim(events []domain.StoredEvent, ev domain.StoredEvent) []domain.StoredEvent {
	events = append(events, ev)
	if len(events) > modelHistorySize {
		events = events[len(events)-modelHistorySize:]
	}

	return events
}

// seedDM fills the buffer for a DM window this client has not seen
// yet from the persisted thread between it and `target`, which is the
// counterpart (see [domain.Message.RoutingKey]). Channel buffers are
// loaded at attach by [ModelClient.loadHistory], so this covers the
// DM windows that have no attach-time list to load from.
//
// The caller holds `h.mu`, which is what makes a load and the read or
// append it precedes one step: a concurrent appender cannot land
// between them, and a turn cannot snapshot a window whose load is
// half done. The buffer and its replies become visible together only
// after both reads succeed. A later delivery can therefore retry a
// failed load without mistaking partial data for a complete seed.
func (h *history) seedDM(ctx context.Context, target domain.ChannelName) error {
	if _, ok := h.buf[target]; ok {
		return nil
	}

	if domain.InferChannelKind(target) != domain.KindDM {
		return nil
	}

	if h.subscription == nil {
		return protocol.ErrSubscriptionClosed
	}

	replies, err := h.subscription.Replies(
		ctx, protocol.DirectWindowTarget(domain.InstanceID(target)), modelHistorySize,
	)
	if err != nil {
		return fmt.Errorf("load DM replies for %q: %w", target, err)
	}

	seed, err := h.subscription.Scrollback(ctx, protocol.WindowTargetForKey(target), modelHistorySize)
	if err != nil {
		return fmt.Errorf("load DM scrollback for %q: %w", target, err)
	}

	h.buf[target] = append(storedScrollback(seed), h.unseeded[target]...)
	if len(h.buf[target]) > modelHistorySize {
		h.buf[target] = h.buf[target][len(h.buf[target])-modelHistorySize:]
	}
	delete(h.unseeded, target)
	h.seedRepliesLocked(protocol.DirectWindowTarget(domain.InstanceID(target)), replies)

	return nil
}
