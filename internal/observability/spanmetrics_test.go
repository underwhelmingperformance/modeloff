package observability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

func TestRuntime_snapshotMetrics_includes_memory_operations(t *testing.T) {
	runtime, err := NewRuntime()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, runtime.Shutdown(context.WithoutCancel(t.Context())))
	})

	operations := []string{"memory.write", "memory.delete", "memory.search"}
	for _, op := range operations {
		_, span := otel.Tracer("test").Start(t.Context(), op)
		span.SetAttributes(
			attribute.String(AttrOperation, op),
			attribute.String(AttrMemoryNick, "claude"),
			attribute.String(AttrResult, ResultOK),
		)
		span.End()
	}

	snapshot, err := runtime.SnapshotMetrics(t.Context())
	require.NoError(t, err)

	type opCount struct {
		Operation string
		Count     uint64
	}
	got := make([]opCount, 0, len(snapshot.Operations))
	for _, op := range snapshot.Operations {
		got = append(got, opCount{op.Operation, op.Count})
	}
	require.ElementsMatch(t, []opCount{
		{"memory.write", 1},
		{"memory.delete", 1},
		{"memory.search", 1},
	}, got)

	require.ElementsMatch(t, []OperationCountSnapshot{
		{Operation: "memory.write", Result: ResultOK, Count: 1},
		{Operation: "memory.delete", Result: ResultOK, Count: 1},
		{Operation: "memory.search", Result: ResultOK, Count: 1},
	}, snapshot.OperationCounts)
}

func TestRuntime_snapshotMetrics_includes_span_derived_usage(t *testing.T) {
	runtime, err := NewRuntime()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, runtime.Shutdown(context.WithoutCancel(t.Context())))
	})

	ctx, span := otel.Tracer("test").Start(t.Context(), "api.openrouter.send_events")
	span.SetAttributes(
		attribute.String(AttrOperation, "api.openrouter.send_events"),
		attribute.String(AttrModelID, "anthropic/claude-3-haiku"),
		attribute.String(AttrResult, ResultReply),
		attribute.Int64(AttrPromptTokens, 21),
		attribute.Int64(AttrCompletionTokens, 13),
		attribute.Int64(AttrReasoningTokens, 5),
		attribute.Int64(AttrCachedTokens, 8),
		attribute.Int64(AttrCacheWriteTokens, 3),
		attribute.Float64(AttrCostCredits, 0.75),
	)
	span.End()

	snapshot, err := runtime.SnapshotMetrics(ctx)
	require.NoError(t, err)

	require.Equal(t, int64(1), snapshot.Summary.Requests)
	require.Equal(t, int64(21), snapshot.Summary.PromptTokens)
	require.Equal(t, int64(13), snapshot.Summary.CompletionTokens)
	require.Equal(t, int64(34), snapshot.Summary.TotalTokens)
	require.Equal(t, int64(5), snapshot.Summary.ReasoningTokens)
	require.Equal(t, int64(8), snapshot.Summary.CachedTokens)
	require.Equal(t, int64(3), snapshot.Summary.CacheWriteTokens)
	require.Equal(t, 0.75, snapshot.Summary.CostCredits)
	require.Equal(t, []ModelUsageSnapshot{{
		ModelID:          "anthropic/claude-3-haiku",
		Requests:         1,
		PromptTokens:     21,
		CompletionTokens: 13,
		TotalTokens:      34,
		ReasoningTokens:  5,
		CachedTokens:     8,
		CacheWriteTokens: 3,
		CostCredits:      0.75,
	}}, snapshot.Models)
	require.NotEmpty(t, snapshot.Operations)
}

func TestRuntime_snapshotMetrics_counts_tool_follow_up_requests(t *testing.T) {
	runtime, err := NewRuntime()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, runtime.Shutdown(context.WithoutCancel(t.Context())))
	})

	recordLLMUsageSpan(t, "api.openrouter.send_events", "anthropic/claude-3-haiku", ResultReply, 21, 13, 0.75)
	recordLLMUsageSpan(t, "api.openrouter.continue_with_tool_results", "anthropic/claude-3-haiku", ResultReply, 5, 7, 0.25)

	snapshot, err := runtime.SnapshotMetrics(t.Context())
	require.NoError(t, err)

	require.Equal(t, int64(2), snapshot.Summary.Requests)
	require.Equal(t, int64(26), snapshot.Summary.PromptTokens)
	require.Equal(t, int64(20), snapshot.Summary.CompletionTokens)
	require.Equal(t, int64(46), snapshot.Summary.TotalTokens)
	require.Equal(t, 1.0, snapshot.Summary.CostCredits)
	require.Equal(t, []ModelUsageSnapshot{{
		ModelID:          "anthropic/claude-3-haiku",
		Requests:         2,
		PromptTokens:     26,
		CompletionTokens: 20,
		TotalTokens:      46,
		CostCredits:      1.0,
	}}, snapshot.Models)
}

func TestRuntime_snapshotMetrics_includes_memory_tool_and_search_metrics(t *testing.T) {
	runtime, err := NewRuntime()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, runtime.Shutdown(context.WithoutCancel(t.Context())))
	})

	RecordMemoryToolCall(t.Context(), "write_memory", ResultOK)
	RecordMemorySearchResults(t.Context(), 0)
	RecordMemorySearchResults(t.Context(), 3)
	RecordMemorySearchTopScore(t.Context(), 0.875)

	snapshot, err := runtime.SnapshotMetrics(t.Context())
	require.NoError(t, err)

	require.Equal(t, []MemoryToolSnapshot{{
		Kind:   "write_memory",
		Result: "ok",
		Count:  1,
	}}, snapshot.MemoryTools)
	require.Equal(t, MemorySearchSnapshot{
		Searches:        2,
		ZeroHitSearches: 1,
		AverageResults:  1.5,
		MaxTopScore:     0.875,
	}, snapshot.MemorySearch)
}

func TestRuntime_snapshotMetrics_counts_generate_personas_as_LLM_usage(t *testing.T) {
	runtime, err := NewRuntime()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, runtime.Shutdown(context.WithoutCancel(t.Context())))
	})

	recordLLMUsageSpan(t, "api.openrouter.generate_personas", "anthropic/claude-3-haiku", ResultReply, 30, 15, 0.5)

	snapshot, err := runtime.SnapshotMetrics(t.Context())
	require.NoError(t, err)

	require.Equal(t, int64(1), snapshot.Summary.Requests)
	require.Equal(t, int64(30), snapshot.Summary.PromptTokens)
	require.Equal(t, int64(15), snapshot.Summary.CompletionTokens)
	require.Equal(t, int64(45), snapshot.Summary.TotalTokens)
	require.Equal(t, 0.5, snapshot.Summary.CostCredits)
	require.Equal(t, []ModelUsageSnapshot{{
		ModelID:          "anthropic/claude-3-haiku",
		Requests:         1,
		PromptTokens:     30,
		CompletionTokens: 15,
		TotalTokens:      45,
		CostCredits:      0.5,
	}}, snapshot.Models)
}

func TestRuntime_snapshotMetrics_any_span_with_token_attrs_counts_as_LLM_usage(t *testing.T) {
	runtime, err := NewRuntime()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, runtime.Shutdown(context.WithoutCancel(t.Context())))
	})

	recordLLMUsageSpan(t, "api.openrouter.hypothetical_future_op", "test/model", ResultReply, 10, 5, 0.1)

	snapshot, err := runtime.SnapshotMetrics(t.Context())
	require.NoError(t, err)

	require.Equal(t, MetricsSummary{
		Requests:         1,
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      15,
		CostCredits:      0.1,
	}, snapshot.Summary)
	require.Equal(t, []ModelUsageSnapshot{{
		ModelID:          "test/model",
		Requests:         1,
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      15,
		CostCredits:      0.1,
	}}, snapshot.Models)
}

func TestRuntime_snapshotMetrics_span_without_token_attrs_not_counted(t *testing.T) {
	runtime, err := NewRuntime()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, runtime.Shutdown(context.WithoutCancel(t.Context())))
	})

	_, span := otel.Tracer("test").Start(t.Context(), "api.openrouter.list_models")
	span.SetAttributes(
		attribute.String(AttrOperation, "api.openrouter.list_models"),
		attribute.String(AttrResult, ResultOK),
	)
	span.End()

	snapshot, err := runtime.SnapshotMetrics(t.Context())
	require.NoError(t, err)

	require.Equal(t, MetricsSummary{}, snapshot.Summary)
	require.Empty(t, snapshot.Models)
}

func recordLLMUsageSpan(
	t *testing.T,
	operation string,
	modelID string,
	result string,
	promptTokens int64,
	completionTokens int64,
	costCredits float64,
) {
	t.Helper()

	_, span := otel.Tracer("test").Start(t.Context(), operation)
	span.SetAttributes(
		attribute.String(AttrOperation, operation),
		attribute.String(AttrModelID, modelID),
		attribute.String(AttrResult, result),
		attribute.Int64(AttrPromptTokens, promptTokens),
		attribute.Int64(AttrCompletionTokens, completionTokens),
		attribute.Float64(AttrCostCredits, costCredits),
	)
	span.End()
}
