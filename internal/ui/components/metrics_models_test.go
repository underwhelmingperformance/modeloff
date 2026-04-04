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
				CostCredits:      0.25,
			},
		},
	})
	require.Nil(t, cmd)
	model = updated

	require.Equal(t, []ui.StatusItem{{
		ID:       "metrics-summary",
		Side:     ui.StatusSideRight,
		Priority: 100,
		Full:     "req 4  in 12  out 8  cost 0.2500",
		Compact:  "in 12  out 8  0.2500",
	}}, model.StatusItems())
}

func TestMetricsPane_view_renders_snapshot(t *testing.T) {
	model := NewMetricsPane(context.Background(), nil)
	updated, cmd := model.Update(metricsPaneRefreshedMsg{
		snapshot: observability.MetricsSnapshot{
			Summary: observability.MetricsSummary{
				Requests:         2,
				PromptTokens:     11,
				CompletionTokens: 7,
				TotalTokens:      18,
				ReasoningTokens:  3,
				CachedTokens:     5,
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
				CostCredits:      1.25,
			}},
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

	view := model.View(80, 12)
	require.Contains(t, view, "req 2  in 11  out 7")
	require.Contains(t, view, "anthropic/claude-3-haiku")
	require.Contains(t, view, "session.send_message")
}

func TestChatWorkspace_statusItems_follow_observability_state(t *testing.T) {
	workspace := NewChatWorkspace(
		NewChatView("#general", "testuser", "", nil),
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
