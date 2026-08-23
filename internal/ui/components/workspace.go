package components

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"golang.org/x/text/language"

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

type observabilityLayout struct {
	LogsRect    uv.Rectangle
	MetricsRect uv.Rectangle
}

// observabilityDrawer owns the two panes drawn in the area that
// MainLayout reserves for local observability.
type observabilityDrawer struct {
	Logs       FeedView
	Metrics    MetricsPane
	HasMetrics bool
	Open       bool
	Fullscreen bool
	Focus      workspaceFocus
	keyMap     WorkspaceKeyMap
	bounds     uv.Rectangle

	logEntries      []observability.PanelEntry
	timestampFormat *string
	locale          language.Tag
}

func newObservabilityDrawer() observabilityDrawer {
	return observabilityDrawer{
		Logs:   NewFeedView("No logs yet", "new logs"),
		keyMap: DefaultWorkspaceKeyMap,
		Focus:  workspaceFocusLogs,
		locale: timestamp.CurrentLocale(),
	}
}

func (d observabilityDrawer) withMetrics(metrics MetricsPane) observabilityDrawer {
	d.Metrics = metrics
	d.HasMetrics = true

	return d
}

func (d observabilityDrawer) Init() tea.Cmd {
	if !d.HasMetrics {
		return nil
	}

	return d.Metrics.Init()
}

func (d observabilityDrawer) Update(msg tea.Msg) (ui.Component, tea.Cmd) {
	switch msg := msg.(type) {
	case ui.BoundsMsg:
		d.bounds = msg.Rect
		return d.updateChildBounds()

	case TimestampFormatMsg:
		d.timestampFormat = ptr.CloneString(msg.Format)
		d.locale = msg.Locale
		d = d.refreshLogs()

		return d, nil

	case tea.KeyPressMsg:
		switch {
		case ui.Matches(msg, d.keyMap.ToggleObservability):
			d.Open = !d.Open
			if !d.Open {
				d.Fullscreen = false
			}

			if d.Open {
				return d.showMetrics()
			}

			return d, nil

		case d.Open && ui.Matches(msg, d.keyMap.ToggleFullscreen):
			d.Fullscreen = !d.Fullscreen

			return d, nil

		case d.Fullscreen && ui.Matches(msg, d.keyMap.NextPane):
			if !d.HasMetrics {
				return d, nil
			}

			if d.Focus == workspaceFocusLogs {
				d.Focus = workspaceFocusMetrics
			} else {
				d.Focus = workspaceFocusLogs
			}

			return d, nil

		case d.Fullscreen && ui.Matches(msg, d.keyMap.ExitFullscreen):
			d.Fullscreen = false

			return d, nil
		}
	}

	if !d.Open {
		return d, nil
	}

	return d.updatePanes(msg)
}

func (d observabilityDrawer) updatePanes(msg tea.Msg) (ui.Component, tea.Cmd) {
	if _, ok := msg.(tea.KeyPressMsg); ok && d.Fullscreen {
		if d.Focus == workspaceFocusMetrics && d.HasMetrics {
			metrics, cmd := d.Metrics.Update(msg)
			d.Metrics = metrics.(MetricsPane)

			return d, cmd
		}

		logs, cmd := d.Logs.Update(msg)
		d.Logs = logs.(FeedView)

		return d, cmd
	}

	if key, ok := msg.(tea.KeyPressMsg); ok && isChatScrollKey(DefaultChatViewKeyMap, key) {
		return d, nil
	}

	var cmds []tea.Cmd

	logs, cmd := d.Logs.Update(msg)
	d.Logs = logs.(FeedView)
	cmds = append(cmds, cmd)

	if d.HasMetrics {
		metrics, cmd := d.Metrics.Update(msg)
		d.Metrics = metrics.(MetricsPane)
		cmds = append(cmds, cmd)
	}

	return d, tea.Batch(cmds...)
}

func (d observabilityDrawer) consumesKey(msg tea.KeyPressMsg) bool {
	return ui.Matches(msg, d.keyMap.ToggleObservability) ||
		(d.Open && ui.Matches(msg, d.keyMap.ToggleFullscreen)) ||
		d.Fullscreen
}

func isChatScrollKey(km ChatViewKeyMap, msg tea.KeyPressMsg) bool {
	return ui.Matches(msg, km.PageUp) || ui.Matches(msg, km.PageDown) ||
		ui.Matches(msg, km.ScrollUp) || ui.Matches(msg, km.ScrollDown)
}

func (d observabilityDrawer) KeyBindings() []ui.KeyBinding {
	bindings := []ui.KeyBinding{d.keyMap.ToggleObservability}

	if d.Open {
		bindings = append(bindings, d.keyMap.ToggleFullscreen)
	}

	if !d.Fullscreen {
		return bindings
	}

	if d.HasMetrics {
		bindings = append(bindings, d.keyMap.NextPane)
	}
	bindings = append(bindings, d.keyMap.ExitFullscreen)

	if d.Focus == workspaceFocusMetrics && d.HasMetrics {
		return append(bindings, d.Metrics.KeyBindings()...)
	}

	return append(bindings, d.Logs.KeyBindings()...)
}

func (d observabilityDrawer) height(totalHeight int) int {
	if !d.Open || d.Fullscreen {
		return 0
	}

	height := max(totalHeight*30/100, minObservabilityDrawerHeight)
	if height >= totalHeight {
		height = totalHeight / 2
	}

	return max(height, 0)
}

func (d observabilityDrawer) SetLogEntries(entries []observability.PanelEntry) observabilityDrawer {
	d.logEntries = entries

	return d.refreshLogs()
}

func (d observabilityDrawer) StatusItems() []ui.StatusItem {
	if !d.Open {
		return nil
	}

	label := "obs drawer"
	if d.Fullscreen {
		if d.Focus == workspaceFocusMetrics {
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
		Compact:  "obs",
	}}
}

func (d observabilityDrawer) updateChildBounds() (ui.Component, tea.Cmd) {
	if !d.Open {
		return d, nil
	}

	layout := d.layout(d.bounds)
	d = d.refreshLogs()

	var cmds []tea.Cmd

	logs, cmd := d.Logs.Update(ui.BoundsMsg{Rect: borderedContentRect(layout.LogsRect)})
	d.Logs = logs.(FeedView)
	cmds = append(cmds, cmd)

	if d.HasMetrics {
		metrics, cmd := d.Metrics.Update(ui.BoundsMsg{Rect: borderedContentRect(layout.MetricsRect)})
		d.Metrics = metrics.(MetricsPane)
		cmds = append(cmds, cmd)
	}

	return d, tea.Batch(cmds...)
}

func (d observabilityDrawer) showMetrics() (ui.Component, tea.Cmd) {
	if !d.HasMetrics {
		return d, nil
	}

	metrics, cmd := d.Metrics.Update(metricsPaneShownMsg{})
	d.Metrics = metrics.(MetricsPane)

	return d, cmd
}

func (d observabilityDrawer) refreshLogs() observabilityDrawer {
	layout := d.layout(d.bounds)
	width, _ := borderedInnerSize(layout.LogsRect.Dx(), layout.LogsRect.Dy())
	d.Logs = d.Logs.SetLines(renderLogEntries(d.logEntries, width, d.timestampFormat, d.locale))

	return d
}

func (d observabilityDrawer) layout(area uv.Rectangle) observabilityLayout {
	if d.Fullscreen && area.Dx() >= 140 {
		logsWidth := area.Dx() * 65 / 100

		return observabilityLayout{
			LogsRect:    uv.Rect(area.Min.X, area.Min.Y, logsWidth, area.Dy()),
			MetricsRect: uv.Rect(area.Min.X+logsWidth, area.Min.Y, area.Dx()-logsWidth, area.Dy()),
		}
	}

	logsHeight := area.Dy() * 70 / 100
	if d.Fullscreen {
		logsHeight = area.Dy() * 60 / 100
	}
	if logsHeight < 3 {
		logsHeight = area.Dy()
	}

	return observabilityLayout{
		LogsRect:    uv.Rect(area.Min.X, area.Min.Y, area.Dx(), logsHeight),
		MetricsRect: uv.Rect(area.Min.X, area.Min.Y+logsHeight, area.Dx(), area.Dy()-logsHeight),
	}
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

func borderedContentRect(area uv.Rectangle) uv.Rectangle {
	left := theme.PaneBorder.GetBorderLeftSize()
	right := theme.PaneBorder.GetBorderRightSize()
	top := theme.PaneBorder.GetBorderTopSize()
	bottom := theme.PaneBorder.GetBorderBottomSize()

	return uv.Rect(
		area.Min.X+left,
		area.Min.Y+top+1,
		max(area.Dx()-left-right, 0),
		max(area.Dy()-top-bottom-1, 0),
	)
}

func borderedInnerSize(width, height int) (int, int) {
	area := borderedContentRect(uv.Rect(0, 0, width, height))

	return area.Dx(), area.Dy()
}
