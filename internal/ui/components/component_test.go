package components

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	uvscreen "github.com/charmbracelet/ultraviolet/screen"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
)

func renderToBuffer(component ui.Component, width, height int) string {
	screen := uv.NewScreenBuffer(width, height)
	component.Draw(screen, screen.Bounds())

	return screen.Render()
}

func TestChatViewDrawDoesNotWriteOutsideAssignedRectangle(t *testing.T) {
	view := NewChatView[testKind](
		func() WindowContent { return WindowContent{Channel: "#general"} },
		"#general",
		domain.KindChannel,
		"testuser",
		"",
	)
	view.input.input.palette.open = true
	view.input.popover.completion = command.Completion{
		Visible: true,
		Suggestions: []command.Suggestion{
			{Value: "/join", Label: "/join"},
			{Value: "/part", Label: "/part"},
		},
	}

	screen := uv.NewScreenBuffer(20, 8)
	uvscreen.FillArea(screen, &uv.Cell{Content: ".", Width: 1}, screen.Bounds())
	area := uv.Rect(5, 3, 10, 2)

	view.Draw(screen, area)

	for y := screen.Bounds().Min.Y; y < screen.Bounds().Max.Y; y++ {
		for x := screen.Bounds().Min.X; x < screen.Bounds().Max.X; x++ {
			if x >= area.Min.X && x < area.Max.X && y >= area.Min.Y && y < area.Max.Y {
				continue
			}

			require.Equal(t, ".", screen.CellAt(x, y).Content,
				"Draw wrote outside its assigned rectangle at (%d,%d)", x, y)
		}
	}
}

func TestInputBarReleaseOverPopoverEndsEditorDrag(t *testing.T) {
	bar := NewInputBar()
	bar.bounds = uv.Rect(10, 5, 40, 4)
	bar.input.mouseSelecting = true
	bar.popover.completion = command.Completion{
		Visible:     true,
		Suggestions: []command.Suggestion{{Value: "/join", Label: "/join"}},
	}
	updated, _ := bar.updateChildBounds()
	bar = updated.(InputBar)
	popover := bar.layout(bar.bounds).popover

	updated, _ = bar.Update(tea.MouseReleaseMsg{
		X:      popover.Min.X,
		Y:      popover.Min.Y,
		Button: tea.MouseLeft,
	})
	bar = updated.(InputBar)

	require.False(t, bar.input.mouseSelecting)
}

func TestCollapsedInputBoundsStillReleaseEditorDrag(t *testing.T) {
	bar := NewInputBar()
	bar.input.mouseSelecting = true
	updated, _ := bar.Update(ui.BoundsMsg{})
	bar = updated.(InputBar)

	updated, _ = bar.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft})
	bar = updated.(InputBar)

	require.False(t, bar.input.mouseSelecting)
}

func TestCollapsedChatViewBoundsStillReleaseEditorDrag(t *testing.T) {
	view := NewChatView[testKind](
		func() WindowContent { return WindowContent{Channel: "#general"} },
		"#general",
		domain.KindChannel,
		"testuser",
		"",
	)
	view.input.input.mouseSelecting = true

	updated, _ := view.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft})
	view = updated.(ChatView[testKind])

	require.False(t, view.input.input.mouseSelecting)
}

func TestObservabilityDrawerChildBoundsMatchDrawLayout(t *testing.T) {
	drawer := newObservabilityDrawer().withMetrics(NewMetricsPane(t.Context, nil))
	area := uv.Rect(3, 2, 100, 30)

	updated, _ := drawer.Update(toggleObservabilityKey())
	drawer = updated.(observabilityDrawer)
	updated, _ = drawer.Update(ui.BoundsMsg{Rect: area})
	drawer = updated.(observabilityDrawer)
	layout := drawer.layout(area)

	require.Equal(t, borderedContentRect(layout.LogsRect), drawer.Logs.bounds)
	require.Equal(t, borderedContentRect(layout.MetricsRect), drawer.Metrics.feed.bounds)

	updated, _ = drawer.Update(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl})
	drawer = updated.(observabilityDrawer)
	updated, _ = drawer.Update(ui.BoundsMsg{Rect: area})
	drawer = updated.(observabilityDrawer)
	layout = drawer.layout(area)

	require.Equal(t, borderedContentRect(layout.LogsRect), drawer.Logs.bounds)
	require.Equal(t, borderedContentRect(layout.MetricsRect), drawer.Metrics.feed.bounds)
}
