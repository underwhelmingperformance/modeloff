package components

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
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
type ChatWorkspace struct {
	Chat       ChatView
	Logs       FeedView
	Metrics    MetricsPane
	HasMetrics bool
	Open       bool
	Fullscreen bool
	Focus      workspaceFocus
	keyMap     WorkspaceKeyMap
	bounds     ui.Rect

	logEntries []observability.PanelEntry
}

// NewChatWorkspace creates the chat content workspace.
func NewChatWorkspace(chat ChatView) ChatWorkspace {
	return ChatWorkspace{
		Chat:   chat,
		Logs:   NewFeedView("No logs yet", "new logs"),
		keyMap: DefaultWorkspaceKeyMap,
		Focus:  workspaceFocusLogs,
	}
}

// WithMetrics attaches a metrics pane to the workspace.
func (w ChatWorkspace) WithMetrics(metrics MetricsPane) ChatWorkspace {
	w.Metrics = metrics
	w.HasMetrics = true

	return w
}

// Init implements ui.Model.
func (w ChatWorkspace) Init() tea.Cmd {
	cmds := []tea.Cmd{w.Chat.Init()}
	if w.HasMetrics {
		cmds = append(cmds, w.Metrics.Init())
	}

	return tea.Batch(cmds...)
}

// Update implements ui.Model.
func (w ChatWorkspace) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case ui.BoundsMsg:
		w.bounds = msg.Rect
		return w.updateChildBounds()

	case tea.KeyMsg:
		switch {
		case key.Matches(msg, w.keyMap.ToggleObservability):
			w.Open = !w.Open
			if !w.Open {
				w.Fullscreen = false
			}
			return w.updateChildBounds()

		case w.Open && key.Matches(msg, w.keyMap.ToggleFullscreen):
			w.Fullscreen = !w.Fullscreen
			return w.updateChildBounds()

		case w.Fullscreen && key.Matches(msg, w.keyMap.NextPane):
			if !w.HasMetrics {
				return w, nil
			}

			if w.Focus == workspaceFocusLogs {
				w.Focus = workspaceFocusMetrics
			} else {
				w.Focus = workspaceFocusLogs
			}

			return w, nil

		case w.Fullscreen && key.Matches(msg, w.keyMap.ExitFullscreen):
			w.Fullscreen = false
			return w.updateChildBounds()
		}
	}

	if w.Open && w.Fullscreen {
		return w.updateFullscreen(msg)
	}

	return w.updateSplit(msg)
}

func (w ChatWorkspace) updateFullscreen(msg tea.Msg) (ui.Model, tea.Cmd) {
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

func (w ChatWorkspace) updateSplit(msg tea.Msg) (ui.Model, tea.Cmd) {
	var cmds []tea.Cmd

	updatedChat, cmd := w.Chat.Update(msg)
	w.Chat = updatedChat.(ChatView)
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
func (w ChatWorkspace) KeyBindings() []key.Binding {
	bindings := []key.Binding{w.keyMap.ToggleObservability}

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

// View implements ui.Model.
func (w ChatWorkspace) View(width, height int) string {
	layout := w.layout(width, height)
	if !w.Open {
		return w.Chat.View(width, height)
	}

	if w.Fullscreen {
		return w.renderObservabilityLayout(layout, width, height)
	}

	chat := w.Chat.View(width, layout.ChatRect.Height)
	obs := w.renderObservabilityLayout(layout, width, layout.ObsRect.Height)

	return lipgloss.JoinVertical(lipgloss.Left, chat, obs)
}

// SetLogEntries updates the log pane content.
func (w ChatWorkspace) SetLogEntries(entries []observability.PanelEntry) ChatWorkspace {
	w.logEntries = entries

	return w.refreshLogs()
}

// WantsNickListHidden hides the nick list while fullscreen observability is active.
func (w ChatWorkspace) WantsNickListHidden() bool {
	return w.Open && w.Fullscreen
}

// StatusItems implements ui.StatusProvider.
func (w ChatWorkspace) StatusItems() []ui.StatusItem {
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

func (w ChatWorkspace) updateChildBounds() (ui.Model, tea.Cmd) {
	layout := w.currentLayout()
	var cmds []tea.Cmd

	if !w.Fullscreen {
		updatedChat, cmd := w.Chat.Update(ui.BoundsMsg{Rect: layout.ChatRect})
		w.Chat = updatedChat.(ChatView)
		cmds = append(cmds, cmd)
	}

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

func (w ChatWorkspace) refreshLogs() ChatWorkspace {
	layout := w.currentLayout()
	innerWidth, _ := borderedInnerSize(layout.LogsRect.Width, layout.LogsRect.Height)
	w.Logs = w.Logs.SetLines(renderLogEntries(w.logEntries, innerWidth))

	return w
}

func (w ChatWorkspace) currentLayout() workspaceLayout {
	return w.layout(w.bounds.Width, w.bounds.Height)
}

func (w ChatWorkspace) layout(width, height int) workspaceLayout {
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

func (w ChatWorkspace) renderObservabilityLayout(layout workspaceLayout, width, height int) string {
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

func (w ChatWorkspace) renderLogsPane(width, height int) string {
	innerWidth, innerHeight := borderedInnerSize(width, height)
	logs, _, _ := w.Logs.View(innerWidth, innerHeight)

	return logs
}

func (w ChatWorkspace) renderMetricsPane(width, height int) string {
	innerWidth, innerHeight := borderedInnerSize(width, height)
	if !w.HasMetrics {
		return lipgloss.Place(innerWidth, innerHeight, lipgloss.Center, lipgloss.Center, "No metrics yet")
	}

	return w.Metrics.View(innerWidth, innerHeight)
}

func renderLogEntries(entries []observability.PanelEntry, width int) []string {
	lines := make([]string, 0, len(entries))

	for _, entry := range entries {
		parts := []string{
			theme.Dim.Render(entry.Timestamp.Format("15:04:05")),
			renderLogLevel(entry.Level),
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
		lines = append(lines, lipgloss.NewStyle().Width(width).Render(line))
	}

	return lines
}

func borderedPane(title, content string, focused bool) string {
	style := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.ANSIColor(8))

	if focused {
		style = style.BorderForeground(lipgloss.ANSIColor(6))
	}

	return style.Render(fmt.Sprintf("%s\n%s", theme.Bold.Render(title), content))
}

func borderedInnerSize(width, height int) (int, int) {
	if width <= 2 {
		width = 2
	}
	if height <= 2 {
		height = 2
	}

	return width - 2, height - 2
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
