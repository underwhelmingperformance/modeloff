package protocol

import (
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
)

// Delivery is the wire-format envelope for a server-to-client
// event. It carries the [Event] payload plus an optional
// [trace.SpanContext] capturing the originating handler's span at
// emit time. Consumers (model-client dispatch goroutines) link
// each turn's span to the carried context so trace continuity is
// preserved across the channel-based delivery boundary, where the
// goroutine cannot inherit the producer's `context.Context`
// directly.
//
// `SpanCtx` is zero-valued when the producer's context carried no
// span (typically tests). Consumers gate on
// [trace.SpanContext.IsValid] before constructing a link.
//
// `Targets` carries the per-recipient channel list for
// actor-scoped events ([domain.Quit] and [domain.NickChange]),
// computed at fan-out time as the intersection of the actor's
// channel set with the recipient's. Other events leave it nil:
// their addressable channel is on the [Event] payload itself.
// Carrying the list on the envelope rather than on the event
// keeps the wire payload free of the actor's full channel set
// (RFC 2812 §3.1.7 and §3.1.2 are actor-scoped notices with no
// target field), so no peer learns membership it does not
// already share.
//
// Domain types stay free of observability metadata: the protocol
// package owns the envelope; the persistence layer (`AppendEvent`
// / `EventsBefore`) sees the inner [Event] only.
type Delivery struct {
	Event   Event
	Targets []domain.ChannelName
	SpanCtx trace.SpanContext
}
