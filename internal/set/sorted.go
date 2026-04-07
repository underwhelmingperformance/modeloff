package set

import (
	"iter"

	"github.com/tidwall/btree"
)

// Sorted is an ordered set backed by a B-tree. Elements are kept in
// the order defined by the less function provided at construction
// time. Insert, remove, and positional access are all O(log n).
type Sorted[T any] struct {
	tree *btree.BTreeG[T]
}

// NewSorted creates a sorted set using the given comparison function.
func NewSorted[T any](less func(a, b T) bool) *Sorted[T] {
	return &Sorted[T]{
		tree: btree.NewBTreeGOptions(less, btree.Options{
			NoLocks: true,
		}),
	}
}

// Insert adds an element to the set. If an equal element already
// exists it is replaced. Returns true if the element was new.
func (s *Sorted[T]) Insert(item T) bool {
	_, replaced := s.tree.Set(item)

	return !replaced
}

// Remove deletes the element equal to item. Returns true if it was
// present.
func (s *Sorted[T]) Remove(item T) bool {
	_, removed := s.tree.Delete(item)

	return removed
}

// Has reports whether the set contains an element equal to item.
func (s *Sorted[T]) Has(item T) bool {
	_, ok := s.tree.Get(item)

	return ok
}

// Get retrieves the element equal to item according to the
// comparator. Returns the stored element and true if found.
func (s *Sorted[T]) Get(item T) (T, bool) {
	if s == nil {
		var zero T
		return zero, false
	}

	return s.tree.Get(item)
}

// Len returns the number of elements.
func (s *Sorted[T]) Len() int {
	if s == nil {
		return 0
	}

	return s.tree.Len()
}

// GetAt returns the element at the given position in sorted order.
func (s *Sorted[T]) GetAt(index int) (T, bool) {
	if s == nil {
		var zero T
		return zero, false
	}

	return s.tree.GetAt(index)
}

// All iterates over every element in sorted order.
func (s *Sorted[T]) All() iter.Seq[T] {
	return func(yield func(T) bool) {
		if s == nil {
			return
		}

		s.tree.Scan(func(item T) bool {
			return yield(item)
		})
	}
}

// Indexed iterates over every element in sorted order, yielding
// the positional index alongside each element.
func (s *Sorted[T]) Indexed() iter.Seq2[int, T] {
	return func(yield func(int, T) bool) {
		if s == nil {
			return
		}

		idx := 0

		s.tree.Scan(func(item T) bool {
			if !yield(idx, item) {
				return false
			}

			idx++

			return true
		})
	}
}

// Items returns all elements as a slice in sorted order.
func (s *Sorted[T]) Items() []T {
	if s == nil {
		return nil
	}

	return s.tree.Items()
}
