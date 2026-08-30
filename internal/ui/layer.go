package ui

import (
	"image"
	"slices"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	uvscreen "github.com/charmbracelet/ultraviolet/screen"
)

// KeyHandler is implemented by a layer that is offered a key and may
// decline it. Declining returns false, and the key passes on to
// whatever sits below.
//
// Returning false means the handler did nothing about the key. The
// stack therefore discards the handler and the command returned with
// it, and offers the key to the next layer with the value it already
// held. A handler that wants to act on a key must take it.
//
// Both methods return a KeyHandler. [Component.Update] returns a
// Component instead, so a stack that took a Component back from either
// call would have to type-assert the replacement each time and would
// silently stop offering it keys once an assertion failed. Narrowing
// the return type keeps a layer able to handle keys for as long as it
// exists.
//
// UpdateKeys does what Update does and narrows its result, which an
// implementer can do because it knows its own type:
//
//	func (x X) UpdateKeys(msg tea.Msg) (KeyHandler, tea.Cmd) {
//		updated, cmd := x.Update(msg)
//
//		return updated.(X), cmd
//	}
type KeyHandler interface {
	Component

	HandleKey(msg tea.KeyPressMsg) (KeyHandler, bool, tea.Cmd)

	UpdateKeys(msg tea.Msg) (KeyHandler, tea.Cmd)
}

// LayerID names a layer within one stack.
type LayerID string

// Layer is a component drawn over its parent's own content, at a
// rectangle the parent chooses.
//
// A layer is either modal, taking every key and pointer event that
// reaches it, or it can decline a key. [NewKeyLayer] requires a
// [KeyHandler], so a non-modal layer always has one; [NewModalLayer]
// takes any component, because a modal never has to decline.
//
// A layer that handles keys stores its content in both fields, and
// every path that replaces the content goes through [KeyHandler],
// whose methods return one. The two therefore cannot come to disagree
// about whether this layer handles keys.
type Layer struct {
	id      LayerID
	content Component
	keys    KeyHandler
	rect    uv.Rectangle
	modal   bool
	opaque  bool
	dismiss []KeyBinding
}

// NewKeyLayer returns a layer that is offered each key and passes on
// the ones it does not want.
func NewKeyLayer(id LayerID, handler KeyHandler, rect uv.Rectangle) Layer {
	return Layer{id: id, content: handler, keys: handler, rect: rect}
}

// NewModalLayer returns a layer that takes every key and pointer event
// reaching it. A component that can handle keys is still offered them
// first; the rest stop here.
func NewModalLayer(id LayerID, content Component, rect uv.Rectangle) Layer {
	keys, _ := content.(KeyHandler)

	return Layer{id: id, content: content, keys: keys, rect: rect, modal: true}
}

// DismissedBy returns a copy the stack removes when one of these keys
// reaches it. A modal layer takes every key it is offered, so its
// holder never sees the key that should dismiss it.
// [LayerStack.HandleKey] checks these bindings before it calls the
// layer's own handler, which leaves the holder nothing to match before
// it offers a key to the stack.
func (l Layer) DismissedBy(bindings ...KeyBinding) Layer {
	l.dismiss = bindings

	return l
}

// WithOpaque returns a copy whose rectangle is cleared before its
// content draws, so nothing underneath shows through the gaps.
func (l Layer) WithOpaque() Layer {
	l.opaque = true

	return l
}

// ID returns the layer's identifier.
func (l Layer) ID() LayerID {
	return l.id
}

// LayerStack holds the layers over one component's content, with the
// front layer last in the slice. Draw order is the z-order, so the
// front layer draws over the ones before it and is offered a key
// before them.
//
// One id names at most one layer. [LayerStack.Push] removes any layer
// already holding the id before it appends the new one, so every
// method that looks an id up has one layer to find, and the
// front-to-back search a key or a pointer event follows agrees with
// them.
//
// Every method that changes the stack returns a new one and copies the
// slice before writing, because a component holding a stack is copied
// by value whenever its parent updates it. Writing through the shared
// backing array would change a stack somebody else is still holding.
type LayerStack struct {
	layers []Layer
}

// Push removes any layer already holding this layer's id and appends
// this one at the front. The layer is attached: it is sent the
// [BoundsMsg] naming its rectangle, and the returned command batches
// the command [Component.Init] returned with the one the component
// returned from that update. A layer pushed part way through a session
// is attached the same way a component in the tree is at startup, so
// its Init runs at that point.
func (s LayerStack) Push(layer Layer) (LayerStack, tea.Cmd) {
	init := layer.content.Init()

	placed, cmd := layer.update(BoundsMsg{Rect: layer.rect})

	s.layers = append(slices.Clone(s.Remove(layer.id).layers), placed)

	return s, tea.Batch(init, cmd)
}

// Remove drops the layer with this id, and returns the stack unchanged
// when it holds no such layer.
func (s LayerStack) Remove(id LayerID) LayerStack {
	at := slices.IndexFunc(s.layers, func(l Layer) bool { return l.id == id })
	if at < 0 {
		return s
	}

	return s.removeAt(at)
}

// removeAt drops the layer at this index.
func (s LayerStack) removeAt(at int) LayerStack {
	s.layers = slices.Delete(slices.Clone(s.layers), at, at+1)

	return s
}

// Get returns the layer with this id.
func (s LayerStack) Get(id LayerID) (Layer, bool) {
	at := slices.IndexFunc(s.layers, func(l Layer) bool { return l.id == id })
	if at < 0 {
		return Layer{}, false
	}

	return s.layers[at], true
}

// HasModal reports whether any layer takes every key reaching it.
func (s LayerStack) HasModal() bool {
	return slices.ContainsFunc(s.layers, func(l Layer) bool { return l.modal })
}

// HandleKey offers the key to each layer from the front, and reports
// whether anything took it. A modal layer ends the search whether or
// not it wanted the key, which is what makes it modal.
func (s LayerStack) HandleKey(msg tea.KeyPressMsg) (LayerStack, bool, tea.Cmd) {
	for i, layer := range slices.Backward(s.layers) {
		if Matches(msg, layer.dismiss...) {
			return s.removeAt(i), true, nil
		}

		if layer.keys != nil {
			updated, handled, cmd := layer.keys.HandleKey(msg)
			if handled {
				return s.replaceAt(i, updated), true, cmd
			}
		}

		if layer.modal {
			return s, true, nil
		}
	}

	return s, false, nil
}

// MouseLayer returns the layer that should receive a pointer event at
// this point. The area is the one the holder draws its stack in, and no
// layer receives an event outside it, so a click beyond the holder
// reaches whatever component is drawn there. A click on the sidebar
// therefore switches window while a channel window's stack holds a
// modal.
//
// Inside that area a modal layer receives the event wherever the
// pointer is, which stops a click reaching what the modal covers, and a
// layer in front of it still receives the clicks inside its own
// rectangle. A layer is matched against as much of its rectangle as
// falls inside the area, which is the part Draw clips it to, so a layer
// never receives a click where the holder gave it no room to appear.
func (s LayerStack) MouseLayer(pt image.Point, area uv.Rectangle) (Layer, bool) {
	if !pt.In(area) {
		return Layer{}, false
	}

	for _, layer := range slices.Backward(s.layers) {
		if layer.modal || pt.In(layer.rect.Intersect(area)) {
			return layer, true
		}
	}

	return Layer{}, false
}

// Update sends a message to every layer.
func (s LayerStack) Update(msg tea.Msg) (LayerStack, tea.Cmd) {
	if len(s.layers) == 0 {
		return s, nil
	}

	layers := slices.Clone(s.layers)

	cmds := make([]tea.Cmd, 0, len(layers))
	for i, layer := range layers {
		updated, cmd := layer.update(msg)
		layers[i] = updated

		cmds = append(cmds, cmd)
	}

	s.layers = layers

	return s, tea.Batch(cmds...)
}

// UpdateID sends a message to one layer, and does nothing when the
// stack holds no layer with that id.
func (s LayerStack) UpdateID(id LayerID, msg tea.Msg) (LayerStack, tea.Cmd) {
	at := slices.IndexFunc(s.layers, func(l Layer) bool { return l.id == id })
	if at < 0 {
		return s, nil
	}

	updated, cmd := s.layers[at].update(msg)

	layers := slices.Clone(s.layers)
	layers[at] = updated
	s.layers = layers

	return s, cmd
}

// Resize moves every layer to a new rectangle. It suits a holder whose
// overlays all cover the same area, which is what Root's do; a holder
// that positions each layer separately uses [LayerStack.Move].
func (s LayerStack) Resize(rect uv.Rectangle) (LayerStack, tea.Cmd) {
	if len(s.layers) == 0 {
		return s, nil
	}

	layers := slices.Clone(s.layers)

	cmds := make([]tea.Cmd, 0, len(layers))
	for i := range layers {
		layers[i].rect = rect

		updated, cmd := layers[i].update(BoundsMsg{Rect: rect})
		layers[i] = updated

		cmds = append(cmds, cmd)
	}

	s.layers = layers

	return s, tea.Batch(cmds...)
}

// Move puts one layer at a new rectangle, keeping its component and
// that component's state. A holder that positions a layer against part
// of its own content recomputes the rectangle as its layout changes.
// A stack holding no layer with that id is returned unchanged.
//
// The layer receives the new rectangle as the [BoundsMsg] every
// component stores its geometry from. Every method that places a layer
// sends it, because a layer that was not sent one would draw where the
// stack put it and hit-test against the rectangle it still holds.
func (s LayerStack) Move(id LayerID, rect uv.Rectangle) (LayerStack, tea.Cmd) {
	at := slices.IndexFunc(s.layers, func(l Layer) bool { return l.id == id })
	if at < 0 {
		return s, nil
	}

	layers := slices.Clone(s.layers)
	layers[at].rect = rect

	updated, cmd := layers[at].update(BoundsMsg{Rect: rect})
	layers[at] = updated

	s.layers = layers

	return s, cmd
}

// Draw renders the layers back to front, so the front layer covers the
// ones before it.
func (s LayerStack) Draw(screen uv.Screen, area uv.Rectangle) {
	for _, layer := range s.layers {
		rect := layer.rect.Intersect(area)
		if rect.Empty() {
			continue
		}

		if layer.opaque {
			uvscreen.ClearArea(screen, rect)
		}

		layer.content.Draw(screen, rect)
	}
}

// update sends a message to the layer's content. A layer that handles
// keys is updated through [KeyHandler.UpdateKeys], so it is still one
// afterwards.
func (l Layer) update(msg tea.Msg) (Layer, tea.Cmd) {
	if l.keys != nil {
		updated, cmd := l.keys.UpdateKeys(msg)
		l.keys, l.content = updated, updated

		return l, cmd
	}

	updated, cmd := l.content.Update(msg)
	l.content = updated

	return l, cmd
}

// replaceAt returns a copy of the stack whose layer at this index uses
// the given key handler.
func (s LayerStack) replaceAt(at int, handler KeyHandler) LayerStack {
	layers := slices.Clone(s.layers)
	layers[at].keys = handler
	layers[at].content = handler

	s.layers = layers

	return s
}
