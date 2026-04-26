package memory

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
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

// Read retrieves all memories for a given model instance.
func (a *StoreAdapter) Read(ctx context.Context, id domain.InstanceID) ([]Entry, error) {
	ctx, span := a.startSpan(ctx, "memory.file.read", attribute.String(observability.AttrInstanceID, string(id)))
	defer span.End()

	entries, err := a.store.ReadMemories(ctx, id)
	if err != nil {
		recordMemoryFileError(span, err)
		return nil, err
	}

	result := make([]Entry, len(entries))
	for i, e := range entries {
		result[i] = Entry{Key: e.Key, Content: e.Content}
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
	return result, nil
}

// Write stores a memory entry for a given model instance.
func (a *StoreAdapter) Write(ctx context.Context, id domain.InstanceID, entry Entry) error {
	ctx, span := a.startSpan(ctx, "memory.file.write", attribute.String(observability.AttrInstanceID, string(id)))
	defer span.End()

	if err := a.store.WriteMemory(ctx, id, entry.Key, entry.Content); err != nil {
		recordMemoryFileError(span, err)
		return err
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
	return nil
}

// Delete removes a specific memory entry by key.
func (a *StoreAdapter) Delete(ctx context.Context, id domain.InstanceID, key string) error {
	ctx, span := a.startSpan(ctx, "memory.file.delete", attribute.String(observability.AttrInstanceID, string(id)))
	defer span.End()

	if err := a.store.DeleteMemory(ctx, id, key); err != nil {
		recordMemoryFileError(span, err)
		return err
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
	return nil
}

// Reset removes all memories. This delegates to ResetMemories on the
// store if available, otherwise it's a no-op.
func (a *StoreAdapter) Reset(ctx context.Context) error {
	ctx, span := a.startSpan(ctx, "memory.file.reset")
	defer span.End()

	if r, ok := a.store.(interface {
		ResetMemories(context.Context) error
	}); ok {
		if err := r.ResetMemories(ctx); err != nil {
			recordMemoryFileError(span, err)
			return err
		}

		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
		return nil
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
	return nil
}

func (a *StoreAdapter) startSpan(ctx context.Context, operation string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	tracer := a.tracerProvider.Tracer("github.com/laney/modeloff/internal/memory")
	attrs = append(attrs, attribute.String(observability.AttrOperation, operation))
	ctx, span := tracer.Start(ctx, operation)
	span.SetAttributes(attrs...)

	return ctx, span
}

func recordMemoryFileError(span trace.Span, err error) {
	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
	span.SetStatus(codes.Error, err.Error())
}
