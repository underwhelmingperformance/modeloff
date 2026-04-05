package observability

import (
	"cmp"
	"context"
	"slices"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// MetricsSummary is the compact current-run summary shown in the UI.
type MetricsSummary struct {
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	ReasoningTokens  int64
	CachedTokens     int64
	CostCredits      float64
}

// ModelUsageSnapshot contains per-model usage totals.
type ModelUsageSnapshot struct {
	ModelID          string
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	ReasoningTokens  int64
	CachedTokens     int64
	CostCredits      float64
}

// OperationTimingSnapshot contains aggregated duration data for an operation.
type OperationTimingSnapshot struct {
	Operation string
	Count     uint64
	AverageMs float64
	MinMs     float64
	MaxMs     float64
}

// MetricsSnapshot is the render-ready projection of collected OTel metrics.
type MetricsSnapshot struct {
	CollectedAt time.Time
	Summary     MetricsSummary
	Models      []ModelUsageSnapshot
	Operations  []OperationTimingSnapshot
}

// SnapshotMetrics collects current metrics from the manual reader.
func SnapshotMetrics(ctx context.Context, reader *sdkmetric.ManualReader) (MetricsSnapshot, error) {
	if reader == nil {
		return MetricsSnapshot{}, nil
	}

	var resourceMetrics metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &resourceMetrics); err != nil {
		return MetricsSnapshot{}, err
	}

	return snapshotFromResourceMetrics(resourceMetrics), nil
}

func snapshotFromResourceMetrics(resourceMetrics metricdata.ResourceMetrics) MetricsSnapshot {
	snapshot := MetricsSnapshot{CollectedAt: time.Now()}
	models := map[string]*ModelUsageSnapshot{}
	operations := map[string]*OperationTimingSnapshot{}

	for _, scopeMetrics := range resourceMetrics.ScopeMetrics {
		for _, metrics := range scopeMetrics.Metrics {
			switch data := metrics.Data.(type) {
			case metricdata.Sum[int64]:
				consumeInt64Sum(&snapshot, models, metrics.Name, data)
			case metricdata.Sum[float64]:
				consumeFloat64Sum(&snapshot, models, metrics.Name, data)
			case metricdata.ExponentialHistogram[float64]:
				consumeDurationHistogram(operations, data)
			}
		}
	}

	for _, model := range models {
		model.TotalTokens = model.PromptTokens + model.CompletionTokens
		snapshot.Models = append(snapshot.Models, *model)
	}

	for _, operation := range operations {
		snapshot.Operations = append(snapshot.Operations, *operation)
	}

	slices.SortFunc(snapshot.Models, func(a, b ModelUsageSnapshot) int {
		if diff := cmp.Compare(b.CostCredits, a.CostCredits); diff != 0 {
			return diff
		}

		return cmp.Compare(a.ModelID, b.ModelID)
	})

	slices.SortFunc(snapshot.Operations, func(a, b OperationTimingSnapshot) int {
		return cmp.Compare(a.Operation, b.Operation)
	})

	snapshot.Summary.TotalTokens = snapshot.Summary.PromptTokens + snapshot.Summary.CompletionTokens

	return snapshot
}

func consumeInt64Sum(snapshot *MetricsSnapshot, models map[string]*ModelUsageSnapshot, name string, data metricdata.Sum[int64]) {
	for _, point := range data.DataPoints {
		modelID := attrValue(point.Attributes, "model_id")
		model := modelSnapshot(models, modelID)

		switch name {
		case MetricLLMRequests:
			snapshot.Summary.Requests += point.Value
			model.Requests += point.Value
		case MetricPromptTokens:
			snapshot.Summary.PromptTokens += point.Value
			model.PromptTokens += point.Value
		case MetricCompletionTokens:
			snapshot.Summary.CompletionTokens += point.Value
			model.CompletionTokens += point.Value
		case MetricReasoningTokens:
			snapshot.Summary.ReasoningTokens += point.Value
			model.ReasoningTokens += point.Value
		case MetricCachedTokens:
			snapshot.Summary.CachedTokens += point.Value
			model.CachedTokens += point.Value
		}
	}
}

func consumeFloat64Sum(snapshot *MetricsSnapshot, models map[string]*ModelUsageSnapshot, name string, data metricdata.Sum[float64]) {
	if name != MetricCostCredits {
		return
	}

	for _, point := range data.DataPoints {
		snapshot.Summary.CostCredits += point.Value

		modelID := attrValue(point.Attributes, "model_id")
		model := modelSnapshot(models, modelID)
		model.CostCredits += point.Value
	}
}

func consumeDurationHistogram(operations map[string]*OperationTimingSnapshot, data metricdata.ExponentialHistogram[float64]) {
	for _, point := range data.DataPoints {
		name := attrValue(point.Attributes, "operation")
		if name == "" {
			continue
		}

		target, ok := operations[name]
		if !ok {
			target = &OperationTimingSnapshot{
				Operation: name,
				MinMs:     0,
				MaxMs:     0,
			}
			operations[name] = target
		}

		target.Count += point.Count
		target.AverageMs += point.Sum

		if minValue, defined := point.Min.Value(); defined {
			if target.MinMs == 0 || minValue < target.MinMs {
				target.MinMs = minValue
			}
		}

		if maxValue, defined := point.Max.Value(); defined {
			if maxValue > target.MaxMs {
				target.MaxMs = maxValue
			}
		}
	}

	for _, target := range operations {
		if target.Count == 0 {
			continue
		}

		target.AverageMs = target.AverageMs / float64(target.Count)
	}
}

func modelSnapshot(models map[string]*ModelUsageSnapshot, modelID string) *ModelUsageSnapshot {
	if modelID == "" {
		modelID = "unknown"
	}

	model, ok := models[modelID]
	if ok {
		return model
	}

	model = &ModelUsageSnapshot{ModelID: modelID}
	models[modelID] = model

	return model
}

func attrValue(set attribute.Set, key string) string {
	value, ok := set.Value(attribute.Key(key))
	if !ok {
		return ""
	}

	return value.AsString()
}
