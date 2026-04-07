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
}

// NewStoreAdapter creates a memory store backed by the given data
// store.
func NewStoreAdapter(s store.Store) *StoreAdapter {
	return &StoreAdapter{store: s}
}

// Read retrieves all memories for a given model instance.
func (a *StoreAdapter) Read(ctx context.Context, nick domain.Nick) ([]Entry, error) {
	ctx, span := startMemoryFileSpan(ctx, "memory.file.read", attribute.String(observability.AttrNick, string(nick)))
	defer span.End()

	entries, err := a.store.ReadMemories(ctx, nick)
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
func (a *StoreAdapter) Write(ctx context.Context, nick domain.Nick, entry Entry) error {
	ctx, span := startMemoryFileSpan(ctx, "memory.file.write", attribute.String(observability.AttrNick, string(nick)))
	defer span.End()

	if err := a.store.WriteMemory(ctx, nick, entry.Key, entry.Content); err != nil {
		recordMemoryFileError(span, err)
		return err
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
	return nil
}

// Delete removes a specific memory entry by key.
func (a *StoreAdapter) Delete(ctx context.Context, nick domain.Nick, key string) error {
	ctx, span := startMemoryFileSpan(ctx, "memory.file.delete", attribute.String(observability.AttrNick, string(nick)))
	defer span.End()

	if err := a.store.DeleteMemory(ctx, nick, key); err != nil {
		recordMemoryFileError(span, err)
		return err
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
	return nil
}

// Reset removes all memories. This delegates to ResetMemories on the
// store if available, otherwise it's a no-op.
func (a *StoreAdapter) Reset(ctx context.Context) error {
	ctx, span := startMemoryFileSpan(ctx, "memory.file.reset")
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

func startMemoryFileSpan(ctx context.Context, operation string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	tracer := otel.Tracer("github.com/laney/modeloff/internal/memory")
	attrs = append(attrs, attribute.String(observability.AttrOperation, operation))
	ctx, span := tracer.Start(ctx, operation)
	span.SetAttributes(attrs...)

	return ctx, span
}

func recordMemoryFileError(span trace.Span, err error) {
	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
	span.SetStatus(codes.Error, err.Error())
}
