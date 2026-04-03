package ui

// Rect describes a rectangular area in absolute screen coordinates.
type Rect struct {
	X      int
	Y      int
	Width  int
	Height int
}

// Contains reports whether the given point lies within the rectangle.
func (r Rect) Contains(x, y int) bool {
	return x >= r.X && y >= r.Y && x < r.X+r.Width && y < r.Y+r.Height
}

// Local converts an absolute point into rectangle-local coordinates.
func (r Rect) Local(x, y int) (int, int) {
	return x - r.X, y - r.Y
}

// BoundsMsg tells a child model the absolute bounds it occupies.
type BoundsMsg struct {
	Rect Rect
}
