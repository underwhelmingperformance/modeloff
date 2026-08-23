package screens

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/ui"
)

func TestMetricsSummaryStatusItems(t *testing.T) {
	snapshot := observability.MetricsSnapshot{
		Summary: observability.MetricsSummary{
			Requests:         4,
			PromptTokens:     12,
			CompletionTokens: 8,
			CachedTokens:     5,
			CacheWriteTokens: 2,
			CostCredits:      0.25,
		},
		RuntimeHealth: observability.RuntimeHealthSnapshot{
			DroppedLogs: 3,
		},
	}

	require.Equal(t, []ui.StatusItem{{
		ID:       "metrics-summary",
		Side:     ui.StatusSideRight,
		Priority: 100,
		Full:     "req 4  in 12  out 8  cache 5/2  cost 0.2500  dropped 3",
		Compact:  "in 12  out 8  c 5/2  0.2500  d3",
	}}, metricsSummaryStatusItems(snapshot))
}
