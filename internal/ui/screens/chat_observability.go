package screens

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
)

const metricsSummaryRefreshInterval = time.Second

// logsUpdatedMsg reports that the observability log buffer has taken
// a new record.
type logsUpdatedMsg struct{}

type metricsSummaryRefreshedMsg struct {
	snapshot observability.MetricsSnapshot
}

// routeObservability answers a log record arriving. The drawer takes
// the buffer's current contents if it is open, and the wait for the
// next record is re-armed either way.
func (s ChatScreen) routeObservability(msg tea.Msg) (ChatScreen, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case logsUpdatedMsg:
		s = s.updateLogEntries()

		return s, s.waitForLogUpdateCmd(), true

	case metricsSummaryRefreshedMsg:
		s.metrics = msg.snapshot

		return s, s.refreshMetricsSummaryCmd(), true

	default:
		return s, nil, false
	}
}

// WithObservability wires local observability into the chat screen.
func (s ChatScreen) WithObservability(obs *observability.Runtime) ChatScreen {
	s.obs = obs

	s.layout = s.layout.
		WithObservability(components.NewMetricsPane(s.baseContext, obs)).
		SetLogEntries(obs.LogBuffer().Entries())

	return s
}

func (s ChatScreen) refreshMetricsSummaryCmd() tea.Cmd {
	if s.obs == nil {
		return nil
	}

	return tea.Tick(metricsSummaryRefreshInterval, func(time.Time) tea.Msg {
		snapshot, err := s.obs.SnapshotMetrics(s.baseContext())
		if err != nil {
			return metricsSummaryRefreshedMsg{}
		}

		return metricsSummaryRefreshedMsg{snapshot: snapshot}
	})
}

func metricsSummaryStatusItems(snapshot observability.MetricsSnapshot) []ui.StatusItem {
	if snapshot.CollectedAt.IsZero() &&
		snapshot.Summary.Requests == 0 &&
		snapshot.Summary.PromptTokens == 0 &&
		snapshot.Summary.CompletionTokens == 0 &&
		snapshot.Summary.CostCredits == 0 &&
		snapshot.RuntimeHealth.DroppedLogs == 0 {
		return nil
	}

	full := fmt.Sprintf(
		"req %d  in %d  out %d  cache %d/%d  cost %.4f",
		snapshot.Summary.Requests,
		snapshot.Summary.PromptTokens,
		snapshot.Summary.CompletionTokens,
		snapshot.Summary.CachedTokens,
		snapshot.Summary.CacheWriteTokens,
		snapshot.Summary.CostCredits,
	)
	compact := fmt.Sprintf(
		"in %d  out %d  c %d/%d  %.4f",
		snapshot.Summary.PromptTokens,
		snapshot.Summary.CompletionTokens,
		snapshot.Summary.CachedTokens,
		snapshot.Summary.CacheWriteTokens,
		snapshot.Summary.CostCredits,
	)

	if snapshot.RuntimeHealth.DroppedLogs > 0 {
		full += fmt.Sprintf("  dropped %d", snapshot.RuntimeHealth.DroppedLogs)
		compact += fmt.Sprintf("  d%d", snapshot.RuntimeHealth.DroppedLogs)
	}

	return []ui.StatusItem{{
		ID:       "metrics-summary",
		Side:     ui.StatusSideRight,
		Priority: 100,
		Full:     full,
		Compact:  compact,
	}}
}

func (s ChatScreen) waitForLogUpdateCmd() tea.Cmd {
	if s.obs == nil {
		return nil
	}

	ch := s.obs.LogBuffer().Updates()

	return func() tea.Msg {
		_, ok := <-ch
		if !ok {
			return nil
		}

		return logsUpdatedMsg{}
	}
}

// updateLogEntries hands the log buffer's current contents to the
// observability drawer, which renders every entry it is given.
//
// A closed drawer takes nothing and is noted as behind instead. The
// application logs several records per command and the drawer is
// closed almost all the time, so rendering every entry into a pane
// nobody can see would be the largest cost of writing a log line.
// [ChatScreen.forwardToLayout] catches the drawer up on the message
// that opens it.
func (s ChatScreen) updateLogEntries() ChatScreen {
	if s.obs == nil {
		return s
	}

	if !s.layout.ObservabilityOpen() {
		s.logsBehind = true

		return s
	}

	s.logsBehind = false
	s.layout = s.layout.SetLogEntries(s.obs.LogBuffer().Entries())

	return s
}
