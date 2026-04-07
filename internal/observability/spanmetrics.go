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

	operation := span.Name()
	result := ResultOK
	modelID := ""

	attrs := make([]attribute.KeyValue, 0, 3)
	spanAttrs := span.Attributes()

	if value, ok := findStringAttr(spanAttrs, AttrOperation); ok && value != "" {
		operation = value
	}

	if value, ok := findStringAttr(spanAttrs, AttrResult); ok && value != "" {
		result = value
	} else if span.Status().Code != codes.Unset {
		result = ResultError
	}

	attrs = append(attrs, attribute.String(AttrOperation, operation), attribute.String(AttrResult, result))

	if value, ok := findStringAttr(spanAttrs, AttrModelID); ok && value != "" {
		modelID = value
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

	if !isLLMUsageOperation(operation) {
		return
	}

	p.instruments.llmRequests.Add(ctx, 1, metric.WithAttributes(attrs...))
	p.instruments.requestDuration.Record(ctx, durationMs, metric.WithAttributes(attrs...))

	if value, ok := findInt64Attr(spanAttrs, AttrPromptTokens); ok {
		p.instruments.promptTokens.Add(ctx, value, metric.WithAttributes(attrs...))
	}

	if value, ok := findInt64Attr(spanAttrs, AttrCompletionTokens); ok {
		p.instruments.completionTokens.Add(ctx, value, metric.WithAttributes(attrs...))
	}

	if value, ok := findInt64Attr(spanAttrs, AttrReasoningTokens); ok {
		p.instruments.reasoningTokens.Add(ctx, value, metric.WithAttributes(attrs...))
	}

	if value, ok := findInt64Attr(spanAttrs, AttrCachedTokens); ok {
		p.instruments.cachedTokens.Add(ctx, value, metric.WithAttributes(attrs...))
	}

	if value, ok := findInt64Attr(spanAttrs, AttrCacheWriteTokens); ok {
		p.instruments.cacheWriteTokens.Add(ctx, value, metric.WithAttributes(attrs...))
	}

	if value, ok := findFloat64Attr(spanAttrs, AttrCostCredits); ok {
		p.instruments.costCredits.Add(ctx, value, metric.WithAttributes(attrs...))
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

func isLLMUsageOperation(operation string) bool {
	switch operation {
	case "api.openrouter.send_events", "api.openrouter.continue_with_tool_results", "api.openrouter.generate_nick":
		return true
	}

	return false
}

func findStringAttr(attrs []attribute.KeyValue, key string) (string, bool) {
	for _, attr := range attrs {
		if string(attr.Key) != key {
			continue
		}

		return attr.Value.AsString(), true
	}

	return "", false
}

func findInt64Attr(attrs []attribute.KeyValue, key string) (int64, bool) {
	for _, attr := range attrs {
		if string(attr.Key) != key {
			continue
		}

		return attr.Value.AsInt64(), true
	}

	return 0, false
}

func findFloat64Attr(attrs []attribute.KeyValue, key string) (float64, bool) {
	for _, attr := range attrs {
		if string(attr.Key) != key {
			continue
		}

		return attr.Value.AsFloat64(), true
	}

	return 0, false
}
