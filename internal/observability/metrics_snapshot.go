package observability

import (
	"cmp"
	"context"
	"math"
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
	CacheWriteTokens int64
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
	CacheWriteTokens int64
	CostCredits      float64
}

// OperationCountSnapshot contains aggregated counts for an operation/result pair.
type OperationCountSnapshot struct {
	Operation string
	Result    string
	Count     int64
}

// MemoryToolSnapshot contains aggregated counts for a memory tool kind/result pair.
type MemoryToolSnapshot struct {
	Kind   string
	Result string
	Count  int64
}

// MemorySearchSnapshot contains aggregate information about memory searches.
type MemorySearchSnapshot struct {
	Searches        int64
	ZeroHitSearches int64
	AverageResults  float64
	MaxTopScore     float64
}

// RuntimeHealthSnapshot contains counts that describe local telemetry health.
type RuntimeHealthSnapshot struct {
	DroppedLogs       int64
	EmbeddingRequests int64
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
	CollectedAt     time.Time
	Summary         MetricsSummary
	Models          []ModelUsageSnapshot
	OperationCounts []OperationCountSnapshot
	MemoryTools     []MemoryToolSnapshot
	MemorySearch    MemorySearchSnapshot
	RuntimeHealth   RuntimeHealthSnapshot
	Operations      []OperationTimingSnapshot
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
	operationCounts := map[string]*OperationCountSnapshot{}
	memoryTools := map[string]*MemoryToolSnapshot{}
	operations := map[string]*OperationTimingSnapshot{}

	for _, scopeMetrics := range resourceMetrics.ScopeMetrics {
		for _, metrics := range scopeMetrics.Metrics {
			switch data := metrics.Data.(type) {
			case metricdata.Sum[int64]:
				consumeInt64Sum(&snapshot, models, operationCounts, memoryTools, metrics.Name, data)
			case metricdata.Sum[float64]:
				consumeFloat64Sum(&snapshot, models, metrics.Name, data)
			case metricdata.Histogram[int64]:
				consumeInt64Histogram(&snapshot, metrics.Name, data)
			case metricdata.Histogram[float64]:
				consumeFloat64Histogram(&snapshot, metrics.Name, data)
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

	for _, operationCount := range operationCounts {
		snapshot.OperationCounts = append(snapshot.OperationCounts, *operationCount)
	}

	for _, memoryTool := range memoryTools {
		snapshot.MemoryTools = append(snapshot.MemoryTools, *memoryTool)
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

	slices.SortFunc(snapshot.OperationCounts, func(a, b OperationCountSnapshot) int {
		if diff := cmp.Compare(a.Operation, b.Operation); diff != 0 {
			return diff
		}

		return cmp.Compare(a.Result, b.Result)
	})

	slices.SortFunc(snapshot.MemoryTools, func(a, b MemoryToolSnapshot) int {
		if diff := cmp.Compare(a.Kind, b.Kind); diff != 0 {
			return diff
		}

		return cmp.Compare(a.Result, b.Result)
	})

	snapshot.Summary.TotalTokens = snapshot.Summary.PromptTokens + snapshot.Summary.CompletionTokens
	if snapshot.MemorySearch.Searches > 0 {
		snapshot.MemorySearch.AverageResults = snapshot.MemorySearch.AverageResults / float64(snapshot.MemorySearch.Searches)
	}

	return snapshot
}

func consumeInt64Sum(
	snapshot *MetricsSnapshot,
	models map[string]*ModelUsageSnapshot,
	operationCounts map[string]*OperationCountSnapshot,
	memoryTools map[string]*MemoryToolSnapshot,
	name string,
	data metricdata.Sum[int64],
) {
	for _, point := range data.DataPoints {
		switch name {
		case MetricOperationCalls:
			operation := attrValue(point.Attributes, AttrOperation)
			result := attrValue(point.Attributes, AttrResult)
			count := operationCountSnapshot(operationCounts, operation, result)
			count.Count += point.Value
		case MetricLLMRequests:
			model := modelSnapshot(models, attrValue(point.Attributes, AttrModelID))
			snapshot.Summary.Requests += point.Value
			model.Requests += point.Value
		case MetricPromptTokens:
			model := modelSnapshot(models, attrValue(point.Attributes, AttrModelID))
			snapshot.Summary.PromptTokens += point.Value
			model.PromptTokens += point.Value
		case MetricCompletionTokens:
			model := modelSnapshot(models, attrValue(point.Attributes, AttrModelID))
			snapshot.Summary.CompletionTokens += point.Value
			model.CompletionTokens += point.Value
		case MetricReasoningTokens:
			model := modelSnapshot(models, attrValue(point.Attributes, AttrModelID))
			snapshot.Summary.ReasoningTokens += point.Value
			model.ReasoningTokens += point.Value
		case MetricCachedTokens:
			model := modelSnapshot(models, attrValue(point.Attributes, AttrModelID))
			snapshot.Summary.CachedTokens += point.Value
			model.CachedTokens += point.Value
		case MetricCacheWriteTokens:
			model := modelSnapshot(models, attrValue(point.Attributes, AttrModelID))
			snapshot.Summary.CacheWriteTokens += point.Value
			model.CacheWriteTokens += point.Value
		case MetricDroppedLogs:
			snapshot.RuntimeHealth.DroppedLogs += point.Value
		case MetricEmbeddingRequests:
			snapshot.RuntimeHealth.EmbeddingRequests += point.Value
		case MetricMemoryToolCalls:
			toolKind := attrValue(point.Attributes, AttrMemoryToolKind)
			result := attrValue(point.Attributes, AttrResult)
			tool := memoryToolSnapshot(memoryTools, toolKind, result)
			tool.Count += point.Value
		}
	}
}

func consumeFloat64Sum(snapshot *MetricsSnapshot, models map[string]*ModelUsageSnapshot, name string, data metricdata.Sum[float64]) {
	if name != MetricCostCredits {
		return
	}

	for _, point := range data.DataPoints {
		snapshot.Summary.CostCredits += point.Value

		model := modelSnapshot(models, attrValue(point.Attributes, AttrModelID))
		model.CostCredits += point.Value
	}
}

func consumeInt64Histogram(snapshot *MetricsSnapshot, name string, data metricdata.Histogram[int64]) {
	if name != MetricMemorySearchResults {
		return
	}

	for _, point := range data.DataPoints {
		snapshot.MemorySearch.Searches += safeUint64ToInt64(point.Count)
		snapshot.MemorySearch.AverageResults += float64(point.Sum)

		if len(point.Bounds) > 0 && point.Bounds[0] == 0 && len(point.BucketCounts) > 0 {
			snapshot.MemorySearch.ZeroHitSearches += safeUint64ToInt64(point.BucketCounts[0])
		}
	}
}

func consumeFloat64Histogram(snapshot *MetricsSnapshot, name string, data metricdata.Histogram[float64]) {
	if name != MetricMemorySearchTopScore {
		return
	}

	for _, point := range data.DataPoints {
		maxValue, ok := point.Max.Value()
		if !ok {
			continue
		}

		if maxValue > snapshot.MemorySearch.MaxTopScore {
			snapshot.MemorySearch.MaxTopScore = maxValue
		}
	}
}

func consumeDurationHistogram(operations map[string]*OperationTimingSnapshot, data metricdata.ExponentialHistogram[float64]) {
	for _, point := range data.DataPoints {
		name := attrValue(point.Attributes, AttrOperation)
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

func operationCountSnapshot(
	counts map[string]*OperationCountSnapshot,
	operation string,
	result string,
) *OperationCountSnapshot {
	key := operation + "\x00" + result

	count, ok := counts[key]
	if ok {
		return count
	}

	count = &OperationCountSnapshot{
		Operation: operation,
		Result:    result,
	}
	counts[key] = count

	return count
}

func memoryToolSnapshot(
	tools map[string]*MemoryToolSnapshot,
	kind string,
	result string,
) *MemoryToolSnapshot {
	key := kind + "\x00" + result

	tool, ok := tools[key]
	if ok {
		return tool
	}

	tool = &MemoryToolSnapshot{
		Kind:   kind,
		Result: result,
	}
	tools[key] = tool

	return tool
}

func attrValue(set attribute.Set, key string) string {
	value, ok := set.Value(attribute.Key(key))
	if !ok {
		return ""
	}

	return value.AsString()
}

func safeUint64ToInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}

	return int64(v)
}
