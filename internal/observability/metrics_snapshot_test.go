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
	modelAttrs := attribute.NewSet(attribute.String(AttrModelID, "anthropic/claude-3-haiku"))
	opCountAttrs := attribute.NewSet(
		attribute.String(AttrOperation, "session.dispatch_to_instance"),
		attribute.String(AttrResult, "reply"),
	)
	memoryToolAttrs := attribute.NewSet(
		attribute.String(AttrMemoryToolKind, "write_memory"),
		attribute.String(AttrResult, "ok"),
	)
	opAttrs := attribute.NewSet(attribute.String(AttrOperation, "session.send_message"))

	metrics := metricdata.ResourceMetrics{
		ScopeMetrics: []metricdata.ScopeMetrics{{
			Metrics: []metricdata.Metrics{
				{
					Name: MetricOperationCalls,
					Data: metricdata.Sum[int64]{DataPoints: []metricdata.DataPoint[int64]{{Attributes: opCountAttrs, Value: 2}}},
				},
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
					Name: MetricDroppedLogs,
					Data: metricdata.Sum[int64]{DataPoints: []metricdata.DataPoint[int64]{{Value: 3}}},
				},
				{
					Name: MetricEmbeddingRequests,
					Data: metricdata.Sum[int64]{DataPoints: []metricdata.DataPoint[int64]{{Value: 4}}},
				},
				{
					Name: MetricMemoryToolCalls,
					Data: metricdata.Sum[int64]{DataPoints: []metricdata.DataPoint[int64]{{Attributes: memoryToolAttrs, Value: 1}}},
				},
				{
					Name: MetricMemorySearchResults,
					Data: metricdata.Histogram[int64]{DataPoints: []metricdata.HistogramDataPoint[int64]{
						{
							Count:        2,
							Bounds:       []float64{0, 1, 2, 3},
							BucketCounts: []uint64{1, 0, 1, 0, 0},
							Sum:          2,
						},
					}},
				},
				{
					Name: MetricMemorySearchTopScore,
					Data: metricdata.Histogram[float64]{DataPoints: []metricdata.HistogramDataPoint[float64]{
						{
							Count:        1,
							Bounds:       []float64{0.25, 0.5, 0.75, 1},
							BucketCounts: []uint64{0, 0, 1, 0, 0},
							Sum:          0.875,
							Max:          metricdata.NewExtrema(0.875),
						},
					}},
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
	require.Equal(t, []ModelUsageSnapshot{{
		ModelID:          "anthropic/claude-3-haiku",
		Requests:         2,
		PromptTokens:     11,
		CompletionTokens: 7,
		TotalTokens:      18,
		ReasoningTokens:  3,
		CachedTokens:     5,
		CacheWriteTokens: 2,
		CostCredits:      1.25,
	}}, snapshot.Models)
	require.Equal(t, []OperationTimingSnapshot{{
		Operation: "session.send_message",
		Count:     2,
		AverageMs: 30,
		MinMs:     20,
		MaxMs:     40,
	}}, snapshot.Operations)
	require.Equal(t, []OperationCountSnapshot{{
		Operation: "session.dispatch_to_instance",
		Result:    "reply",
		Count:     2,
	}}, snapshot.OperationCounts)
	require.Equal(t, []MemoryToolSnapshot{{
		Kind:   "write_memory",
		Result: "ok",
		Count:  1,
	}}, snapshot.MemoryTools)
	require.Equal(t, MemorySearchSnapshot{
		Searches:        2,
		ZeroHitSearches: 1,
		AverageResults:  1,
		MaxTopScore:     0.875,
	}, snapshot.MemorySearch)
	require.Equal(t, RuntimeHealthSnapshot{
		DroppedLogs:       3,
		EmbeddingRequests: 4,
	}, snapshot.RuntimeHealth)
}
