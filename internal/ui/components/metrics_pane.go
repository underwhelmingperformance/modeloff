package components

import (
	"context"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/ui"
)

type metricsPaneRefreshedMsg struct {
	snapshot observability.MetricsSnapshot
}

// MetricsPane renders a scrollable snapshot of current metrics.
type MetricsPane struct {
	ctx      context.Context
	obs      *observability.Runtime
	feed     FeedView
	snapshot observability.MetricsSnapshot
	width    int
	height   int
}

// NewMetricsPane creates a metrics pane backed by OpenTelemetry snapshots.
func NewMetricsPane(ctx context.Context, obs *observability.Runtime) MetricsPane {
	return MetricsPane{
		ctx:  ctx,
		obs:  obs,
		feed: NewFeedView("No metrics yet", "updated metrics"),
	}
}

// Init starts periodic metrics collection.
func (m MetricsPane) Init() tea.Cmd {
	return m.refreshCmd()
}

// Update implements ui.Model.
func (m MetricsPane) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case ui.BoundsMsg:
		m.width = msg.Rect.Width
		m.height = msg.Rect.Height
		m.feed = m.feed.SetLines(renderMetricsSnapshot(m.snapshot, m.width))
		updatedFeed, cmd := m.feed.Update(msg)
		m.feed = updatedFeed
		return m, cmd

	case metricsPaneRefreshedMsg:
		m.snapshot = msg.snapshot
		m.feed = m.feed.SetLines(renderMetricsSnapshot(m.snapshot, m.width))
		return m, m.refreshCmd()
	}

	updatedFeed, cmd := m.feed.Update(msg)
	m.feed = updatedFeed

	return m, cmd
}

// View implements ui.Model.
func (m MetricsPane) View(width, height int) string {
	view, _, _ := m.feed.View(width, height)

	return view
}

// KeyBindings implements ui.Keybinding.
func (m MetricsPane) KeyBindings() []key.Binding {
	return m.feed.KeyBindings()
}

func (m MetricsPane) refreshCmd() tea.Cmd {
	if m.obs == nil {
		return nil
	}

	return tea.Tick(metricsRefreshInterval, func(time.Time) tea.Msg {
		snapshot, err := m.obs.SnapshotMetrics(m.ctx)
		if err != nil {
			return metricsPaneRefreshedMsg{}
		}

		return metricsPaneRefreshedMsg{snapshot: snapshot}
	})
}

func renderMetricsSnapshot(snapshot observability.MetricsSnapshot, width int) []string {
	wrap := lipgloss.NewStyle().Width(width)
	lines := []string{
		wrap.Render(fmt.Sprintf(
			"req %d  in %d  out %d  total %d  reasoning %d  cached %d  wrote %d  cost %.4f",
			snapshot.Summary.Requests,
			snapshot.Summary.PromptTokens,
			snapshot.Summary.CompletionTokens,
			snapshot.Summary.TotalTokens,
			snapshot.Summary.ReasoningTokens,
			snapshot.Summary.CachedTokens,
			snapshot.Summary.CacheWriteTokens,
			snapshot.Summary.CostCredits,
		)),
		"",
		"By model:",
	}

	for _, model := range snapshot.Models {
		lines = append(lines, wrap.Render(fmt.Sprintf(
			"%s  req %d  in %d  out %d  reasoning %d  cached %d  wrote %d  cost %.4f",
			model.ModelID,
			model.Requests,
			model.PromptTokens,
			model.CompletionTokens,
			model.ReasoningTokens,
			model.CachedTokens,
			model.CacheWriteTokens,
			model.CostCredits,
		)))
	}

	if len(snapshot.Models) == 0 {
		lines = append(lines, "No model usage yet")
	}

	lines = append(lines, "", "Operation timings:")

	for _, operation := range snapshot.Operations {
		lines = append(lines, wrap.Render(fmt.Sprintf(
			"%s  count %d  avg %.2fms  min %.2fms  max %.2fms",
			operation.Operation,
			operation.Count,
			operation.AverageMs,
			operation.MinMs,
			operation.MaxMs,
		)))
	}

	if len(snapshot.Operations) == 0 {
		lines = append(lines, "No operation timings yet")
	}

	return lines
}
