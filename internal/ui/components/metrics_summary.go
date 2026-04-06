package components

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/ui"
)

const metricsRefreshInterval = time.Second

type metricsSummaryRefreshedMsg struct {
	snapshot observability.MetricsSnapshot
}

// MetricsSummaryModel owns the compact metrics summary shown in the
// status bar.
type MetricsSummaryModel struct {
	ctx      context.Context
	obs      *observability.Runtime
	snapshot observability.MetricsSnapshot
}

// NewMetricsSummaryModel creates a metrics summary model.
func NewMetricsSummaryModel(ctx context.Context, obs *observability.Runtime) MetricsSummaryModel {
	return MetricsSummaryModel{
		ctx: ctx,
		obs: obs,
	}
}

// Init starts periodic metrics collection.
func (m MetricsSummaryModel) Init() tea.Cmd {
	return m.refreshCmd()
}

// Update applies refresh messages and schedules the next collection.
func (m MetricsSummaryModel) Update(msg tea.Msg) (MetricsSummaryModel, tea.Cmd) {
	switch msg := msg.(type) {
	case metricsSummaryRefreshedMsg:
		m.snapshot = msg.snapshot
		return m, m.refreshCmd()
	default:
		return m, nil
	}
}

// StatusItems implements ui.StatusProvider.
func (m MetricsSummaryModel) StatusItems() []ui.StatusItem {
	if m.snapshot.CollectedAt.IsZero() &&
		m.snapshot.Summary.Requests == 0 &&
		m.snapshot.Summary.PromptTokens == 0 &&
		m.snapshot.Summary.CompletionTokens == 0 &&
		m.snapshot.Summary.CostCredits == 0 {
		return nil
	}

	return []ui.StatusItem{{
		ID:       "metrics-summary",
		Side:     ui.StatusSideRight,
		Priority: 100,
		Full: fmt.Sprintf(
			"req %d  in %d  out %d  cache %d/%d  cost %.4f",
			m.snapshot.Summary.Requests,
			m.snapshot.Summary.PromptTokens,
			m.snapshot.Summary.CompletionTokens,
			m.snapshot.Summary.CachedTokens,
			m.snapshot.Summary.CacheWriteTokens,
			m.snapshot.Summary.CostCredits,
		),
		Compact: fmt.Sprintf(
			"in %d  out %d  c %d/%d  %.4f",
			m.snapshot.Summary.PromptTokens,
			m.snapshot.Summary.CompletionTokens,
			m.snapshot.Summary.CachedTokens,
			m.snapshot.Summary.CacheWriteTokens,
			m.snapshot.Summary.CostCredits,
		),
	}}
}

func (m MetricsSummaryModel) refreshCmd() tea.Cmd {
	if m.obs == nil {
		return nil
	}

	return tea.Tick(metricsRefreshInterval, func(time.Time) tea.Msg {
		snapshot, err := m.obs.SnapshotMetrics(m.ctx)
		if err != nil {
			return metricsSummaryRefreshedMsg{}
		}

		return metricsSummaryRefreshedMsg{snapshot: snapshot}
	})
}
