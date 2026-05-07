package set

import (
	"iter"
	"slices"
)

// Lesser is the constraint for elements of a [Sorted]. Putting
// the comparator on the element type rather than storing it as a
// closure on the set keeps `reflect.DeepEqual` honest on
// [Sorted] values: Go treats two non-nil function values as
// never deeply equal, so a stored less-func would defeat
// structural comparison even when contents match.
type Lesser[T any] interface {
	Less(other T) bool
}

// Sorted is an ordered set whose elements are kept in the order
// defined by `T.Less`. Backed by a sorted slice; all operations
// are O(log n) lookup with O(n) shift on mutation.
type Sorted[T Lesser[T]] struct {
	items []T
}

// NewSorted creates an empty sorted set.
func NewSorted[T Lesser[T]]() *Sorted[T] {
	return &Sorted[T]{}
}

// find returns the index where `item` belongs and whether an
// equal element is already present.
func (s *Sorted[T]) find(item T) (int, bool) {
	return slices.BinarySearchFunc(s.items, item, cmp[T])
}

func cmp[T Lesser[T]](a, b T) int {
	if a.Less(b) {
		return -1
	}

	if b.Less(a) {
		return 1
	}

	return 0
}

// Insert adds an element to the set, replacing any equal
// element already present. Returns true if the element was new.
func (s *Sorted[T]) Insert(item T) bool {
	i, found := s.find(item)
	if found {
		s.items[i] = item
		return false
	}

	s.items = slices.Insert(s.items, i, item)
	return true
}

// Remove deletes the element equal to `item`. Returns true if
// it was present.
func (s *Sorted[T]) Remove(item T) bool {
	i, found := s.find(item)
	if !found {
		return false
	}

	s.items = slices.Delete(s.items, i, i+1)
	return true
}

// Has reports whether the set contains an element equal to
// `item`.
func (s *Sorted[T]) Has(item T) bool {
	if s == nil {
		return false
	}

	_, found := s.find(item)
	return found
}

// Get retrieves the element equal to `item` according to the
// comparator. Returns the stored element and true if found.
func (s *Sorted[T]) Get(item T) (T, bool) {
	if s == nil {
		var zero T
		return zero, false
	}

	i, found := s.find(item)
	if !found {
		var zero T
		return zero, false
	}

	return s.items[i], true
}

// Len returns the number of elements.
func (s *Sorted[T]) Len() int {
	if s == nil {
		return 0
	}

	return len(s.items)
}

// GetAt returns the element at the given position in sorted
// order.
func (s *Sorted[T]) GetAt(index int) (T, bool) {
	if s == nil || index < 0 || index >= len(s.items) {
		var zero T
		return zero, false
	}

	return s.items[index], true
}

// All iterates over every element in sorted order.
func (s *Sorted[T]) All() iter.Seq[T] {
	return func(yield func(T) bool) {
		if s == nil {
			return
		}

		for _, item := range s.items {
			if !yield(item) {
				return
			}
		}
	}
}

// Indexed iterates over every element in sorted order, yielding
// the positional index alongside each element.
func (s *Sorted[T]) Indexed() iter.Seq2[int, T] {
	return func(yield func(int, T) bool) {
		if s == nil {
			return
		}

		for i, item := range s.items {
			if !yield(i, item) {
				return
			}
		}
	}
}

// Items returns all elements as a slice in sorted order.
func (s *Sorted[T]) Items() []T {
	if s == nil {
		return nil
	}

	return slices.Clone(s.items)
}
