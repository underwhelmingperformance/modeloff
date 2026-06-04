package modelclient

import (
	"context"
	"log/slog"
	"reflect"
	"sync"

	"github.com/laney/modeloff/internal/domain"
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
}

func newHistory() *history {
	return &history{buf: make(map[domain.ChannelName][]domain.StoredEvent)}
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
// are the model's `/whois` and `/list` results, which are
// `!ModelVisible` by design and must be kept, so no model-visibility
// filter applies here. The buffer trims to [modelHistorySize] from
// the older end so a chatty lookup history cannot grow it without
// bound.
func (h *history) appendReply(ev domain.StoredEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.replies = append(h.replies, ev)
	if len(h.replies) > modelHistorySize {
		h.replies = h.replies[len(h.replies)-modelHistorySize:]
	}
}

// snapshotReplies returns a defensive copy of the private-replies
// buffer. The dispatch turn iterates the slice without holding the
// lock, so the snapshot must not alias the live backing array.
func (h *history) snapshotReplies() []domain.StoredEvent {
	h.mu.Lock()
	defer h.mu.Unlock()

	if len(h.replies) == 0 {
		return nil
	}

	dst := make([]domain.StoredEvent, len(h.replies))
	copy(dst, h.replies)
	return dst
}

// snapshot returns a defensive copy of the buffer for `target`. The
// dispatch turn iterates the slice without holding the lock, so the
// snapshot must not alias the live backing array.
func (h *history) snapshot(target domain.ChannelName) []domain.StoredEvent {
	h.mu.Lock()
	defer h.mu.Unlock()

	src := h.buf[target]
	if len(src) == 0 {
		return nil
	}

	dst := make([]domain.StoredEvent, len(src))
	copy(dst, src)
	return dst
}

// append records `ev` against `target` in the rolling buffer. Events
// the LLM never sees in its prompt (`!ModelVisible`) are skipped so
// the buffer's trim cap reflects turns of conversation rather than
// wire chatter.
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
	if !ev.Event.ModelVisible() {
		return
	}

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
