package protocol

import "go.opentelemetry.io/otel/trace"

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
// Domain types stay free of observability metadata: the protocol
// package owns the envelope; the persistence layer (`AppendEvent`
// / `EventsBefore`) sees the inner [Event] only.
type Delivery struct {
	Event   Event
	SpanCtx trace.SpanContext
}
