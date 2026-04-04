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
	"go.opentelemetry.io/otel/trace"
)

func TestPanelExporter_exports_records_to_entries(t *testing.T) {
	ingest := make(chan PanelEntry, 1)
	exporter := NewPanelExporter(ingest, nil)

	var record sdklog.Record
	record.SetTimestamp(time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC))
	record.SetSeverityText("INFO")
	record.SetBody(otellog.StringValue("hello"))
	record.SetTraceID(trace.TraceID{1})
	record.SetSpanID(trace.SpanID{2})
	record.AddAttributes(otellog.KeyValueFromAttribute(attribute.String("component", "session")))

	require.NoError(t, exporter.Export(context.Background(), []sdklog.Record{record}))

	entry := <-ingest
	require.Equal(t, "INFO", entry.Level)
	require.Equal(t, "hello", entry.Message)
	require.Equal(t, trace.TraceID{1}.String(), entry.TraceID)
	require.Equal(t, trace.SpanID{2}.String(), entry.SpanID)
	require.Equal(t, []PanelField{{Key: "component", Value: ""}}, entry.Fields)
}

func TestPanelExporter_records_dropped_logs_on_backpressure(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = provider.Shutdown(context.Background()) }()

	counter, err := provider.Meter("test").Int64Counter(MetricDroppedLogs)
	require.NoError(t, err)

	ingest := make(chan PanelEntry)
	exporter := NewPanelExporter(ingest, counter)

	var record sdklog.Record
	record.SetObservedTimestamp(time.Now())
	record.SetBody(otellog.StringValue("dropped"))

	require.NoError(t, exporter.Export(context.Background(), []sdklog.Record{record}))

	var metrics metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &metrics))

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

	var record sdklog.Record
	record.SetObservedTimestamp(observed)
	record.SetBody(otellog.StringValue("hello"))
	record.AddAttributes(otellog.KeyValueFromAttribute(attribute.String("model_id", "anthropic/claude-3-haiku")))
	record.SetSeverity(otellog.SeverityInfo)

	entry := panelEntryFromRecord(record)

	require.Equal(t, observed, entry.Timestamp)
	require.Equal(t, "INFO", entry.Level)
	require.Equal(t, []PanelField{{
		Key:   "model_id",
		Value: "",
	}}, entry.Fields)
}

func TestValueString_formats_string_values(t *testing.T) {
	require.Equal(t, "session", valueString(otellog.StringValue("session")))
	require.Equal(t, "42", valueString(otellog.Int64Value(42)))
}
