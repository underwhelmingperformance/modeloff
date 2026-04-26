package observability

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/trace"
)

// emitRecord routes an [otellog.Record] through a real
// [sdklog.LoggerProvider] and returns every [sdklog.Record] the SDK
// produced for the emit. Going through a provider populates the SDK
// attribute-count and value-length limits on the record; a bare
// `sdklog.Record{}` has zero-valued limits, which truncates string
// attributes to the empty string and does not reflect how records
// reach an [Exporter] in production.
//
// Callers receive the full captured slice rather than a single
// element so the export contract — exactly what the SDK produced —
// is exposed at the call site, where downstream structural
// assertions on the resulting `PanelEntry` pin the shape end-to-end.
//
// The provider is created per call and torn down via t.Cleanup; do
// not share a provider across emits or retain captured records past
// the next call.
//
// `ctx` is exposed as a parameter so the trace-propagation test can
// pass a `SpanContext`-wrapped context through `logger.Emit` and
// assert the span and trace ids reach the exported `PanelEntry`.
// Other callers pass `t.Context()` directly.
func emitRecord(ctx context.Context, t *testing.T, apply func(r *otellog.Record)) []sdklog.Record {
	t.Helper()

	var captured []sdklog.Record

	provider := sdklog.NewLoggerProvider(
		sdklog.WithResource(resource.Empty()),
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(&recordCaptureExporter{target: &captured})),
	)
	t.Cleanup(func() {
		require.NoError(t, provider.Shutdown(context.WithoutCancel(ctx)))
	})

	logger := provider.Logger("panel_exporter_test")

	var record otellog.Record
	apply(&record)
	logger.Emit(ctx, record)

	return captured
}

// recordCaptureExporter appends every exported record to target so callers can
// detect unexpected emits rather than silently discarding them.
type recordCaptureExporter struct {
	target *[]sdklog.Record
}

func (e *recordCaptureExporter) Export(_ context.Context, records []sdklog.Record) error {
	*e.target = append(*e.target, records...)

	return nil
}

func (*recordCaptureExporter) Shutdown(context.Context) error   { return nil }
func (*recordCaptureExporter) ForceFlush(context.Context) error { return nil }

func TestPanelExporter_exports_records_to_entries(t *testing.T) {
	ingest := make(chan PanelEntry, 1)
	exporter := NewPanelExporter(ingest, nil)

	traceID := trace.TraceID{1}
	spanID := trace.SpanID{2}
	ctx := trace.ContextWithSpanContext(t.Context(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID,
		SpanID:  spanID,
	}))

	timestamp := time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC)
	records := emitRecord(ctx, t, func(r *otellog.Record) {
		r.SetTimestamp(timestamp)
		r.SetSeverityText("INFO")
		r.SetBody(otellog.StringValue("hello"))
		r.AddAttributes(otellog.KeyValueFromAttribute(attribute.String("component", "session")))
	})

	require.NoError(t, exporter.Export(ctx, records))

	entry := <-ingest

	expected := PanelEntry{
		Timestamp: timestamp,
		Level:     "INFO",
		Message:   "hello",
		Scope:     "panel_exporter_test",
		TraceID:   traceID.String(),
		SpanID:    spanID.String(),
		Fields:    []PanelField{{Key: "component", Value: "session"}},
	}
	require.Equal(t, expected, entry)
}

func TestPanelExporter_records_dropped_logs_on_backpressure(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		require.NoError(t, provider.Shutdown(context.WithoutCancel(t.Context())))
	})

	counter, err := provider.Meter("test").Int64Counter(MetricDroppedLogs)
	require.NoError(t, err)

	ingest := make(chan PanelEntry)
	exporter := NewPanelExporter(ingest, counter)

	records := emitRecord(t.Context(), t, func(r *otellog.Record) {
		r.SetObservedTimestamp(time.Now())
		r.SetBody(otellog.StringValue("dropped"))
	})

	require.NoError(t, exporter.Export(t.Context(), records))

	var metrics metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &metrics))

	require.Equal(t, int64(1), sumValueForMetric(metrics, MetricDroppedLogs))
}

func sumValueForMetric(metrics metricdata.ResourceMetrics, name string) int64 {
	for _, scope := range metrics.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != name {
				continue
			}

			sum, ok := metric.Data.(metricdata.Sum[int64])
			if !ok || len(sum.DataPoints) == 0 {
				return 0
			}

			return sum.DataPoints[0].Value
		}
	}

	return 0
}

func TestPanelEntryFromRecord_uses_observed_timestamp_when_timestamp_missing(t *testing.T) {
	observed := time.Date(2026, 4, 4, 13, 0, 0, 0, time.UTC)

	records := emitRecord(t.Context(), t, func(r *otellog.Record) {
		r.SetObservedTimestamp(observed)
		r.SetBody(otellog.StringValue("hello"))
		r.AddAttributes(otellog.KeyValueFromAttribute(attribute.String("model_id", "anthropic/claude-3-haiku")))
		r.SetSeverity(otellog.SeverityInfo)
	})

	entries := make([]PanelEntry, 0, len(records))
	for _, record := range records {
		entries = append(entries, panelEntryFromRecord(record))
	}

	expected := []PanelEntry{{
		Timestamp: observed,
		Level:     "INFO",
		Message:   "hello",
		Scope:     "panel_exporter_test",
		TraceID:   trace.TraceID{}.String(),
		SpanID:    trace.SpanID{}.String(),
		Fields:    []PanelField{{Key: "model_id", Value: "anthropic/claude-3-haiku"}},
	}}
	require.Equal(t, expected, entries)
}

func TestValueString_formats_string_values(t *testing.T) {
	require.Equal(t, "session", valueString(otellog.StringValue("session")))
	require.Equal(t, "42", valueString(otellog.Int64Value(42)))
}
