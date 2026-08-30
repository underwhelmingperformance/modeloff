package ui

import (
	"image"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/require"
)

// markerLayer paints one character over its whole rectangle and takes
// a key only when that key is its own marker.
//
// It records the keys it was offered through a pointer the test holds,
// because the stack keeps a layer's replacement only when it took the
// key: a layer that declined would otherwise have nowhere to leave the
// evidence that it was asked at all.
type markerLayer struct {
	marker  rune
	offered *[]rune
	taken   []rune
}

func (m markerLayer) Init() tea.Cmd { return nil }

func (m markerLayer) Update(msg tea.Msg) (Component, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		m.taken = append(append([]rune(nil), m.taken...), msg.Code)

	case setMarkerMsg:
		m.marker = msg.marker
	}

	return m, nil
}

// setMarkerMsg changes which key a markerLayer takes, so a test can
// send a layer an ordinary message and then find out which version of
// it the next key reached.
type setMarkerMsg struct {
	marker rune
}

func (m markerLayer) Draw(screen uv.Screen, area uv.Rectangle) {
	for y := area.Min.Y; y < area.Max.Y; y++ {
		for x := area.Min.X; x < area.Max.X; x++ {
			screen.SetCell(x, y, uv.NewCell(screen.WidthMethod(), string(m.marker)))
		}
	}
}

func (m markerLayer) UpdateKeys(msg tea.Msg) (KeyHandler, tea.Cmd) {
	updated, cmd := m.Update(msg)

	return updated.(markerLayer), cmd
}

func (m markerLayer) HandleKey(msg tea.KeyPressMsg) (KeyHandler, bool, tea.Cmd) {
	if m.offered != nil {
		*m.offered = append(*m.offered, msg.Code)
	}

	if msg.Code != m.marker {
		return m, false, nil
	}

	m.taken = append(append([]rune(nil), m.taken...), msg.Code)

	return m, true, nil
}

// stackOf pushes layers in order, front last. It drops the commands
// the placement produced, which the tests using it do not read.
func stackOf(t *testing.T, layers ...Layer) LayerStack {
	t.Helper()

	var stack LayerStack
	for _, layer := range layers {
		stack, _ = stack.Push(layer)
	}

	return stack
}

// stackRows renders a stack over a prefilled screen and returns each
// row as a string, so a test can read what covered what.
func stackRows(t *testing.T, s LayerStack, w, h int) []string {
	t.Helper()

	screen := uv.NewScreenBuffer(w, h)
	for y := range h {
		for x := range w {
			screen.SetCell(x, y, uv.NewCell(screen.WidthMethod(), "."))
		}
	}

	s.Draw(screen, screen.Bounds())

	rows := make([]string, 0, h)
	for y := range h {
		var row strings.Builder
		for x := range w {
			row.WriteString(screen.CellAt(x, y).Content)
		}

		rows = append(rows, row.String())
	}

	return rows
}

func TestLayerStack_draws_the_front_layer_over_the_ones_behind_it(t *testing.T) {
	stack := stackOf(t,
		NewKeyLayer("back", markerLayer{marker: 'a'}, uv.Rect(0, 0, 4, 2)),
		NewKeyLayer("front", markerLayer{marker: 'b'}, uv.Rect(2, 0, 4, 1)),
	)

	require.Equal(t, []string{
		"aabbbb",
		"aaaa..",
	}, stackRows(t, stack, 6, 2))
}

func TestLayerStack_clips_a_layer_to_the_area_it_is_drawn_in(t *testing.T) {
	stack := stackOf(t,
		NewKeyLayer("wide", markerLayer{marker: 'a'}, uv.Rect(0, 0, 10, 4)),
	)

	require.Equal(t, []string{
		"aaa",
		"aaa",
	}, stackRows(t, stack, 3, 2))
}

// TestLayerStack_does_not_write_through_a_copy pins the value
// semantics the rest of the UI relies on. A component holding a stack
// is copied whenever its parent updates it, so a mutator that wrote
// into the shared backing array would change a stack somebody else is
// still holding. Every method that changes a stack is exercised here,
// because one of them writing in place is enough to lose the property.
func TestLayerStack_does_not_write_through_a_copy(t *testing.T) {
	base := stackOf(t,
		NewKeyLayer("first", markerLayer{marker: 'a'}, uv.Rect(0, 0, 2, 1)),
	)

	// Each of these derives a stack from base. Reading base back
	// unchanged at the end is what says none of them wrote into it.
	pushed, _ := base.Push(NewKeyLayer("second", markerLayer{marker: 'b'}, uv.Rect(0, 0, 2, 1)))
	replaced, _ := base.Push(NewKeyLayer("first", markerLayer{marker: 'c'}, uv.Rect(0, 0, 2, 1)))
	removed := pushed.Remove("second")
	updated, _ := base.Update(setMarkerMsg{marker: 'd'})
	updatedOne, _ := base.UpdateID("first", setMarkerMsg{marker: 'e'})
	moved, _ := base.Move("first", uv.Rect(2, 0, 2, 1))
	resized, _ := base.Resize(uv.Rect(2, 0, 2, 1))
	keyed, _, _ := base.HandleKey(tea.KeyPressMsg{Code: 'a'})

	require.Equal(t, []string{"bb"}, stackRows(t, pushed, 2, 1))
	require.Equal(t, []string{"cc"}, stackRows(t, replaced, 2, 1))
	require.Equal(t, []string{"aa"}, stackRows(t, removed, 2, 1))
	require.Equal(t, []string{"dd"}, stackRows(t, updated, 2, 1))
	require.Equal(t, []string{"ee"}, stackRows(t, updatedOne, 2, 1))
	require.Equal(t, []string{"..aa"}, stackRows(t, moved, 4, 1))
	require.Equal(t, []string{"..aa"}, stackRows(t, resized, 4, 1))
	require.Equal(t, []string{"aa"}, stackRows(t, keyed, 2, 1))

	require.Equal(t, []string{"aa.."}, stackRows(t, base, 4, 1),
		"a mutator wrote into the stack it was derived from")
}

// TestLayerStack_runs_a_key_against_the_updated_layer pins that a key
// layer's two references hold the same value. The stack keeps a key
// layer as both its content and its key handler, and updating one of
// them alone would run the next key against the version an ordinary
// message had already replaced.
func TestLayerStack_runs_a_key_against_the_updated_layer(t *testing.T) {
	stack := stackOf(t,
		NewKeyLayer("front", markerLayer{marker: 'a'}, uv.Rect(0, 0, 1, 1)),
	)

	stack, _ = stack.Update(setMarkerMsg{marker: 'b'})

	_, handled, _ := stack.HandleKey(tea.KeyPressMsg{Code: 'b'})
	require.True(t, handled, "the key ran against the layer as it was before the update")

	moved, _ := stack.UpdateID("front", setMarkerMsg{marker: 'c'})

	_, handled, _ = moved.HandleKey(tea.KeyPressMsg{Code: 'c'})
	require.True(t, handled, "UpdateID left the key handler behind")
}

// TestLayerStack_moves_one_layer_and_keeps_its_state pins that
// repositioning a layer does not replace it. A holder recomputes an
// overlay's rectangle as its own layout changes, and the overlay's
// component must come through that with the state it had.
func TestLayerStack_moves_one_layer_and_keeps_its_state(t *testing.T) {
	stack := stackOf(t,
		NewKeyLayer("front", markerLayer{marker: 'a'}, uv.Rect(0, 0, 1, 1)),
	)

	stack, _ = stack.Update(setMarkerMsg{marker: 'b'})

	moved := uv.Rect(2, 0, 2, 1)
	stack, _ = stack.Move("front", moved)

	require.Equal(t, []string{"..bb"}, stackRows(t, stack, 4, 1))

	_, handled, _ := stack.HandleKey(tea.KeyPressMsg{Code: 'b'})
	require.True(t, handled,
		"the moved layer no longer takes the key its last update told it to")

	absent, cmd := stack.Move("absent", uv.Rect(0, 0, 4, 1))

	require.Nil(t, cmd)
	require.Equal(t, []string{"..bb"}, stackRows(t, absent, 4, 1),
		"moving an id the stack does not hold must change nothing")
}

// TestLayerStack_tells_a_layer_where_it_has_been_put pins that a
// layout change reaches the layer as well as the stack. A component
// reads its geometry from BoundsMsg and hit-tests against what it
// last received, so a stack that moved a layer silently would leave it
// drawing in one place and hit-testing in another.
func TestLayerStack_tells_a_layer_where_it_has_been_put(t *testing.T) {
	var told []uv.Rectangle

	first := uv.Rect(1, 0, 2, 1)
	second := uv.Rect(4, 0, 3, 1)

	stack, cmd := LayerStack{}.Push(NewKeyLayer("front", boundsLayer{told: &told}, first))

	require.Equal(t, []uv.Rectangle{first}, told,
		"a layer must be told where it was put when it is put there")
	requireBoundsCmd(t, cmd, first)

	stack, cmd = stack.Move("front", second)

	require.Equal(t, []uv.Rectangle{first, second}, told)
	requireBoundsCmd(t, cmd, second)

	third := uv.Rect(0, 0, 8, 1)

	_, cmd = stack.Resize(third)

	require.Equal(t, []uv.Rectangle{first, second, third}, told)
	requireBoundsCmd(t, cmd, third)
}

// TestLayerStack_attaches_a_layer_it_is_given pins that pushing a
// layer calls its Init and returns the command Init gave back. A layer
// arriving part way through a session is attached the same way a
// component in the tree is at startup, and one whose Init returns a
// command to start a timer or a load would otherwise never run it.
// The batch's order is not asserted: Init's command and the command the
// layer returned from its BoundsMsg come back together, and the runtime
// runs a batch's commands concurrently.
func TestLayerStack_attaches_a_layer_it_is_given(t *testing.T) {
	type startedMsg struct{}

	var told []uv.Rectangle

	rect := uv.Rect(0, 0, 2, 1)
	layer := NewKeyLayer("front", boundsLayer{told: &told, initial: startedMsg{}}, rect)

	_, cmd := LayerStack{}.Push(layer)

	require.Equal(t, []uv.Rectangle{rect}, told,
		"a pushed layer must be told where it is")

	var started []tea.Msg
	for _, msg := range batchedMsgs(t, cmd) {
		if _, ok := msg.(startedMsg); ok {
			started = append(started, msg)
		}
	}

	require.Equal(t, []tea.Msg{startedMsg{}}, started,
		"a pushed layer must have its Init run")
}

// batchedMsgs runs a command and returns the messages it produced,
// flattening the batch a placement returns.
func batchedMsgs(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()

	switch msg := cmd().(type) {
	case tea.BatchMsg:
		msgs := make([]tea.Msg, 0, len(msg))
		for _, inner := range msg {
			msgs = append(msgs, batchedMsgs(t, inner)...)
		}

		return msgs

	default:
		return []tea.Msg{msg}
	}
}

// requireBoundsCmd runs the command a placement returned and asserts
// it names the rectangle the layer was sent. A stack that
// delivered the BoundsMsg and dropped the command would leave a layer
// whose own resize work never runs.
func requireBoundsCmd(t *testing.T, cmd tea.Cmd, rect uv.Rectangle) {
	t.Helper()

	require.Equal(t, []tea.Msg{BoundsMsg{Rect: rect}}, batchedMsgs(t, cmd))
}

// boundsLayer records every rectangle it was told it occupies. Its
// Init returns a command producing initial, so a test can tell whether
// the stack attached it.
type boundsLayer struct {
	told    *[]uv.Rectangle
	initial tea.Msg
}

func (b boundsLayer) Init() tea.Cmd {
	if b.initial == nil {
		return nil
	}

	return func() tea.Msg { return b.initial }
}

func (b boundsLayer) Update(msg tea.Msg) (Component, tea.Cmd) {
	bounds, ok := msg.(BoundsMsg)
	if !ok {
		return b, nil
	}

	*b.told = append(*b.told, bounds.Rect)

	// A real component returns the commands its own children produced
	// when it is resized. Returning one here is what lets a test see
	// whether the stack passed it on.
	return b, func() tea.Msg { return bounds }
}

func (boundsLayer) Draw(uv.Screen, uv.Rectangle) {}

func (b boundsLayer) UpdateKeys(msg tea.Msg) (KeyHandler, tea.Cmd) {
	updated, cmd := b.Update(msg)

	return updated.(boundsLayer), cmd
}

func (b boundsLayer) HandleKey(tea.KeyPressMsg) (KeyHandler, bool, tea.Cmd) {
	return b, false, nil
}

// TestLayerStack_keeps_one_layer_per_id pins that pushing a layer
// whose id the stack already holds removes the layer that held it, so
// only the newly pushed one is left under that id. With two, a pointer
// event could select one of them and the update that follows reach the
// other.
func TestLayerStack_keeps_one_layer_per_id(t *testing.T) {
	stack := stackOf(t,
		NewKeyLayer("same", markerLayer{marker: 'a'}, uv.Rect(0, 0, 2, 1)),
		NewKeyLayer("other", markerLayer{marker: 'c'}, uv.Rect(0, 0, 2, 1)),
		NewKeyLayer("same", markerLayer{marker: 'b'}, uv.Rect(0, 0, 2, 1)),
	)

	require.Equal(t, []string{"bb"}, stackRows(t, stack, 2, 1))

	require.Equal(t, []string{"cc"}, stackRows(t, stack.Remove("same"), 2, 1),
		"removing the id must leave no earlier layer still holding it")
}

func TestLayerStack_offers_a_declined_key_to_the_layer_below(t *testing.T) {
	var frontOffered []rune

	stack := stackOf(t,
		NewKeyLayer("back", markerLayer{marker: 'a'}, uv.Rect(0, 0, 1, 1)),
		NewKeyLayer("front", markerLayer{marker: 'b', offered: &frontOffered}, uv.Rect(0, 0, 1, 1)),
	)

	stack, handled, cmd := stack.HandleKey(tea.KeyPressMsg{Code: 'a'})

	require.True(t, handled)
	require.Nil(t, cmd)

	require.Equal(t, []rune{'a'}, frontOffered,
		"the front layer must be offered the key before the one behind it")

	back, ok := stack.Get("back")
	require.True(t, ok)
	require.Equal(t, []rune{'a'}, back.content.(markerLayer).taken)

	front, ok := stack.Get("front")
	require.True(t, ok)
	require.Empty(t, front.content.(markerLayer).taken)
}

func TestLayerStack_reports_a_key_no_layer_wants(t *testing.T) {
	stack := stackOf(t,
		NewKeyLayer("front", markerLayer{marker: 'b'}, uv.Rect(0, 0, 1, 1)),
	)

	_, handled, _ := stack.HandleKey(tea.KeyPressMsg{Code: 'z'})

	require.False(t, handled)
}

// TestLayerStack_stops_a_key_at_a_modal_layer pins what modal means: a
// key the modal layer does not want stops there, reaching neither the
// layer below nor the component under the stack.
func TestLayerStack_stops_a_key_at_a_modal_layer(t *testing.T) {
	var backOffered []rune

	stack := stackOf(t,
		NewKeyLayer("back", markerLayer{marker: 'a', offered: &backOffered}, uv.Rect(0, 0, 1, 1)),
		NewModalLayer("modal", markerLayer{marker: 'b'}, uv.Rect(0, 0, 1, 1)),
	)

	_, handled, _ := stack.HandleKey(tea.KeyPressMsg{Code: 'a'})

	require.True(t, handled, "a modal layer takes the key whether or not it wanted it")
	require.Empty(t, backOffered,
		"a modal layer stops the key before the layer behind it is asked")
}

// TestLayerStack_still_offers_keys_after_an_ordinary_update pins that
// a key layer stays one. Every path that replaces a key layer's
// content goes through KeyHandler, so an update cannot leave a
// non-modal layer that neither takes a key nor declines it.
func TestLayerStack_still_offers_keys_after_an_ordinary_update(t *testing.T) {
	var offered []rune

	stack := stackOf(t,
		NewKeyLayer("front", markerLayer{marker: 'b', offered: &offered}, uv.Rect(0, 0, 1, 1)),
	)

	stack, _ = stack.Update(BoundsMsg{Rect: uv.Rect(0, 0, 4, 4)})
	stack, _ = stack.UpdateID("front", BoundsMsg{Rect: uv.Rect(0, 0, 4, 4)})

	_, handled, _ := stack.HandleKey(tea.KeyPressMsg{Code: 'b'})

	require.True(t, handled)
	require.Equal(t, []rune{'b'}, offered)
}

// TestLayerStack_routes_a_pointer_event_front_to_back pins the mouse
// rule, which differs from a plain hit test in one way: a modal layer
// takes the event wherever the pointer is, and a layer in front of it
// still takes the clicks inside its own rectangle.
func TestLayerStack_routes_a_pointer_event_front_to_back(t *testing.T) {
	corner := markerLayer{marker: 'b'}

	withModal := stackOf(t,
		NewModalLayer("modal", markerLayer{marker: 'a'}, uv.Rect(0, 0, 2, 2)),
		NewKeyLayer("front", corner, uv.Rect(8, 8, 2, 2)),
	)

	withoutModal := stackOf(t,
		NewKeyLayer("front", corner, uv.Rect(8, 8, 2, 2)),
	)

	// The area the holder draws its stack in. The front layer sits
	// inside it; the last case shrinks it so part of that layer falls
	// outside.
	area := uv.Rect(0, 0, 16, 16)

	tests := map[string]struct {
		stack LayerStack
		at    image.Point
		area  uv.Rectangle
		want  LayerID
		hit   bool
	}{
		"inside the front layer": {
			stack: withModal, at: image.Pt(8, 8), want: "front", hit: true,
		},
		"outside every rectangle, over a modal": {
			stack: withModal, at: image.Pt(5, 5), want: "modal", hit: true,
		},
		"inside the modal's own rectangle": {
			stack: withModal, at: image.Pt(0, 0), want: "modal", hit: true,
		},
		"outside every rectangle, no modal": {
			stack: withoutModal, at: image.Pt(5, 5),
		},
		"in the part of a layer the holder left no room for": {
			stack: withoutModal, at: image.Pt(9, 9), area: uv.Rect(0, 0, 9, 9),
		},
		"outside the holder's area, over a modal": {
			stack: withModal, at: image.Pt(20, 20), area: uv.Rect(0, 0, 16, 16),
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			within := tc.area
			if within.Empty() {
				within = area
			}

			layer, ok := tc.stack.MouseLayer(tc.at, within)

			require.Equal(t, tc.hit, ok)
			require.Equal(t, tc.want, layer.ID())
		})
	}
}

func TestLayerStack_sends_a_message_to_one_layer_or_to_all_of_them(t *testing.T) {
	base := stackOf(t,
		NewKeyLayer("back", markerLayer{marker: 'a'}, uv.Rect(0, 0, 1, 1)),
		NewKeyLayer("front", markerLayer{marker: 'b'}, uv.Rect(0, 0, 1, 1)),
	)

	takenBy := func(s LayerStack, id LayerID) []rune {
		t.Helper()

		layer, ok := s.Get(id)
		require.True(t, ok)

		return layer.content.(markerLayer).taken
	}

	one, _ := base.UpdateID("back", tea.KeyPressMsg{Code: 'z'})

	require.Equal(t, []rune{'z'}, takenBy(one, "back"))
	require.Empty(t, takenBy(one, "front"))

	all, _ := base.Update(tea.KeyPressMsg{Code: 'z'})

	require.Equal(t, []rune{'z'}, takenBy(all, "back"))
	require.Equal(t, []rune{'z'}, takenBy(all, "front"))

	missing, cmd := base.UpdateID("absent", tea.KeyPressMsg{Code: 'z'})

	require.Nil(t, cmd)
	require.Empty(t, takenBy(missing, "back"))
}

func TestLayerStack_clears_under_an_opaque_layer(t *testing.T) {
	tests := map[string]struct {
		layer Layer
		want  []string
	}{
		"opaque clears what it covers": {
			layer: NewKeyLayer("l", drawsNothing{}, uv.Rect(1, 0, 2, 1)).WithOpaque(),
			want:  []string{"a  a"},
		},
		"transparent leaves it": {
			layer: NewKeyLayer("l", drawsNothing{}, uv.Rect(1, 0, 2, 1)),
			want:  []string{"aaaa"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			screen := uv.NewScreenBuffer(4, 1)
			markerLayer{marker: 'a'}.Draw(screen, screen.Bounds())

			stackOf(t, tc.layer).Draw(screen, screen.Bounds())

			var row strings.Builder
			for x := range 4 {
				row.WriteString(screen.CellAt(x, 0).Content)
			}

			require.Equal(t, tc.want, []string{row.String()})
		})
	}
}

// drawsNothing paints no cells, so the cells a test reads are the ones
// the layer's own clearing left.
type drawsNothing struct{}

func (drawsNothing) Init() tea.Cmd { return nil }

func (d drawsNothing) Update(tea.Msg) (Component, tea.Cmd) { return d, nil }

func (drawsNothing) Draw(uv.Screen, uv.Rectangle) {}

func (d drawsNothing) UpdateKeys(msg tea.Msg) (KeyHandler, tea.Cmd) {
	updated, cmd := d.Update(msg)

	return updated.(drawsNothing), cmd
}

func (d drawsNothing) HandleKey(tea.KeyPressMsg) (KeyHandler, bool, tea.Cmd) {
	return d, false, nil
}
