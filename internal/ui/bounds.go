package ui

import uv "github.com/charmbracelet/ultraviolet"

// BoundsMsg tells a component the absolute bounds its parent assigned.
type BoundsMsg struct {
	Rect uv.Rectangle
}
