package components

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/language"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/ui"
)

func TestRenderLogEntries_respects_timestamp_format(t *testing.T) {
	entries := []observability.PanelEntry{{
		Timestamp: time.Date(2026, 4, 7, 9, 30, 0, 0, time.UTC),
		Level:     "INFO",
		Message:   "hello",
	}}
	format := "%X"

	lines := renderLogEntries(entries, 120, &format, language.BritishEnglish)

	require.Equal(t, []string{"09:30:00 INFO hello"}, stripLines(lines))
}

func TestRenderLogEntries_can_disable_timestamps(t *testing.T) {
	entries := []observability.PanelEntry{{
		Timestamp: time.Date(2026, 4, 7, 9, 30, 0, 0, time.UTC),
		Level:     "INFO",
		Message:   "hello",
	}}
	disabled := ""

	lines := renderLogEntries(entries, 120, &disabled, language.BritishEnglish)

	require.Equal(t, []string{"INFO hello"}, stripLines(lines))
}

func stripLines(lines []string) []string {
	stripped := make([]string, 0, len(lines))

	for _, line := range lines {
		stripped = append(stripped, trimLine(ansi.Strip(line)))
	}

	return stripped
}

func trimLine(line string) string {
	for len(line) > 0 && line[len(line)-1] == ' ' {
		line = line[:len(line)-1]
	}

	return line
}

func TestBorderedPane_renders_exactly_requested_height(t *testing.T) {
	cases := []struct {
		width  int
		height int
	}{
		{width: 20, height: 5},
		{width: 40, height: 10},
		{width: 80, height: 23},
		{width: 80, height: 76},
	}

	for _, tc := range cases {
		innerWidth, innerHeight := borderedInnerSize(tc.width, tc.height)
		content := strings.Repeat("x\n", innerHeight)
		content = strings.TrimSuffix(content, "\n")
		content = lipgloss.NewStyle().Width(innerWidth).Render(content)

		pane := borderedPane("Title", content, false)

		require.Equalf(t,
			tc.height, lipgloss.Height(pane),
			"borderedPane(W=%d H=%d) must render exactly H rows", tc.width, tc.height)
		require.Equalf(t,
			tc.width, lipgloss.Width(pane),
			"borderedPane(W=%d H=%d) must render exactly W cols", tc.width, tc.height)
	}
}

func TestChatWorkspace_ObsView_height_matches_ObsHeight(t *testing.T) {
	cases := []struct {
		name   string
		width  int
		height int
	}{
		{name: "narrow", width: 80, height: 30},
		{name: "wide", width: 200, height: 60},
		{name: "very tall", width: 120, height: 256},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace := NewChatWorkspace(
				NewChatView[testKind](func() []domain.Event { return nil }, "#general", domain.KindChannel, "testuser", ""),
			).WithMetrics(NewMetricsPane(t.Context, nil))

			sized, _ := workspace.Update(ui.BoundsMsg{
				Rect: ui.Rect{Width: tc.width, Height: tc.height},
			})
			workspace = sized.(ChatWorkspace[testKind])

			opened, _ := workspace.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
			workspace = opened.(ChatWorkspace[testKind])

			obsH := workspace.ObsHeight(tc.height)
			obsView := workspace.ObsView(tc.width, obsH)

			require.Equal(t, obsH, lipgloss.Height(obsView),
				"ObsView must render exactly ObsHeight rows so MainLayout's reservation matches the actual drawer")
		})
	}
}
