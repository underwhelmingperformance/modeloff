package components

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/uitest"
)

func TestMetricsPane_view_renders_snapshot(t *testing.T) {
	model := NewMetricsPane(t.Context, nil)

	sized, _ := model.Update(ui.BoundsMsg{
		Rect: uv.Rect(0, 0, 80, 30),
	})
	model = sized.(MetricsPane)

	updated, cmd := model.Update(metricsPaneRefreshedMsg{
		series: model.series,
		snapshot: observability.MetricsSnapshot{
			Summary: observability.MetricsSummary{
				Requests:         2,
				PromptTokens:     11,
				CompletionTokens: 7,
				TotalTokens:      18,
				ReasoningTokens:  3,
				CachedTokens:     5,
				CacheWriteTokens: 2,
				CostCredits:      1.25,
			},
			Models: []observability.ModelUsageSnapshot{{
				ModelID:          "anthropic/claude-3-haiku",
				Requests:         2,
				PromptTokens:     11,
				CompletionTokens: 7,
				TotalTokens:      18,
				ReasoningTokens:  3,
				CachedTokens:     5,
				CacheWriteTokens: 2,
				CostCredits:      1.25,
			}},
			OperationCounts: []observability.OperationCountSnapshot{{
				Operation: "session.dispatch_to_instance",
				Result:    "reply",
				Count:     2,
			}},
			MemoryTools: []observability.MemoryToolSnapshot{{
				Kind:   "write_memory",
				Result: "ok",
				Count:  1,
			}},
			MemorySearch: observability.MemorySearchSnapshot{
				Searches:        2,
				ZeroHitSearches: 1,
				AverageResults:  1.5,
				MaxTopScore:     0.875,
			},
			RuntimeHealth: observability.RuntimeHealthSnapshot{
				DroppedLogs:       2,
				EmbeddingRequests: 3,
			},
			Operations: []observability.OperationTimingSnapshot{{
				Operation: "session.send_message",
				Count:     2,
				AverageMs: 30,
				MinMs:     20,
				MaxMs:     40,
			}},
		},
	})
	require.Nil(t, cmd)
	model = updated.(MetricsPane)

	view := renderToBuffer(model, 80, 30)
	require.Equal(t, []string{
		"req 2  in 11  out 7  total 18  reasoning 3  cached 5  wrote 2  cost 1.2500",
		"By model:",
		"anthropic/claude-3-haiku  req 2  in 11  out 7  reasoning 3  cached 5  wrote 2",
		"cost 1.2500",
		"Operation outcomes:",
		"session.dispatch_to_instance  reply  count 2",
		"Memory activity:",
		"searches 2  zero-hit 1  avg results 1.50  max top score 0.8750",
		"write_memory  ok  count 1",
		"Runtime health:",
		"dropped logs 2  embedding requests 3",
		"Operation timings:",
		"session.send_message  count 2  avg 30.00ms  min 20.00ms  max 40.00ms",
	}, uitest.NonEmptyLines(view))
}

// TestMetricsPane_collects_only_while_the_drawer_is_open pins the
// pane's refresh schedule. The workspace forwards messages to the pane
// only while the observability drawer is open, so a refresh that ticks
// while the drawer is closed never reaches the pane and its chain ends
// there. Opening the drawer is what starts a chain; being resized is
// not, or a pane that had been resized while hidden would go on
// collecting snapshots nobody reads.
func TestMetricsPane_collects_only_while_the_drawer_is_open(t *testing.T) {
	obs, err := observability.NewRuntime()
	require.NoError(t, err)
	t.Cleanup(func() { _ = obs.Shutdown(t.Context()) })

	model := NewMetricsPane(t.Context, obs)
	require.Nil(t, model.Init(), "a pane behind a closed drawer collects nothing")

	sized, sizeCmd := model.Update(ui.BoundsMsg{Rect: uv.Rect(0, 0, 80, 30)})
	require.Nil(t, sizeCmd, "being resized does not start a chain")
	model = sized.(MetricsPane)

	shown, showCmd := model.Update(metricsPaneShownMsg{})
	require.NotNil(t, showCmd, "the drawer opening starts one")
	first := shown.(MetricsPane)

	reshown, _ := first.Update(metricsPaneShownMsg{})
	current := reshown.(MetricsPane)

	stale, staleCmd := current.Update(metricsPaneRefreshedMsg{
		series:   first.series,
		snapshot: observability.MetricsSnapshot{Summary: observability.MetricsSummary{Requests: 9}},
	})
	require.Nil(t, staleCmd, "a refresh from the abandoned chain must not schedule another")
	require.Equal(t, observability.MetricsSnapshot{}, stale.(MetricsPane).snapshot,
		"and must not overwrite the snapshot either")

	_, liveCmd := current.Update(metricsPaneRefreshedMsg{series: current.series})
	require.NotNil(t, liveCmd, "the chain the second opening started carries on")
}

// TestObservabilityDrawer_starts_the_metrics_pane_when_it_opens pins
// the other half: the pane cannot see the drawer's state, so the
// drawer has to tell it.
func TestObservabilityDrawer_starts_the_metrics_pane_when_it_opens(t *testing.T) {
	obs, err := observability.NewRuntime()
	require.NoError(t, err)
	t.Cleanup(func() { _ = obs.Shutdown(t.Context()) })

	drawer := newObservabilityDrawer().withMetrics(NewMetricsPane(t.Context, obs))

	toggle := tea.KeyPressMsg{Code: 'l', Mod: tea.ModAlt}

	closed := drawer.Metrics.series

	opened, cmd := drawer.Update(toggle)
	drawer = opened.(observabilityDrawer)

	require.NotNil(t, cmd, "opening the drawer must carry the pane's first refresh out")
	require.Equal(t, closed+1, drawer.Metrics.series,
		"opening the drawer starts a refresh chain")

	shut, _ := drawer.Update(toggle)
	drawer = shut.(observabilityDrawer)

	require.Equal(t, closed+1, drawer.Metrics.series,
		"closing it starts nothing; the running chain ends on its own next tick")
}

func TestObservabilityDrawer_statusItems_follow_state(t *testing.T) {
	drawer := newObservabilityDrawer().withMetrics(NewMetricsPane(t.Context, nil))

	require.Empty(t, drawer.StatusItems())

	updated, _ := drawer.Update(toggleObservabilityKey())
	drawer = updated.(observabilityDrawer)

	require.Equal(t, []ui.StatusItem{{
		ID:       "observability-mode",
		Side:     ui.StatusSideRight,
		Priority: 10,
		Full:     "obs drawer",
		Compact:  "obs",
	}}, drawer.StatusItems())

	updated, _ = drawer.Update(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl})
	drawer = updated.(observabilityDrawer)

	require.Equal(t, []ui.StatusItem{{
		ID:       "observability-mode",
		Side:     ui.StatusSideRight,
		Priority: 10,
		Full:     "obs logs",
		Compact:  "obs",
	}}, drawer.StatusItems())

	updated, _ = drawer.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	drawer = updated.(observabilityDrawer)

	require.Equal(t, []ui.StatusItem{{
		ID:       "observability-mode",
		Side:     ui.StatusSideRight,
		Priority: 10,
		Full:     "obs metrics",
		Compact:  "obs",
	}}, drawer.StatusItems())
}

func TestObservabilityDrawer_fullscreen_renders_logs_and_metrics(t *testing.T) {
	drawer := newObservabilityDrawer().withMetrics(NewMetricsPane(t.Context, nil))

	updated, _ := drawer.Update(toggleObservabilityKey())
	drawer = updated.(observabilityDrawer)

	updated, _ = drawer.Update(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl})
	drawer = updated.(observabilityDrawer)
	updated, _ = drawer.Update(ui.BoundsMsg{Rect: uv.Rect(0, 0, 140, 30)})
	drawer = updated.(observabilityDrawer)

	view := renderToBuffer(drawer, 140, 30)
	require.Equal(t, []string{
		"Logs",
		"Metrics",
		"req 0  in 0  out 0  total 0  reasoning 0",
		"cached 0  wrote 0  cost 0.0000",
		"By model:",
		"No model usage yet",
		"No logs yet",
		"Operation outcomes:",
		"No operation counts yet",
		"Memory activity:",
		"searches 0  zero-hit 0  avg results 0.00  max",
		"top score 0.0000",
		"No memory tool calls yet",
		"Runtime health:",
		"dropped logs 0  embedding requests 0",
		"Operation timings:",
		"No operation timings yet",
	}, uitest.NonBorderSegments(view))

}
