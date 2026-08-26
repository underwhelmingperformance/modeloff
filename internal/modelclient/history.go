package modelclient

import (
	"context"
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
// bounded by the ring's [modelHistorySize] event-count cap. The
// dispatch turn iterates the slice without holding the lock, so the
// snapshot must not alias the live backing array.
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
