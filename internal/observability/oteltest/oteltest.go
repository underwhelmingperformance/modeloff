// Package oteltest provides test helpers for OpenTelemetry span and
// metric assertions.
package oteltest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// NewSpanRecorder returns an in-memory span recorder paired with a
// `TracerProvider` that feeds it. The provider is shut down when
// the test ends. Tests pass the provider into the component-under-
// test via its `WithTracerProvider` builder so span recordings stay
// scoped to a single test even when background goroutines outlive
// it; nothing is installed on the global `otel` provider.
func NewSpanRecorder(t *testing.T) (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	t.Helper()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))

	t.Cleanup(func() {
		shutdownCtx := context.WithoutCancel(t.Context())
		require.NoError(t, provider.Shutdown(shutdownCtx))
	})

	return recorder, provider
}

// FindSpan returns the first completed span with the given name, or
// fails the test if none is found.
func FindSpan(t *testing.T, recorder *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()

	for _, span := range recorder.Ended() {
		if span.Name() == name {
			return span
		}
	}

	t.Fatalf("span %q not found", name)
	return nil
}

// AttrValue returns the string value of the named attribute, or an
// empty string if not found.
func AttrValue(attrs []attribute.KeyValue, key string) string {
	for _, attr := range attrs {
		if string(attr.Key) != key {
			continue
		}

		return attr.Value.Emit()
	}

	return ""
}
