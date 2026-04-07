// Package oteltest provides test helpers for OpenTelemetry span and
// metric assertions.
package oteltest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// InstallSpanRecorder sets up an in-memory span recorder as the global
// tracer provider for the duration of the test.
func InstallSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()

	prev := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)

	t.Cleanup(func() {
		shutdownCtx := context.WithoutCancel(t.Context())
		require.NoError(t, provider.Shutdown(shutdownCtx))
		otel.SetTracerProvider(prev)
	})

	return recorder
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
