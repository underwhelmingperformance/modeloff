package observability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

func TestRuntime_snapshotMetrics_includes_span_derived_usage(t *testing.T) {
	runtime, err := NewRuntime()
	require.NoError(t, err)
	defer func() {
		require.NoError(t, runtime.Shutdown(context.Background()))
	}()

	ctx, span := otel.Tracer("test").Start(context.Background(), "api.openrouter.send_events")
	span.SetAttributes(
		attribute.String(AttrOperation, "api.openrouter.send_events"),
		attribute.String(AttrModelID, "anthropic/claude-3-haiku"),
		attribute.String(AttrResult, ResultReply),
		attribute.Int64(AttrPromptTokens, 21),
		attribute.Int64(AttrCompletionTokens, 13),
		attribute.Int64(AttrReasoningTokens, 5),
		attribute.Int64(AttrCachedTokens, 8),
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
	require.Equal(t, 0.75, snapshot.Summary.CostCredits)
	require.Len(t, snapshot.Models, 1)
	require.Equal(t, "anthropic/claude-3-haiku", snapshot.Models[0].ModelID)
	require.NotEmpty(t, snapshot.Operations)
}
