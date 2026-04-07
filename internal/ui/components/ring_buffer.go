package components

// RingBuffer is a fixed-capacity circular buffer. When full, new
// items overwrite the oldest. Items are yielded in insertion order
// (oldest first).
type RingBuffer[T any] struct {
	items []T
	head  int
	count int
}

// NewRingBuffer creates a ring buffer with the given capacity.
func NewRingBuffer[T any](capacity int) *RingBuffer[T] {
	if capacity < 0 {
		capacity = 0
	}

	return &RingBuffer[T]{
		items: make([]T, capacity),
	}
}

// Append adds an item to the buffer. If full, the oldest item is
// dropped.
func (r *RingBuffer[T]) Append(item T) {
	if len(r.items) == 0 {
		return
	}

	tail := (r.head + r.count) % len(r.items)
	r.items[tail] = item

	if r.count == len(r.items) {
		r.head = (r.head + 1) % len(r.items)
	} else {
		r.count++
	}
}

// Len returns the number of items in the buffer.
func (r *RingBuffer[T]) Len() int {
	if r == nil {
		return 0
	}

	return r.count
}

// Cap returns the capacity of the buffer.
func (r *RingBuffer[T]) Cap() int {
	if r == nil {
		return 0
	}

	return len(r.items)
}

// GetAt returns the item at the given position (0 = oldest).
func (r *RingBuffer[T]) GetAt(index int) (T, bool) {
	var zero T

	if r == nil || index < 0 || index >= r.count {
		return zero, false
	}

	return r.items[(r.head+index)%len(r.items)], true
}

// Clear removes all items from the buffer without changing capacity.
func (r *RingBuffer[T]) Clear() {
	r.head = 0
	r.count = 0
}

// Resize changes the capacity, preserving as many recent items as
// fit. If the new capacity is smaller, the oldest items are dropped.
func (r *RingBuffer[T]) Resize(capacity int) {
	if capacity < 0 {
		capacity = 0
	}

	old := r.Slice()

	r.items = make([]T, capacity)
	r.head = 0
	r.count = 0

	// Keep the most recent items that fit.
	start := max(len(old)-capacity, 0)

	for _, item := range old[start:] {
		r.Append(item)
	}
}

// Slice returns all items in order (oldest first) as a new slice.
func (r *RingBuffer[T]) Slice() []T {
	if r == nil || r.count == 0 {
		return nil
	}

	out := make([]T, r.count)

	for i := range r.count {
		out[i] = r.items[(r.head+i)%len(r.items)]
	}

	return out
}
