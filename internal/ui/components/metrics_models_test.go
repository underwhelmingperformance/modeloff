package components

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/ui"
)

func TestMetricsSummaryModel_statusItems(t *testing.T) {
	model := NewMetricsSummaryModel(context.Background(), nil)

	updated, cmd := model.Update(metricsSummaryRefreshedMsg{
		snapshot: observability.MetricsSnapshot{
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
		},
	})
	require.Nil(t, cmd)
	model = updated

	require.Equal(t, []ui.StatusItem{{
		ID:       "metrics-summary",
		Side:     ui.StatusSideRight,
		Priority: 100,
		Full:     "req 4  in 12  out 8  cache 5/2  cost 0.2500  dropped 3",
		Compact:  "in 12  out 8  c 5/2  0.2500  d3",
	}}, model.StatusItems())
}

func TestMetricsPane_view_renders_snapshot(t *testing.T) {
	model := NewMetricsPane(context.Background(), nil)

	sized, _ := model.Update(ui.BoundsMsg{
		Rect: ui.Rect{Width: 80, Height: 30},
	})
	model = sized.(MetricsPane)

	updated, cmd := model.Update(metricsPaneRefreshedMsg{
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

	view := model.View(80, 30)
	require.Contains(t, view, "req 2  in 11  out 7")
	require.Contains(t, view, "cached 5  wrote 2")
	require.Contains(t, view, "anthropic/claude-3-haiku")
	require.Contains(t, view, "Operation outcomes:")
	require.Contains(t, view, "session.dispatch_to_instance  reply  count 2")
	require.Contains(t, view, "Memory activity:")
	require.Contains(t, view, "searches 2  zero-hit 1")
	require.Contains(t, view, "write_memory  ok  count 1")
	require.Contains(t, view, "Runtime health:")
	require.Contains(t, view, "dropped logs 2  embedding requests 3")
	require.Contains(t, view, "session.send_message")
}

func TestChatWorkspace_statusItems_follow_observability_state(t *testing.T) {
	workspace := NewChatWorkspace(
		NewChatView("#general", "testuser", ""),
	).WithMetrics(NewMetricsPane(context.Background(), nil))

	require.Empty(t, workspace.StatusItems())
	require.False(t, workspace.WantsNickListHidden())

	updated, _ := workspace.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	workspace = updated.(ChatWorkspace)

	require.Equal(t, []ui.StatusItem{{
		ID:       "observability-mode",
		Side:     ui.StatusSideRight,
		Priority: 10,
		Full:     "obs drawer",
		Compact:  "obs",
	}}, workspace.StatusItems())

	updated, _ = workspace.Update(tea.KeyMsg{Type: tea.KeyCtrlF})
	workspace = updated.(ChatWorkspace)

	require.True(t, workspace.WantsNickListHidden())
	require.Equal(t, []ui.StatusItem{{
		ID:       "observability-mode",
		Side:     ui.StatusSideRight,
		Priority: 10,
		Full:     "obs logs",
		Compact:  "obs",
	}}, workspace.StatusItems())

	updated, _ = workspace.Update(tea.KeyMsg{Type: tea.KeyTab})
	workspace = updated.(ChatWorkspace)

	require.Equal(t, []ui.StatusItem{{
		ID:       "observability-mode",
		Side:     ui.StatusSideRight,
		Priority: 10,
		Full:     "obs metrics",
		Compact:  "obs",
	}}, workspace.StatusItems())
}
