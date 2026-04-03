package set

import (
	"iter"
)

// Set stores unique values with no ordering guarantees.
type Set[T comparable] map[T]struct{}

// New constructs a set from the given values.
func New[T comparable](values ...T) Set[T] {
	set := make(Set[T], len(values))
	set.Add(values...)

	return set
}

// Has reports whether the set contains the given value.
func (s Set[T]) Has(value T) bool {
	_, ok := s[value]

	return ok
}

// Add inserts any missing values into the set.
func (s *Set[T]) Add(values ...T) bool {
	if *s == nil && len(values) > 0 {
		*s = make(Set[T], len(values))
	}

	changed := false
	for _, value := range values {
		if _, ok := (*s)[value]; ok {
			continue
		}

		(*s)[value] = struct{}{}
		changed = true
	}

	return changed
}

// Remove deletes the given values from the set.
func (s *Set[T]) Remove(values ...T) bool {
	if *s == nil {
		return false
	}

	changed := false
	for _, value := range values {
		if _, ok := (*s)[value]; !ok {
			continue
		}

		delete(*s, value)
		changed = true
	}

	return changed
}

// Clone returns a shallow copy of the set.
func (s Set[T]) Clone() Set[T] {
	cloned := make(Set[T], len(s))
	for value := range s {
		cloned[value] = struct{}{}
	}

	return cloned
}

// All iterates over the set in unspecified order.
func (s Set[T]) All() iter.Seq[T] {
	return func(yield func(T) bool) {
		for value := range s {
			if !yield(value) {
				return
			}
		}
	}
}

// Except iterates over the values present in s but not in other.
func (s Set[T]) Except(other Set[T]) iter.Seq[T] {
	return func(yield func(T) bool) {
		for value := range s {
			if other.Has(value) {
				continue
			}

			if !yield(value) {
				return
			}
		}
	}
}

// Intersect iterates over the values present in both sets.
func (s Set[T]) Intersect(other Set[T]) iter.Seq[T] {
	left := s
	right := other
	if len(right) < len(left) {
		left, right = right, left
	}

	return func(yield func(T) bool) {
		for value := range left {
			if !right.Has(value) {
				continue
			}

			if !yield(value) {
				return
			}
		}
	}
}
