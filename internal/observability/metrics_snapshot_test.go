package observability

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestSnapshotFromResourceMetrics_summarises_usage_and_timings(t *testing.T) {
	now := time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC)
	modelAttrs := attribute.NewSet(attribute.String("model_id", "anthropic/claude-3-haiku"))
	opAttrs := attribute.NewSet(attribute.String("operation", "session.send_message"))

	metrics := metricdata.ResourceMetrics{
		ScopeMetrics: []metricdata.ScopeMetrics{{
			Metrics: []metricdata.Metrics{
				{
					Name: MetricLLMRequests,
					Data: metricdata.Sum[int64]{DataPoints: []metricdata.DataPoint[int64]{{Attributes: modelAttrs, Value: 2}}},
				},
				{
					Name: MetricPromptTokens,
					Data: metricdata.Sum[int64]{DataPoints: []metricdata.DataPoint[int64]{{Attributes: modelAttrs, Value: 11}}},
				},
				{
					Name: MetricCompletionTokens,
					Data: metricdata.Sum[int64]{DataPoints: []metricdata.DataPoint[int64]{{Attributes: modelAttrs, Value: 7}}},
				},
				{
					Name: MetricReasoningTokens,
					Data: metricdata.Sum[int64]{DataPoints: []metricdata.DataPoint[int64]{{Attributes: modelAttrs, Value: 3}}},
				},
				{
					Name: MetricCachedTokens,
					Data: metricdata.Sum[int64]{DataPoints: []metricdata.DataPoint[int64]{{Attributes: modelAttrs, Value: 5}}},
				},
				{
					Name: MetricCacheWriteTokens,
					Data: metricdata.Sum[int64]{DataPoints: []metricdata.DataPoint[int64]{{Attributes: modelAttrs, Value: 2}}},
				},
				{
					Name: MetricCostCredits,
					Data: metricdata.Sum[float64]{DataPoints: []metricdata.DataPoint[float64]{{Attributes: modelAttrs, Value: 1.25}}},
				},
				{
					Name: MetricOperationDurationMs,
					Data: metricdata.ExponentialHistogram[float64]{DataPoints: []metricdata.ExponentialHistogramDataPoint[float64]{
						{
							Attributes: opAttrs,
							Time:       now,
							Count:      2,
							Sum:        60,
							Min:        metricdata.NewExtrema(20.0),
							Max:        metricdata.NewExtrema(40.0),
						},
					}},
				},
			},
		}},
	}

	snapshot := snapshotFromResourceMetrics(metrics)

	require.Equal(t, MetricsSummary{
		Requests:         2,
		PromptTokens:     11,
		CompletionTokens: 7,
		TotalTokens:      18,
		ReasoningTokens:  3,
		CachedTokens:     5,
		CacheWriteTokens: 2,
		CostCredits:      1.25,
	}, snapshot.Summary)
	require.Len(t, snapshot.Models, 1)
	require.Equal(t, ModelUsageSnapshot{
		ModelID:          "anthropic/claude-3-haiku",
		Requests:         2,
		PromptTokens:     11,
		CompletionTokens: 7,
		TotalTokens:      18,
		ReasoningTokens:  3,
		CachedTokens:     5,
		CacheWriteTokens: 2,
		CostCredits:      1.25,
	}, snapshot.Models[0])
	require.Equal(t, []OperationTimingSnapshot{{
		Operation: "session.send_message",
		Count:     2,
		AverageMs: 30,
		MinMs:     20,
		MaxMs:     40,
	}}, snapshot.Operations)
}
