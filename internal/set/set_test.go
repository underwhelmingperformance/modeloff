package set

import "testing"

import "github.com/stretchr/testify/require"

func TestSet_AddKeepsUniqueness(t *testing.T) {
	set := New("alice", "bob")

	changed := set.Add("bob", "carol")

	require.True(t, changed)
	require.Equal(t, New("alice", "bob", "carol"), set)
}

func TestSet_RemoveDeletesValues(t *testing.T) {
	set := New("alice", "bob", "carol")

	changed := set.Remove("bob", "nobody")

	require.True(t, changed)
	require.Equal(t, New("alice", "carol"), set)
}

func TestSet_Difference(t *testing.T) {
	set := New("alice", "bob", "carol")

	var values []string
	for value := range set.Except(New("bob")) {
		values = append(values, value)
	}

	require.ElementsMatch(t, []string{"alice", "carol"}, values)
}

func TestSet_Intersection(t *testing.T) {
	left := New("alice", "bob", "carol")
	right := New("bob", "dave")

	var values []string
	for value := range left.Intersect(right) {
		values = append(values, value)
	}

	require.Equal(t, []string{"bob"}, values)
}
