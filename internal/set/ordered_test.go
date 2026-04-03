package set

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOrdered_MarshalJSONUsesArrayEncoding(t *testing.T) {
	data, err := json.Marshal(NewOrdered("alice", "bob"))

	require.NoError(t, err)
	require.JSONEq(t, `["alice","bob"]`, string(data))
}

func TestOrdered_UnmarshalJSONDeduplicatesValues(t *testing.T) {
	var set Ordered[string]

	err := json.Unmarshal([]byte(`["alice","bob","alice"]`), &set)

	require.NoError(t, err)
	require.Equal(t, NewOrdered("alice", "bob"), set)
}

func TestOrdered_SortedReturnsStableOrder(t *testing.T) {
	set := NewOrdered("carol", "alice", "bob")

	var values []string
	for value := range set.Sorted() {
		values = append(values, value)
	}

	require.Equal(t, []string{"alice", "bob", "carol"}, values)
}

func TestSortSeq(t *testing.T) {
	values := slices.Collect(SortSeq(New("carol", "alice", "bob").All(), func(left, right string) int {
		switch {
		case left < right:
			return -1
		case left > right:
			return 1
		default:
			return 0
		}
	}))

	require.Equal(t, []string{"alice", "bob", "carol"}, values)
}
