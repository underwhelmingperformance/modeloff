package components

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/ui"
)

// metricsPaneShownMsg tells the pane the observability drawer has just
// opened, which is when it starts collecting.
type metricsPaneShownMsg struct{}

type metricsPaneRefreshedMsg struct {
	snapshot observability.MetricsSnapshot

	// series says which chain of refreshes this message belongs to.
	// The pane answers a refresh with the next tick of the same
	// chain, so a message from an abandoned chain is what would
	// otherwise start a second one running alongside the current.
	series int
}

// MetricsPane renders a scrollable snapshot of current metrics.
type MetricsPane struct {
	baseContext func() context.Context
	obs         *observability.Runtime
	feed        FeedView
	snapshot    observability.MetricsSnapshot
	width       int
	height      int

	// series identifies the chain of refreshes currently running.
	//
	// The workspace forwards messages to this pane only while the
	// observability drawer is open, so a refresh that ticks while the
	// drawer is closed is dropped before it reaches here and its chain
	// ends there. That is what keeps a hidden pane from collecting
	// metrics once a second for the rest of the session, and
	// [metricsPaneShownMsg] is what starts it again the next time the
	// drawer opens. A drawer closed and reopened inside one tick
	// leaves the earlier chain's refresh still in flight, so starting
	// a chain counts a new series and the older one is ignored when it
	// arrives.
	series int
}

// NewMetricsPane creates a metrics pane backed by OpenTelemetry
// snapshots. `baseContext` is the supplier the pane calls for a
// fresh ctx on each refresh, mirroring [session.New]'s shape.
func NewMetricsPane(baseContext func() context.Context, obs *observability.Runtime) MetricsPane {
	return MetricsPane{
		baseContext: baseContext,
		obs:         obs,
		feed:        NewFeedView("No metrics yet", "updated metrics"),
	}
}

// Init implements ui.Model. Collection starts when the drawer opens,
// not here: the drawer is closed when the application starts, and a
// snapshot taken for a pane nobody can see has no reader.
func (m MetricsPane) Init() tea.Cmd {
	return nil
}

// Update implements ui.Model.
func (m MetricsPane) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case metricsPaneShownMsg:
		m.series++

		return m, m.refreshCmd()

	case ui.BoundsMsg:
		m.width = msg.Rect.Width
		m.height = msg.Rect.Height
		m.feed = m.feed.SetLines(renderMetricsSnapshot(m.snapshot, m.width))
		updatedFeed, feedCmd := m.feed.Update(msg)
		m.feed = updatedFeed

		return m, feedCmd

	case metricsPaneRefreshedMsg:
		if msg.series != m.series {
			return m, nil
		}

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
func (m MetricsPane) KeyBindings() []ui.KeyBinding {
	return m.feed.KeyBindings()
}

func (m MetricsPane) refreshCmd() tea.Cmd {
	if m.obs == nil {
		return nil
	}

	series := m.series

	return tea.Tick(metricsRefreshInterval, func(time.Time) tea.Msg {
		snapshot, err := m.obs.SnapshotMetrics(m.baseContext())
		if err != nil {
			return metricsPaneRefreshedMsg{series: series}
		}

		return metricsPaneRefreshedMsg{snapshot: snapshot, series: series}
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

	lines = append(lines, "", "Operation outcomes:")

	for _, operation := range snapshot.OperationCounts {
		lines = append(lines, wrap.Render(fmt.Sprintf(
			"%s  %s  count %d",
			operation.Operation,
			operation.Result,
			operation.Count,
		)))
	}

	if len(snapshot.OperationCounts) == 0 {
		lines = append(lines, "No operation counts yet")
	}

	lines = append(lines, "", "Memory activity:")
	lines = append(lines, wrap.Render(fmt.Sprintf(
		"searches %d  zero-hit %d  avg results %.2f  max top score %.4f",
		snapshot.MemorySearch.Searches,
		snapshot.MemorySearch.ZeroHitSearches,
		snapshot.MemorySearch.AverageResults,
		snapshot.MemorySearch.MaxTopScore,
	)))

	for _, tool := range snapshot.MemoryTools {
		lines = append(lines, wrap.Render(fmt.Sprintf(
			"%s  %s  count %d",
			tool.Kind,
			tool.Result,
			tool.Count,
		)))
	}

	if len(snapshot.MemoryTools) == 0 {
		lines = append(lines, "No memory tool calls yet")
	}

	lines = append(lines, "", "Runtime health:")
	lines = append(lines, wrap.Render(fmt.Sprintf(
		"dropped logs %d  embedding requests %d",
		snapshot.RuntimeHealth.DroppedLogs,
		snapshot.RuntimeHealth.EmbeddingRequests,
	)))

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
