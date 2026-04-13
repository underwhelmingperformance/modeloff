package observability

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type metricInstruments struct {
	operationCalls    metric.Int64Counter
	llmRequests       metric.Int64Counter
	promptTokens      metric.Int64Counter
	completionTokens  metric.Int64Counter
	reasoningTokens   metric.Int64Counter
	cachedTokens      metric.Int64Counter
	cacheWriteTokens  metric.Int64Counter
	costCredits       metric.Float64Counter
	operationDuration metric.Float64Histogram
	requestDuration   metric.Float64Histogram
	droppedLogs       metric.Int64Counter

	memoryOperations     metric.Int64Counter
	memoryToolCalls      metric.Int64Counter
	memorySearchResults  metric.Int64Histogram
	memorySearchTopScore metric.Float64Histogram
	embeddingRequests    metric.Int64Counter
	embeddingDurationMs  metric.Float64Histogram
}

// SpanMetricsProcessor derives metrics from ended spans.
type SpanMetricsProcessor struct {
	instruments metricInstruments
}

// NewSpanMetricsProcessor creates a processor that records metric updates.
func NewSpanMetricsProcessor(instruments metricInstruments) *SpanMetricsProcessor {
	return &SpanMetricsProcessor{instruments: instruments}
}

// OnStart does nothing because metrics are derived from final span state.
func (*SpanMetricsProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

// OnEnd records counters and histograms from the ended span.
func (p *SpanMetricsProcessor) OnEnd(span sdktrace.ReadOnlySpan) {
	if span == nil {
		return
	}

	attrSet := attribute.NewSet(span.Attributes()...)

	operation := span.Name()
	result := ResultOK
	modelID := ""

	if val, ok := attrSet.Value(attribute.Key(AttrOperation)); ok && val.AsString() != "" {
		operation = val.AsString()
	}

	if val, ok := attrSet.Value(attribute.Key(AttrResult)); ok && val.AsString() != "" {
		result = val.AsString()
	} else if span.Status().Code != codes.Unset {
		result = ResultError
	}

	attrs := []attribute.KeyValue{
		attribute.String(AttrOperation, operation),
		attribute.String(AttrResult, result),
	}

	if val, ok := attrSet.Value(attribute.Key(AttrModelID)); ok && val.AsString() != "" {
		modelID = val.AsString()
		attrs = append(attrs, attribute.String(AttrModelID, modelID))
	}

	ctx := context.Background()
	p.instruments.operationCalls.Add(ctx, 1, metric.WithAttributes(attrs...))

	durationMs := float64(span.EndTime().Sub(span.StartTime())) / float64(time.Millisecond)
	p.instruments.operationDuration.Record(ctx, durationMs, metric.WithAttributes(attrs...))

	if operation == "memory.embed" {
		p.instruments.embeddingRequests.Add(ctx, 1, metric.WithAttributes(attrs...))
		p.instruments.embeddingDurationMs.Record(ctx, durationMs, metric.WithAttributes(attrs...))

		return
	}

	if isMemoryOperation(operation) {
		p.instruments.memoryOperations.Add(ctx, 1, metric.WithAttributes(attrs...))
		return
	}

	if _, hasUsage := attrSet.Value(attribute.Key(AttrPromptTokens)); !hasUsage {
		return
	}

	p.instruments.llmRequests.Add(ctx, 1, metric.WithAttributes(attrs...))
	p.instruments.requestDuration.Record(ctx, durationMs, metric.WithAttributes(attrs...))

	if val, ok := attrSet.Value(attribute.Key(AttrPromptTokens)); ok {
		p.instruments.promptTokens.Add(ctx, val.AsInt64(), metric.WithAttributes(attrs...))
	}

	if val, ok := attrSet.Value(attribute.Key(AttrCompletionTokens)); ok {
		p.instruments.completionTokens.Add(ctx, val.AsInt64(), metric.WithAttributes(attrs...))
	}

	if val, ok := attrSet.Value(attribute.Key(AttrReasoningTokens)); ok {
		p.instruments.reasoningTokens.Add(ctx, val.AsInt64(), metric.WithAttributes(attrs...))
	}

	if val, ok := attrSet.Value(attribute.Key(AttrCachedTokens)); ok {
		p.instruments.cachedTokens.Add(ctx, val.AsInt64(), metric.WithAttributes(attrs...))
	}

	if val, ok := attrSet.Value(attribute.Key(AttrCacheWriteTokens)); ok {
		p.instruments.cacheWriteTokens.Add(ctx, val.AsInt64(), metric.WithAttributes(attrs...))
	}

	if val, ok := attrSet.Value(attribute.Key(AttrCostCredits)); ok {
		p.instruments.costCredits.Add(ctx, val.AsFloat64(), metric.WithAttributes(attrs...))
	}
}

// ForceFlush performs no work.
func (*SpanMetricsProcessor) ForceFlush(context.Context) error {
	return nil
}

// Shutdown performs no work.
func (*SpanMetricsProcessor) Shutdown(context.Context) error {
	return nil
}

func isMemoryOperation(operation string) bool {
	switch operation {
	case "memory.write", "memory.delete", "memory.search":
		return true
	}

	return false
}
