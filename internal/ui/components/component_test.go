package components

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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

// TestInputBarHeightCountsEveryRowTheLayoutPlaces covers the two halves
// of the input bar's own geometry. `Height` reports the rows the bar
// needs and `layout` allots them to its bands. ChatView reserves
// `Height` rows at the bottom and hands the bar exactly that rectangle,
// so a row the height omits is one the layout has no room for, and a
// row the height counts and the layout allots to nothing is one no band
// can be drawn into.
//
// Both now read `bandRows`, so this covers the arithmetic that turns
// those rows into a total and into rectangles. Which band gives way in
// an area too short for all of them is a separate decision, and
// TestInputBarKeepsTheInputRowInAShortArea is what covers it.
func TestInputBarHeightCountsEveryRowTheLayoutPlaces(t *testing.T) {
	cases := map[string]struct {
		popover        bool
		palette        bool
		pasteFlattened bool
	}{
		"plain":               {},
		"popover":             {popover: true},
		"palette":             {palette: true},
		"paste note":          {pasteFlattened: true},
		"popover hides note":  {popover: true, pasteFlattened: true},
		"palette and note":    {palette: true, pasteFlattened: true},
		"palette and popover": {palette: true, popover: true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			bar := NewInputBar()
			bar.input.palette.open = tc.palette
			bar.pasteFlattened = tc.pasteFlattened
			if tc.popover {
				bar.popover.completion = command.Completion{
					Visible: true,
					Suggestions: []command.Suggestion{
						{Value: "/join", Label: "/join"},
						{Value: "/part", Label: "/part"},
					},
				}
			}

			// Lay out in far more room than the bar needs, so the
			// layout places every row it wants and clips nothing.
			const ample = 40
			area := uv.Rect(4, 6, 40, ample)

			// The rows Height reserved are the ones at the bottom of the
			// area, each allotted to one band. Comparing the whole list
			// catches two bands sharing a row and a row no band covers,
			// which a count or a span from the top row to the bottom
			// would both accept. Where the bands are drawn is a separate
			// question, and TestChatViewPaintsChildrenIntoTheirLayout-
			// Rectangles is what answers it.
			var want []int
			for row := area.Max.Y - bar.Height(); row < area.Max.Y; row++ {
				want = append(want, row)
			}

			require.Equal(t, want, placedRows(bar.layout(area)))
		})
	}
}

// placedRows returns the screen rows an input-bar layout devotes to a
// band of its own, in order. A row appears once per band covering it,
// so two bands sharing a row show up as a repeat. The editor is left
// out: it sits on the input row, to the right of the prompt, so
// counting it would report that row twice for every layout.
func placedRows(layout inputBarLayout) []int {
	var rows []int
	for _, rect := range []uv.Rectangle{layout.palette, layout.popover, layout.note, layout.input} {
		for row := rect.Min.Y; row < rect.Max.Y; row++ {
			rows = append(rows, row)
		}
	}
	slices.Sort(rows)

	return rows
}

// TestChatViewPaintsChildrenIntoTheirLayoutRectangles locates each
// child by what it puts on the screen, so the rectangle Draw handed it
// is observable. One layout function serves the update and draw paths,
// which fixes what the rectangles are but not which child each one is
// given.
//
// The palette and the popover both sit above the input row and are
// placed by the same pass, so the case with both open is the one where
// handing a child its neighbour's rectangle is possible at all.
func TestChatViewPaintsChildrenIntoTheirLayoutRectangles(t *testing.T) {
	cases := map[string]struct {
		popover bool
		palette bool
	}{
		"bare":                {},
		"popover":             {popover: true},
		"palette":             {palette: true},
		"palette and popover": {palette: true, popover: true},
	}

	// Long enough to wrap to the full width, and enough of them to
	// reach every row, so the cells they cover are the message list's
	// whole rectangle and not a point inside it.
	content := WindowContent{Channel: "#general", Events: fillerMessages(40, 200)}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			view := NewChatView[testKind](
				func() WindowContent { return content },
				"#general",
				domain.KindChannel,
				"testuser",
				"",
			)
			view.input.input.palette.open = tc.palette
			if tc.popover {
				view.input.popover.completion = command.Completion{
					Visible: true,
					Suggestions: []command.Suggestion{
						{Value: "/join", Label: "/join"},
						{Value: "/part", Label: "/part"},
					},
				}
			}

			// A non-zero origin, which is what Root hands down while a
			// banner shows, so a Draw working from the corner of the
			// screen disagrees.
			area := uv.Rect(3, 2, 60, 20)
			updated, _ := view.Update(ui.BoundsMsg{Rect: area})
			view = updated.(ChatView[testKind])

			screen := uv.NewScreenBuffer(area.Max.X, area.Max.Y)
			uvscreen.FillArea(screen, &uv.Cell{Content: untouchedCell, Width: 1}, screen.Bounds())
			view.Draw(screen, area)
			rendered := screen.Render()

			layout := view.layoutRectsFor(area)
			bar := view.input.layout(layout.InputRect)

			type painted struct {
				PromptRows  []int
				PopoverRows []int
				PaletteRows []int
			}

			require.Equal(t, painted{
				PromptRows:  rowsOf(bar.input),
				PopoverRows: rowsOf(bar.popover),
				PaletteRows: rowsOf(bar.palette),
			}, painted{
				PromptRows:  rowsContaining(rendered, "testuser"),
				PopoverRows: rowsContaining(rendered, "/join", "/part"),
				PaletteRows: rowsContaining(rendered, "fg:"),
			})

			// The transcript fills its rectangle, so the cells it
			// reached bound the rectangle on every side.
			require.Equal(t, layout.MessageRect, paintedBox(screen, rendered, "filler"))
		})
	}
}

// fillerMessages returns count messages of the given body width, each
// containing the word "filler" so a test can find the cells they cover.
func fillerMessages(count, width int) []domain.Event {
	events := make([]domain.Event, count)
	for i := range events {
		events[i] = domain.Message{
			Source: domain.ClientSource("inst-filler", "filler"),
			Target: "#general",
			Body:   strings.Repeat("filler ", width/len("filler ")),
			At:     time.Date(2026, 4, 6, 12, 0, i, 0, time.UTC),
		}
	}

	return events
}

// untouchedCell is what a test paints the screen with before drawing,
// so the cells a component wrote into can be told from the cells
// nothing wrote into.
const untouchedCell = "."

// paintedBox returns the smallest rectangle covering every cell a
// component wrote into, on the rows its marker appears on. Drawing
// clears the cells of the rectangle it was given before writing into
// them, so a row runs to the rectangle's own edge even where its text
// stops short, which a rendered string cannot show: rendering drops
// the trailing spaces.
func paintedBox(screen uv.Screen, rendered, marker string) uv.Rectangle {
	box := uv.Rectangle{}
	bounds := screen.Bounds()

	for row, line := range strings.Split(rendered, "\n") {
		if !strings.Contains(line, marker) {
			continue
		}

		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			cell := screen.CellAt(x, row)
			if cell == nil || cell.Content == untouchedCell {
				continue
			}

			at := uv.Rect(x, row, 1, 1)
			if box.Empty() {
				box = at

				continue
			}
			box = box.Union(at)
		}
	}

	return box
}

// rowsContaining returns the rows of a rendered view whose text
// includes any of the markers.
func rowsContaining(view string, markers ...string) []int {
	var rows []int
	for row, line := range strings.Split(view, "\n") {
		if slices.ContainsFunc(markers, func(marker string) bool {
			return strings.Contains(line, marker)
		}) {
			rows = append(rows, row)
		}
	}

	return rows
}

// rowsOf returns every row a rectangle covers.
func rowsOf(rect uv.Rectangle) []int {
	var rows []int
	for row := rect.Min.Y; row < rect.Max.Y; row++ {
		rows = append(rows, row)
	}

	return rows
}

// TestInputBarKeepsTheInputRowInAShortArea covers what the bar gives up
// when it has less room than it asked for. ChatView clamps the bar's
// height to the window, so a short window is the ordinary way the bar
// is handed fewer rows than its bands want, and the input row is the
// one the operator is typing at.
func TestInputBarKeepsTheInputRowInAShortArea(t *testing.T) {
	type bands struct {
		Palette uv.Rectangle
		Popover uv.Rectangle
		Input   uv.Rectangle
	}

	// The bar wants four rows here: a palette, a two-row popover, and
	// the input row.
	bar := NewInputBar()
	bar.input.palette.open = true
	bar.popover.completion = command.Completion{
		Visible: true,
		Suggestions: []command.Suggestion{
			{Value: "/join", Label: "/join"},
			{Value: "/part", Label: "/part"},
		},
	}

	require.Equal(t, 4, bar.Height(), "the bar under test wants four rows")

	cases := map[int]bands{
		0: {},
		1: {Input: uv.Rect(4, 9, 40, 1)},
		2: {Popover: uv.Rect(4, 8, 40, 1), Input: uv.Rect(4, 9, 40, 1)},
		3: {Popover: uv.Rect(4, 7, 40, 2), Input: uv.Rect(4, 9, 40, 1)},
		4: {
			Palette: uv.Rect(4, 6, 40, 1),
			Popover: uv.Rect(4, 7, 40, 2),
			Input:   uv.Rect(4, 9, 40, 1),
		},
	}

	for height, want := range cases {
		t.Run(fmt.Sprintf("%d rows", height), func(t *testing.T) {
			area := uv.Rect(4, 10-height, 40, height)
			layout := bar.layout(area)

			require.Equal(t, want, bands{
				Palette: occupied(layout.palette),
				Popover: occupied(layout.popover),
				Input:   occupied(layout.input),
			})
		})
	}
}

// occupied returns a band's rectangle, and the zero rectangle when the
// band has no rows. The layout gives such a band a zero-height
// rectangle, which `Empty` already reports as empty. Normalising it to
// the zero value lets these tests compare with `uv.Rectangle{}`.
func occupied(rect uv.Rectangle) uv.Rectangle {
	if rect.Empty() {
		return uv.Rectangle{}
	}

	return rect
}

// TestChatViewKeepsTheInputRowInAShortWindow is the same priority one
// level up. The header and the input bar both ask for fixed rows, and a
// window with too few gives them to the bar: a window showing its title
// and nothing to type into is worse than one showing neither.
func TestChatViewKeepsTheInputRowInAShortWindow(t *testing.T) {
	type rows struct {
		Message int
		Input   int
	}

	// The header renders two rows for a channel with a topic, and
	// the bar wants one, so the transcript gets nothing until the
	// fourth.
	cases := map[int]rows{
		0: {},
		1: {Input: 1},
		2: {Input: 1},
		3: {Input: 1},
		4: {Message: 1, Input: 1},
	}

	for height, want := range cases {
		t.Run(fmt.Sprintf("%d rows", height), func(t *testing.T) {
			view := NewChatView[testKind](
				func() WindowContent { return WindowContent{Channel: "#general"} },
				"#general",
				domain.KindChannel,
				"testuser",
				"a topic long enough to render a header",
			)

			layout := view.layoutRectsFor(uv.Rect(3, 2, 60, height))

			require.Equal(t, want, rows{
				Message: layout.MessageRect.Dy(),
				Input:   layout.InputRect.Dy(),
			})
		})
	}
}
