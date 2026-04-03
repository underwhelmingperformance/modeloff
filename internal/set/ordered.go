package set

import (
	"cmp"
	"encoding/json"
	"iter"
	"slices"
)

// Ordered stores unique values with a natural ordering.
type Ordered[T cmp.Ordered] map[T]struct{}

// NewOrdered constructs an ordered set from the given values.
func NewOrdered[T cmp.Ordered](values ...T) Ordered[T] {
	set := make(Ordered[T], len(values))
	set.Add(values...)

	return set
}

// Has reports whether the ordered set contains the given value.
func (s Ordered[T]) Has(value T) bool {
	return Set[T](s).Has(value)
}

// Add inserts any missing values into the ordered set.
func (s *Ordered[T]) Add(values ...T) bool {
	base := Set[T](*s)
	changed := (&base).Add(values...)
	*s = Ordered[T](base)

	return changed
}

// Remove deletes the given values from the ordered set.
func (s *Ordered[T]) Remove(values ...T) bool {
	base := Set[T](*s)
	changed := (&base).Remove(values...)
	*s = Ordered[T](base)

	return changed
}

// Clone returns a shallow copy of the ordered set.
func (s Ordered[T]) Clone() Ordered[T] {
	return Ordered[T](Set[T](s).Clone())
}

// All iterates over the ordered set in unspecified order.
func (s Ordered[T]) All() iter.Seq[T] {
	return Set[T](s).All()
}

// Except iterates over the values present in s but not in other.
func (s Ordered[T]) Except(other Ordered[T]) iter.Seq[T] {
	return Set[T](s).Except(Set[T](other))
}

// Intersect iterates over the values present in both ordered sets.
func (s Ordered[T]) Intersect(other Ordered[T]) iter.Seq[T] {
	return Set[T](s).Intersect(Set[T](other))
}

// Sorted iterates over the set in a stable order.
func (s Ordered[T]) Sorted() iter.Seq[T] {
	return SortOrdered(s.All())
}

// SortSeq iterates over the values of seq in sorted order.
func SortSeq[T any](seq iter.Seq[T], cmp func(T, T) int) iter.Seq[T] {
	values := slices.SortedFunc(seq, cmp)

	return func(yield func(T) bool) {
		for _, value := range values {
			if !yield(value) {
				return
			}
		}
	}
}

// SortOrdered iterates over the values of seq in sorted order.
func SortOrdered[T cmp.Ordered](seq iter.Seq[T]) iter.Seq[T] {
	values := slices.Sorted(seq)

	return func(yield func(T) bool) {
		for _, value := range values {
			if !yield(value) {
				return
			}
		}
	}
}

// MarshalJSON encodes the ordered set as a JSON array.
func (s Ordered[T]) MarshalJSON() ([]byte, error) {
	values := slices.Collect(s.Sorted())
	if values == nil {
		values = []T{}
	}

	return json.Marshal(values)
}

// UnmarshalJSON decodes a JSON array into a deduplicated ordered set.
func (s *Ordered[T]) UnmarshalJSON(data []byte) error {
	var values []T
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}

	*s = nil
	s.Add(values...)

	return nil
}
