package observability

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// SpanRunner brackets a unit of work with span lifecycle and result
// recording so component callsites carry only the work, not the
// `Tracer.Start` / `defer span.End` / `SetAttributes(ResultOK|Error)`
// scaffolding. Each component constructs one (typically lazily, from
// its injected `TracerProvider`) with its own tracer name and
// default error kind; tests' `SpanRecorder` picks up everything
// through the same injected provider.
//
// The struct is value-shaped so call sites can construct it on the
// fly without lifecycle concerns.
type SpanRunner struct {
	// Tracer is the tracer to start spans on. Construct from the
	// component's injected `TracerProvider` so tests scope spans to
	// a per-test recorder.
	Tracer trace.Tracer

	// DefaultErrKind is the value attached to `AttrErrorKind` when
	// the inner function returns a non-nil error and `ClassifyError`
	// either returns "" or is unset. Components pick the kind that
	// best describes their failure mode (`ErrorKindStore` for
	// persistence, etc.).
	DefaultErrKind string

	// ClassifyError optionally maps a returned error to a more
	// specific kind. Returning "" falls back to `DefaultErrKind`.
	// nil disables classification. Useful for cases like
	// `sql.ErrNoRows` -> `ErrorKindNotFound`.
	ClassifyError func(error) string
}

// Run starts a span named op with the given attrs (plus an
// `AttrOperation` attribute set to op), invokes fn, and records the
// result. The inner function may add further attributes via the
// supplied span handle. The span is always ended.
//
// On a non-nil return from fn, the span is annotated with
// `AttrResult=error`, `AttrErrorKind=<kind>` (resolved through
// `ClassifyError` if present, else `DefaultErrKind`), and a
// `codes.Error` status carrying the error message. On nil return,
// the span is annotated with `AttrResult=ok`.
//
// Pass nil for attrs when no per-call attributes are needed.
func (r SpanRunner) Run(
	ctx context.Context,
	op string,
	attrs []attribute.KeyValue,
	fn func(ctx context.Context, span trace.Span) error,
) error {
	ctx, span := r.Tracer.Start(ctx, op)
	defer span.End()

	startAttrs := make([]attribute.KeyValue, 0, len(attrs)+1)
	startAttrs = append(startAttrs, attribute.String(AttrOperation, op))
	startAttrs = append(startAttrs, attrs...)
	span.SetAttributes(startAttrs...)

	if err := fn(ctx, span); err != nil {
		kind := r.DefaultErrKind
		if r.ClassifyError != nil {
			if k := r.ClassifyError(err); k != "" {
				kind = k
			}
		}

		span.SetAttributes(
			attribute.String(AttrResult, ResultError),
			attribute.String(AttrErrorKind, kind),
		)
		span.SetStatus(codes.Error, err.Error())

		return err
	}

	span.SetAttributes(attribute.String(AttrResult, ResultOK))

	return nil
}
