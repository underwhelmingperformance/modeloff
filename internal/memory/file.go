package memory

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/store"
)

// StoreAdapter implements the memory Store interface by delegating
// to a store.Store's memory methods.
type StoreAdapter struct {
	store store.Store

	// tracerProvider is the OTel `TracerProvider` the adapter uses
	// for its spans. Defaults to `otel.GetTracerProvider()`; tests
	// inject a per-test recorder via `WithTracerProvider`.
	tracerProvider trace.TracerProvider
}

// NewStoreAdapter creates a memory store backed by the given data
// store.
func NewStoreAdapter(s store.Store) *StoreAdapter {
	return &StoreAdapter{
		store:          s,
		tracerProvider: otel.GetTracerProvider(),
	}
}

// WithTracerProvider overrides the OTel `TracerProvider` the adapter
// uses for its spans. Tests inject a per-test recorder so span
// recordings stay scoped to a single test rather than relying on the
// global provider's swap-and-restore.
func (a *StoreAdapter) WithTracerProvider(tp trace.TracerProvider) *StoreAdapter {
	a.tracerProvider = tp

	return a
}

// inSpan brackets fn with a span and result-recording on the adapter's
// tracer provider. See `observability.SpanRunner` for the wrapper's
// shape; underlying failures are tagged `ErrorKindStore` since the
// adapter is a thin facade over the data store.
func (a *StoreAdapter) inSpan(
	ctx context.Context,
	op string,
	attrs []attribute.KeyValue,
	fn func(ctx context.Context, span trace.Span) error,
) error {
	return observability.SpanRunner{
		Tracer:         a.tracerProvider.Tracer("github.com/laney/modeloff/internal/memory"),
		DefaultErrKind: observability.ErrorKindStore,
	}.Run(ctx, op, attrs, fn)
}

// Read retrieves all memories for a given model instance.
func (a *StoreAdapter) Read(ctx context.Context, id domain.InstanceID) ([]Entry, error) {
	var result []Entry
	err := a.inSpan(ctx, "memory.file.read",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			entries, err := a.store.ReadMemories(ctx, id)
			if err != nil {
				return err
			}

			result = make([]Entry, len(entries))
			for i, e := range entries {
				result[i] = Entry{Key: e.Key, Content: e.Content}
			}

			return nil
		})

	return result, err
}

// Write stores a memory entry for a given model instance.
func (a *StoreAdapter) Write(ctx context.Context, id domain.InstanceID, entry Entry) error {
	return a.inSpan(ctx, "memory.file.write",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			return a.store.WriteMemory(ctx, id, entry.Key, entry.Content)
		})
}

// Delete removes a specific memory entry by key.
func (a *StoreAdapter) Delete(ctx context.Context, id domain.InstanceID, key string) error {
	return a.inSpan(ctx, "memory.file.delete",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			return a.store.DeleteMemory(ctx, id, key)
		})
}

// Reset removes all memories. This delegates to ResetMemories on the
// store if available, otherwise it's a no-op.
func (a *StoreAdapter) Reset(ctx context.Context) error {
	return a.inSpan(ctx, "memory.file.reset", nil,
		func(ctx context.Context, _ trace.Span) error {
			r, ok := a.store.(interface {
				ResetMemories(context.Context) error
			})
			if !ok {
				return nil
			}

			return r.ResetMemories(ctx)
		})
}
