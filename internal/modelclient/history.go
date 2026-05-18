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

// history holds the per-channel rolling buffer this model uses to
// construct each dispatch turn's prompt. Channels are eager-seeded
// at attach via [ModelClient.seedHistory]; DM targets are lazy-
// seeded on first event arrival, both under `historyMu` so no
// concurrent appender can interleave with a seed.
type history struct {
	mu  sync.Mutex
	buf map[domain.ChannelName][]domain.StoredEvent
}

func newHistory() *history {
	return &history{buf: make(map[domain.ChannelName][]domain.StoredEvent)}
}

// seedChannel populates the buffer for `ch` with a pre-fetched slice
// of stored events. Used by [ModelClient.seedHistory] at attach to
// fill channel buffers from the event log.
func (h *history) seedChannel(ch domain.ChannelName, events []domain.StoredEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.buf[ch] = events
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
// append. Channel targets are eager-seeded at attach time; the
// lazy-seed branch is DM-only.
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
// persisted event. The store-loaded form (from `EventsBefore` /
// `DMEventsBefore`) carries the row's ID; the fan-out form is
// constructed without the ID since the wire layer does not
// propagate it. Compare on the (type, timestamp) tuple instead:
// two events of the same concrete type at the same nanosecond
// timestamp are not realistically distinct, and a storeload-then-
// fanout duplicate has both attributes identical by construction.
func sameStoredEvent(a, b domain.StoredEvent) bool {
	if a.ID != 0 && b.ID != 0 {
		return a.ID == b.ID
	}

	if a.Event == nil || b.Event == nil {
		return false
	}

	if reflect.TypeOf(a.Event) != reflect.TypeOf(b.Event) {
		return false
	}

	return domain.EventTime(a.Event).Equal(domain.EventTime(b.Event))
}
