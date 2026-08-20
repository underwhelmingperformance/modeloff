package screens

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
)

// logsUpdatedMsg reports that the observability log buffer has taken
// a new record.
type logsUpdatedMsg struct{}

// routeObservability answers a log record arriving. The drawer takes
// the buffer's current contents if it is open, and the wait for the
// next record is re-armed either way.
func (s ChatScreen) routeObservability(msg tea.Msg) (ChatScreen, tea.Cmd, bool) {
	if _, ok := msg.(logsUpdatedMsg); !ok {
		return s, nil, false
	}

	s = s.updateLogEntries()

	return s, s.waitForLogUpdateCmd(), true
}

// WithObservability wires local observability into the chat screen.
func (s ChatScreen) WithObservability(obs *observability.Runtime) ChatScreen {
	s.obs = obs
	s.summary = components.NewMetricsSummaryModel(s.baseContext, obs)

	chatView, ok := s.layout.Content.(components.ChatView[chatcmd.CompletionContext])
	if !ok {
		return s
	}

	workspace := components.NewChatWorkspace(chatView).
		WithMetrics(components.NewMetricsPane(s.baseContext, obs)).
		SetLogEntries(obs.LogBuffer().Entries())
	s.layout.Content = workspace

	return s
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

	workspace, ok := s.layout.Content.(components.ChatWorkspace[chatcmd.CompletionContext])
	if !ok {
		return s
	}

	if !workspace.Open {
		s.logsBehind = true

		return s
	}

	s.logsBehind = false
	s.layout.Content = workspace.SetLogEntries(s.obs.LogBuffer().Entries())

	return s
}
