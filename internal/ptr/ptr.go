// Package ptr provides small helpers for pointer values.
package ptr

// CloneString returns a new pointer to the same string value.
func CloneString(value *string) *string {
	if value == nil {
		return nil
	}

	cloned := *value

	return &cloned
}

// EqualString reports whether two optional strings say the same thing:
// both absent, or both present with the same value. Comparing the
// pointers would answer "different" for two pointers to the same text,
// which is what [CloneString] produces.
func EqualString(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}

	return *a == *b
}
