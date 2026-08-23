package components

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/language"

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

func TestDrawBorderedPane_respects_assigned_rectangle(t *testing.T) {
	screen := uv.NewScreenBuffer(30, 10)
	area := uv.Rect(3, 2, 20, 5)

	drawBorderedPane(screen, area, "Title", false, func(contentArea uv.Rectangle) {
		drawString(screen, contentArea, "content")
	})

	require.Equal(t, "┌", screen.CellAt(3, 2).Content)
	require.Equal(t, "┐", screen.CellAt(22, 2).Content)
	require.Equal(t, "T", screen.CellAt(4, 3).Content)
	require.Equal(t, "c", screen.CellAt(4, 4).Content)
	require.Equal(t, "└", screen.CellAt(3, 6).Content)
	require.Equal(t, "┘", screen.CellAt(22, 6).Content)
}

func TestObservabilityDrawer_Draw_fills_assigned_height(t *testing.T) {
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
			drawer := newObservabilityDrawer().withMetrics(NewMetricsPane(t.Context, nil))

			sized, _ := drawer.Update(ui.BoundsMsg{
				Rect: uv.Rect(0, 0, tc.width, tc.height),
			})
			drawer = sized.(observabilityDrawer)

			opened, _ := drawer.Update(toggleObservabilityKey())
			drawer = opened.(observabilityDrawer)

			screen := uv.NewScreenBuffer(tc.width, tc.height)
			drawer.Draw(screen, screen.Bounds())
			obsRect := screen.Bounds()

			require.Equal(t, "┌", screen.CellAt(obsRect.Min.X, obsRect.Min.Y).Content)
			require.Equal(t, "┘", screen.CellAt(obsRect.Max.X-1, obsRect.Max.Y-1).Content)
		})
	}
}

func TestObservabilityDrawer_fullscreen_wide_split_stays_at_sixty_five_percent(t *testing.T) {
	drawer := newObservabilityDrawer().withMetrics(NewMetricsPane(t.Context, nil))

	updated, _ := drawer.Update(toggleObservabilityKey())
	drawer = updated.(observabilityDrawer)
	updated, _ = drawer.Update(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl})
	drawer = updated.(observabilityDrawer)
	updated, _ = drawer.Update(ui.BoundsMsg{Rect: uv.Rect(0, 0, 140, 30)})
	drawer = updated.(observabilityDrawer)

	layout := drawer.layout(uv.Rect(0, 0, 140, 30))
	require.Equal(t, uv.Rect(0, 0, 91, 30), layout.LogsRect)
	require.Equal(t, uv.Rect(91, 0, 49, 30), layout.MetricsRect)
	require.Equal(t, borderedContentRect(layout.LogsRect), drawer.Logs.bounds)
	require.Equal(t, borderedContentRect(layout.MetricsRect), drawer.Metrics.feed.bounds)

	screen := uv.NewScreenBuffer(140, 30)
	drawer.Draw(screen, screen.Bounds())
	require.Equal(t, "┐", screen.CellAt(90, 0).Content)
	require.Equal(t, "┌", screen.CellAt(91, 0).Content)
}

// toggleObservabilityKey is the alt+l keypress DefaultWorkspaceKeyMap
// binds to ToggleObservability.
func toggleObservabilityKey() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: 'l', Mod: tea.ModAlt}
}

func TestIsChatScrollKey(t *testing.T) {
	km := DefaultChatViewKeyMap

	tests := []struct {
		name string
		msg  tea.KeyPressMsg
		want bool
	}{
		{name: "pgup", msg: tea.KeyPressMsg{Code: tea.KeyPgUp}, want: true},
		{name: "pgdown", msg: tea.KeyPressMsg{Code: tea.KeyPgDown}, want: true},
		{name: "ctrl+up", msg: tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModCtrl}, want: true},
		{name: "ctrl+down", msg: tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModCtrl}, want: true},
		{name: "plain up is not a chat scroll key", msg: tea.KeyPressMsg{Code: tea.KeyUp}, want: false},
		{name: "the drawer toggle is not a chat scroll key", msg: toggleObservabilityKey(), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isChatScrollKey(km, tt.msg))
		})
	}
}

func TestObservabilityDrawer_split_mode_ignores_chat_scroll_keys(t *testing.T) {
	drawer := newObservabilityDrawer()

	sized, _ := drawer.Update(ui.BoundsMsg{Rect: uv.Rect(0, 0, 80, 30)})
	drawer = sized.(observabilityDrawer)

	opened, _ := drawer.Update(toggleObservabilityKey())
	drawer = opened.(observabilityDrawer)
	require.True(t, drawer.Open)
	require.False(t, drawer.Fullscreen)

	entries := make([]observability.PanelEntry, 0, 50)
	for range 50 {
		entries = append(entries, observability.PanelEntry{Level: "INFO", Message: "log line"})
	}
	drawer = drawer.SetLogEntries(entries)
	require.False(t, drawer.Logs.ScrolledUp(), "the log feed starts pinned to its tail")

	updated, _ := drawer.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	drawer = updated.(observabilityDrawer)

	require.False(t, drawer.Logs.ScrolledUp(),
		"PgUp must scroll the chat transcript, not the drawer, while the drawer is only split open")
}
