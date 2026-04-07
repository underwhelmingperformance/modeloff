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
