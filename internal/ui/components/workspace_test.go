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
				NewChatView[testKind](func() WindowContent { return WindowContent{Channel: "#general"} }, "#general", domain.KindChannel, "testuser", ""),
			).WithMetrics(NewMetricsPane(t.Context, nil))

			sized, _ := workspace.Update(ui.BoundsMsg{
				Rect: ui.Rect{Width: tc.width, Height: tc.height},
			})
			workspace = sized.(ChatWorkspace[testKind])

			opened, _ := workspace.Update(toggleObservabilityKey())
			workspace = opened.(ChatWorkspace[testKind])

			obsH := workspace.ObsHeight(tc.height)
			obsView := workspace.ObsView(tc.width, obsH)

			require.Equal(t, obsH, lipgloss.Height(obsView),
				"ObsView must render exactly ObsHeight rows so MainLayout's reservation matches the actual drawer")
		})
	}
}

// toggleObservabilityKey is the alt+l keypress DefaultWorkspaceKeyMap
// binds to ToggleObservability.
func toggleObservabilityKey() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}, Alt: true}
}

func TestIsChatScrollKey(t *testing.T) {
	km := DefaultChatViewKeyMap

	tests := []struct {
		name string
		msg  tea.KeyMsg
		want bool
	}{
		{name: "pgup", msg: tea.KeyMsg{Type: tea.KeyPgUp}, want: true},
		{name: "pgdown", msg: tea.KeyMsg{Type: tea.KeyPgDown}, want: true},
		{name: "ctrl+up", msg: tea.KeyMsg{Type: tea.KeyCtrlUp}, want: true},
		{name: "ctrl+down", msg: tea.KeyMsg{Type: tea.KeyCtrlDown}, want: true},
		{name: "plain up is not a chat scroll key", msg: tea.KeyMsg{Type: tea.KeyUp}, want: false},
		{name: "the drawer toggle is not a chat scroll key", msg: toggleObservabilityKey(), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isChatScrollKey(km, tt.msg))
		})
	}
}

func TestChatWorkspace_split_mode_routes_scroll_keys_to_chat_only(t *testing.T) {
	workspace := NewChatWorkspace(
		NewChatView[testKind](func() WindowContent { return WindowContent{Channel: "#general"} }, "#general", domain.KindChannel, "testuser", ""),
	)

	sized, _ := workspace.Update(ui.BoundsMsg{Rect: ui.Rect{Width: 80, Height: 30}})
	workspace = sized.(ChatWorkspace[testKind])

	opened, _ := workspace.Update(toggleObservabilityKey())
	workspace = opened.(ChatWorkspace[testKind])
	require.True(t, workspace.Open)
	require.False(t, workspace.Fullscreen)

	entries := make([]observability.PanelEntry, 0, 50)
	for range 50 {
		entries = append(entries, observability.PanelEntry{Level: "INFO", Message: "log line"})
	}
	workspace = workspace.SetLogEntries(entries)
	require.False(t, workspace.Logs.ScrolledUp(), "the log feed starts pinned to its tail")

	updated, _ := workspace.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	workspace = updated.(ChatWorkspace[testKind])

	require.False(t, workspace.Logs.ScrolledUp(),
		"PgUp must scroll the chat transcript, not the drawer, while the drawer is only split open")
}
