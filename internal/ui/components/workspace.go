package components

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/text/language"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/ptr"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
	"github.com/laney/modeloff/internal/ui/timestamp"
)

const minObservabilityDrawerHeight = 8

type workspaceFocus int

const (
	workspaceFocusLogs workspaceFocus = iota
	workspaceFocusMetrics
)

type workspaceLayout struct {
	ChatRect    ui.Rect
	ObsRect     ui.Rect
	LogsRect    ui.Rect
	MetricsRect ui.Rect
}

// ChatWorkspace renders chat alongside the observability panes.
type ChatWorkspace[C command.KindProvider] struct {
	Chat       ChatView[C]
	Logs       FeedView
	Metrics    MetricsPane
	HasMetrics bool
	Open       bool
	Fullscreen bool
	Focus      workspaceFocus
	keyMap     WorkspaceKeyMap
	bounds     ui.Rect

	logEntries      []observability.PanelEntry
	timestampFormat *string
	locale          language.Tag
}

// NewChatWorkspace creates the chat content workspace.
func NewChatWorkspace[C command.KindProvider](chat ChatView[C]) ChatWorkspace[C] {
	return ChatWorkspace[C]{
		Chat:   chat,
		Logs:   NewFeedView("No logs yet", "new logs"),
		keyMap: DefaultWorkspaceKeyMap,
		Focus:  workspaceFocusLogs,
		locale: timestamp.CurrentLocale(),
	}
}

// WithMetrics attaches a metrics pane to the workspace.
func (w ChatWorkspace[C]) WithMetrics(metrics MetricsPane) ChatWorkspace[C] {
	w.Metrics = metrics
	w.HasMetrics = true

	return w
}

// Init implements ui.Model.
func (w ChatWorkspace[C]) Init() tea.Cmd {
	cmds := []tea.Cmd{w.Chat.Init()}
	if w.HasMetrics {
		cmds = append(cmds, w.Metrics.Init())
	}

	return tea.Batch(cmds...)
}

// Update implements ui.Model.
func (w ChatWorkspace[C]) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case ui.BoundsMsg:
		w.bounds = msg.Rect
		return w.updateChildBounds()

	case TimestampFormatMsg:
		w.timestampFormat = ptr.CloneString(msg.Format)
		w.locale = msg.Locale
		w = w.refreshLogs()
		return w, nil

	case tea.KeyMsg:
		switch {
		case ui.Matches(msg, w.keyMap.ToggleObservability):
			w.Open = !w.Open
			if !w.Open {
				w.Fullscreen = false
			}
			return w.updateChildBounds()

		case w.Open && ui.Matches(msg, w.keyMap.ToggleFullscreen):
			w.Fullscreen = !w.Fullscreen
			return w.updateChildBounds()

		case w.Fullscreen && ui.Matches(msg, w.keyMap.NextPane):
			if !w.HasMetrics {
				return w, nil
			}

			if w.Focus == workspaceFocusLogs {
				w.Focus = workspaceFocusMetrics
			} else {
				w.Focus = workspaceFocusLogs
			}

			return w, nil

		case w.Fullscreen && ui.Matches(msg, w.keyMap.ExitFullscreen):
			w.Fullscreen = false
			return w.updateChildBounds()
		}
	}

	if w.Open && w.Fullscreen {
		return w.updateFullscreen(msg)
	}

	return w.updateSplit(msg)
}

func (w ChatWorkspace[C]) updateFullscreen(msg tea.Msg) (ui.Model, tea.Cmd) {
	var cmds []tea.Cmd

	if _, ok := msg.(tea.KeyMsg); ok {
		if w.Focus == workspaceFocusMetrics && w.HasMetrics {
			updatedMetrics, cmd := w.Metrics.Update(msg)
			w.Metrics = updatedMetrics.(MetricsPane)
			cmds = append(cmds, cmd)

			return w, tea.Batch(cmds...)
		}

		updatedLogs, cmd := w.Logs.Update(msg)
		w.Logs = updatedLogs
		cmds = append(cmds, cmd)

		return w, tea.Batch(cmds...)
	}

	updatedLogs, cmd := w.Logs.Update(msg)
	w.Logs = updatedLogs
	cmds = append(cmds, cmd)

	if w.HasMetrics {
		updatedMetrics, cmd := w.Metrics.Update(msg)
		w.Metrics = updatedMetrics.(MetricsPane)
		cmds = append(cmds, cmd)
	}

	return w, tea.Batch(cmds...)
}

func (w ChatWorkspace[C]) updateSplit(msg tea.Msg) (ui.Model, tea.Cmd) {
	var cmds []tea.Cmd

	updatedChat, cmd := w.Chat.Update(msg)
	w.Chat = updatedChat.(ChatView[C])
	cmds = append(cmds, cmd)

	if !w.Open {
		return w, tea.Batch(cmds...)
	}

	updatedLogs, cmd := w.Logs.Update(msg)
	w.Logs = updatedLogs
	cmds = append(cmds, cmd)

	if w.HasMetrics {
		updatedMetrics, cmd := w.Metrics.Update(msg)
		w.Metrics = updatedMetrics.(MetricsPane)
		cmds = append(cmds, cmd)
	}

	return w, tea.Batch(cmds...)
}

// KeyBindings implements ui.Keybinding.
func (w ChatWorkspace[C]) KeyBindings() []ui.KeyBinding {
	bindings := []ui.KeyBinding{w.keyMap.ToggleObservability}

	if w.Open {
		bindings = append(bindings, w.keyMap.ToggleFullscreen)
	}

	if w.Fullscreen {
		if w.HasMetrics {
			bindings = append(bindings, w.keyMap.NextPane)
		}
		bindings = append(bindings, w.keyMap.ExitFullscreen)

		if w.Focus == workspaceFocusMetrics && w.HasMetrics {
			return append(bindings, w.Metrics.KeyBindings()...)
		}

		return append(bindings, w.Logs.KeyBindings()...)
	}

	return append(bindings, ui.CollectKeyBindings(w.Chat)...)
}

// ObsHeight returns the height the observability drawer needs. In
// split mode, this is the drawer height that MainLayout should
// reserve below the three-column area. In fullscreen or closed
// mode it returns 0 because the drawer is handled entirely within
// View.
func (w ChatWorkspace[C]) ObsHeight(totalHeight int) int {
	if !w.Open || w.Fullscreen {
		return 0
	}

	h := max(totalHeight*30/100, minObservabilityDrawerHeight)
	if h >= totalHeight {
		h = totalHeight / 2
	}
	if h < 0 {
		h = 0
	}

	return h
}

// ObsView renders the observability panes at the given dimensions.
// MainLayout calls this to render the drawer spanning the full
// terminal width below the three-column area.
func (w ChatWorkspace[C]) ObsView(width, height int) string {
	if !w.Open || w.Fullscreen || height <= 0 {
		return ""
	}

	layout := w.obsLayout(width, height)

	return w.renderObservabilityLayout(layout, width, height)
}

// View implements ui.Model.
func (w ChatWorkspace[C]) View(width, height int) string {
	if !w.Open {
		return w.Chat.View(width, height)
	}

	if w.Fullscreen {
		layout := w.layout(width, height)
		return w.renderObservabilityLayout(layout, width, height)
	}

	return w.Chat.View(width, height)
}

// SetLogEntries updates the log pane content.
func (w ChatWorkspace[C]) SetLogEntries(entries []observability.PanelEntry) ChatWorkspace[C] {
	w.logEntries = entries

	return w.refreshLogs()
}

// WantsNickListHidden hides the nick list while fullscreen observability is active.
func (w ChatWorkspace[C]) WantsNickListHidden() bool {
	return w.Open && w.Fullscreen
}

// StatusItems implements ui.StatusProvider.
func (w ChatWorkspace[C]) StatusItems() []ui.StatusItem {
	if !w.Open {
		return nil
	}

	label := "obs drawer"
	compact := "obs"

	if w.Fullscreen {
		if w.Focus == workspaceFocusMetrics {
			label = "obs metrics"
		} else {
			label = "obs logs"
		}
	}

	return []ui.StatusItem{{
		ID:       "observability-mode",
		Side:     ui.StatusSideRight,
		Priority: 10,
		Full:     label,
		Compact:  compact,
	}}
}

func (w ChatWorkspace[C]) updateChildBounds() (ui.Model, tea.Cmd) {
	var cmds []tea.Cmd

	if w.Fullscreen {
		layout := w.layout(w.bounds.Width, w.bounds.Height)

		w = w.refreshLogs()
		updatedLogs, cmd := w.Logs.Update(ui.BoundsMsg{Rect: layout.LogsRect})
		w.Logs = updatedLogs
		cmds = append(cmds, cmd)

		if w.HasMetrics {
			updatedMetrics, cmd := w.Metrics.Update(ui.BoundsMsg{Rect: layout.MetricsRect})
			w.Metrics = updatedMetrics.(MetricsPane)
			cmds = append(cmds, cmd)
		}

		return w, tea.Batch(cmds...)
	}

	obsH := w.ObsHeight(w.bounds.Height)
	chatHeight := w.bounds.Height - obsH

	chatRect := ui.Rect{X: w.bounds.X, Y: w.bounds.Y, Width: w.bounds.Width, Height: chatHeight}
	updatedChat, cmd := w.Chat.Update(ui.BoundsMsg{Rect: chatRect})
	w.Chat = updatedChat.(ChatView[C])
	cmds = append(cmds, cmd)

	if w.Open {
		obsLay := w.obsLayout(w.bounds.Width, obsH)

		w = w.refreshLogs()
		updatedLogs, cmd := w.Logs.Update(ui.BoundsMsg{Rect: obsLay.LogsRect})
		w.Logs = updatedLogs
		cmds = append(cmds, cmd)

		if w.HasMetrics {
			updatedMetrics, cmd := w.Metrics.Update(ui.BoundsMsg{Rect: obsLay.MetricsRect})
			w.Metrics = updatedMetrics.(MetricsPane)
			cmds = append(cmds, cmd)
		}
	}

	return w, tea.Batch(cmds...)
}

func (w ChatWorkspace[C]) refreshLogs() ChatWorkspace[C] {
	var logsWidth int

	if w.Fullscreen {
		layout := w.layout(w.bounds.Width, w.bounds.Height)
		logsWidth, _ = borderedInnerSize(layout.LogsRect.Width, layout.LogsRect.Height)
	} else {
		obsLay := w.obsLayout(w.bounds.Width, w.ObsHeight(w.bounds.Height))
		logsWidth, _ = borderedInnerSize(obsLay.LogsRect.Width, obsLay.LogsRect.Height)
	}

	w.Logs = w.Logs.SetLines(renderLogEntries(w.logEntries, logsWidth, w.timestampFormat, w.locale))

	return w
}

func (w ChatWorkspace[C]) layout(width, height int) workspaceLayout {
	layout := workspaceLayout{
		ChatRect: ui.Rect{X: w.bounds.X, Y: w.bounds.Y, Width: width, Height: height},
	}

	if !w.Open {
		return layout
	}

	if w.Fullscreen {
		layout.ObsRect = ui.Rect{X: w.bounds.X, Y: w.bounds.Y, Width: width, Height: height}
		if width >= 140 {
			logsWidth := width * 65 / 100
			layout.LogsRect = ui.Rect{X: w.bounds.X, Y: w.bounds.Y, Width: logsWidth, Height: height}
			layout.MetricsRect = ui.Rect{X: w.bounds.X + logsWidth, Y: w.bounds.Y, Width: width - logsWidth, Height: height}

			return layout
		}

		logsHeight := height * 60 / 100
		layout.LogsRect = ui.Rect{X: w.bounds.X, Y: w.bounds.Y, Width: width, Height: logsHeight}
		layout.MetricsRect = ui.Rect{X: w.bounds.X, Y: w.bounds.Y + logsHeight, Width: width, Height: height - logsHeight}

		return layout
	}

	drawerHeight := max(height*30/100, minObservabilityDrawerHeight)
	if drawerHeight >= height {
		drawerHeight = height / 2
	}
	if drawerHeight < 0 {
		drawerHeight = 0
	}

	chatHeight := height - drawerHeight
	layout.ChatRect.Height = chatHeight
	layout.ObsRect = ui.Rect{X: w.bounds.X, Y: w.bounds.Y + chatHeight, Width: width, Height: drawerHeight}

	logsHeight := drawerHeight * 70 / 100
	if logsHeight < 3 {
		logsHeight = drawerHeight
	}
	layout.LogsRect = ui.Rect{X: w.bounds.X, Y: layout.ObsRect.Y, Width: width, Height: logsHeight}
	layout.MetricsRect = ui.Rect{X: w.bounds.X, Y: layout.ObsRect.Y + logsHeight, Width: width, Height: drawerHeight - logsHeight}

	return layout
}

func (w ChatWorkspace[C]) obsLayout(width, height int) workspaceLayout {
	layout := workspaceLayout{
		ObsRect: ui.Rect{Width: width, Height: height},
	}

	logsHeight := height * 70 / 100
	if logsHeight < 3 {
		logsHeight = height
	}

	layout.LogsRect = ui.Rect{Width: width, Height: logsHeight}
	layout.MetricsRect = ui.Rect{Y: logsHeight, Width: width, Height: height - logsHeight}

	return layout
}

func (w ChatWorkspace[C]) renderObservabilityLayout(layout workspaceLayout, width, height int) string {
	if height <= 0 {
		return ""
	}

	if w.Fullscreen && width >= 140 {
		logs := borderedPane("Logs", w.renderLogsPane(layout.LogsRect.Width, layout.LogsRect.Height), w.Focus == workspaceFocusLogs)
		metrics := borderedPane("Metrics", w.renderMetricsPane(layout.MetricsRect.Width, layout.MetricsRect.Height), w.Focus == workspaceFocusMetrics)

		return lipgloss.JoinHorizontal(lipgloss.Top, logs, metrics)
	}

	logs := borderedPane("Logs", w.renderLogsPane(layout.LogsRect.Width, layout.LogsRect.Height), w.Focus == workspaceFocusLogs)
	metrics := borderedPane("Metrics", w.renderMetricsPane(layout.MetricsRect.Width, layout.MetricsRect.Height), w.Focus == workspaceFocusMetrics)

	return lipgloss.JoinVertical(lipgloss.Left, logs, metrics)
}

func (w ChatWorkspace[C]) renderLogsPane(width, height int) string {
	innerWidth, innerHeight := borderedInnerSize(width, height)
	if innerHeight <= 0 {
		return ""
	}

	logs, _, _ := w.Logs.View(innerWidth, innerHeight)

	return logs
}

func (w ChatWorkspace[C]) renderMetricsPane(width, height int) string {
	innerWidth, innerHeight := borderedInnerSize(width, height)
	if innerHeight <= 0 {
		return ""
	}

	if !w.HasMetrics {
		return lipgloss.Place(innerWidth, innerHeight, lipgloss.Center, lipgloss.Center, "No metrics yet")
	}

	return w.Metrics.View(innerWidth, innerHeight)
}

func renderLogEntries(entries []observability.PanelEntry, width int, format *string, locale language.Tag) []string {
	lines := make([]string, 0, len(entries))
	lineStyle := lipgloss.NewStyle().Width(width)

	for _, entry := range entries {
		parts := []string{renderLogLevel(entry.Level)}

		if ts := timestamp.Format(entry.Timestamp, format, locale); ts != "" {
			parts = append([]string{theme.Dim.Render(ts)}, parts...)
		}

		if entry.Scope != "" {
			parts = append(parts, theme.Info.Render(entry.Scope))
		}

		parts = append(parts, entry.Message)

		if len(entry.Fields) > 0 {
			fields := make([]string, 0, len(entry.Fields))
			for _, field := range entry.Fields {
				fields = append(fields, fmt.Sprintf("%s=%s", field.Key, field.Value))
			}

			parts = append(parts, theme.Dim.Render(strings.Join(fields, " ")))
		}

		line := strings.Join(parts, " ")
		lines = append(lines, lineStyle.Render(line))
	}

	return lines
}

func borderedPane(title, content string, focused bool) string {
	style := theme.PaneBorder
	if focused {
		style = theme.PaneBorderFocused
	}

	parts := []string{theme.Bold.Render(title)}
	if content != "" {
		parts = append(parts, content)
	}

	return style.Render(lipgloss.JoinVertical(lipgloss.Left, parts...))
}

// borderedInnerSize returns the content-area dimensions for a pane
// wrapped by borderedPane. Width loses the two border columns;
// height loses the two border rows plus the one-row title that
// borderedPane prepends above the content.
func borderedInnerSize(width, height int) (int, int) {
	if width <= 2 {
		width = 2
	}
	if height <= 3 {
		height = 3
	}

	return width - 2, height - 3
}

func renderLogLevel(level string) string {
	switch strings.ToUpper(level) {
	case "ERROR":
		return theme.Error.Render(level)
	case "WARN", "WARNING":
		return theme.Warning.Render(level)
	case "DEBUG":
		return theme.Dim.Render(level)
	default:
		return theme.Info.Render(level)
	}
}
