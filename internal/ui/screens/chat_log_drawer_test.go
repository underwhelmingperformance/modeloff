package screens

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/ui/uitest"
)

// drawerState is what a test asserts about the observability drawer:
// whether the chat-screen knows the drawer's copy of the log buffer is
// out of date, and whether the record is on screen.
type drawerState struct {
	Behind    bool
	ShowsLine bool
}

// TestChatScreen_the_log_drawer_takes_records_when_it_opens pins the
// cost of writing a log line against the drawer's state. Rendering a
// record costs the same whether the drawer is on screen or not, and
// the application writes several per command, so a closed drawer takes
// nothing and is caught up on the message that opens it.
func TestChatScreen_the_log_drawer_takes_records_when_it_opens(t *testing.T) {
	const (
		width  = 200
		height = 60
		line   = "a recorded line"
	)

	obs, err := observability.NewRuntime()
	require.NoError(t, err)
	t.Cleanup(func() { _ = obs.Shutdown(t.Context()) })

	screen := newScreenFixture(t).WithObservability(obs)
	screen, _ = screen.update(tea.WindowSizeMsg{Width: width, Height: height})

	obs.LogBuffer().Ingest() <- observability.PanelEntry{
		Level:     "INFO",
		Message:   line,
		Timestamp: time.Now(),
	}

	// The buffer's drain loop runs on its own goroutine and signals
	// here once the record is in.
	<-obs.LogBuffer().Updates()

	state := func() drawerState {
		return drawerState{
			Behind:    screen.logsBehind,
			ShowsLine: strings.Contains(uitest.StripANSI(screen.View(width, height)), line),
		}
	}

	screen, _ = screen.update(logsUpdatedMsg{})

	require.Equal(t, drawerState{Behind: true, ShowsLine: false}, state(),
		"a record arriving while the drawer is closed is not rendered")

	screen, _ = screen.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}, Alt: true})

	require.Equal(t, drawerState{Behind: false, ShowsLine: true}, state(),
		"opening the drawer catches it up with the buffer")
}
