package components_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui/components"
)

func TestRingBuffer_Append_within_capacity(t *testing.T) {
	r := components.NewRingBuffer[int](5)
	r.Append(1)
	r.Append(2)
	r.Append(3)

	require.Equal(t, []int{1, 2, 3}, r.Slice())
}

func TestRingBuffer_Append_overflow_drops_oldest(t *testing.T) {
	r := components.NewRingBuffer[int](3)
	r.Append(1)
	r.Append(2)
	r.Append(3)
	r.Append(4)
	r.Append(5)

	require.Equal(t, []int{3, 4, 5}, r.Slice())
}

func TestRingBuffer_GetAt(t *testing.T) {
	r := components.NewRingBuffer[string](3)
	r.Append("a")
	r.Append("b")
	r.Append("c")
	r.Append("d") // drops "a"

	got, ok := r.GetAt(0)
	require.True(t, ok)
	require.Equal(t, "b", got)

	got, ok = r.GetAt(2)
	require.True(t, ok)
	require.Equal(t, "d", got)

	_, ok = r.GetAt(3)
	require.False(t, ok)

	_, ok = r.GetAt(-1)
	require.False(t, ok)
}

func TestRingBuffer_Clear(t *testing.T) {
	r := components.NewRingBuffer[int](3)
	r.Append(1)
	r.Append(2)
	r.Clear()

	require.Nil(t, r.Slice())
}

func TestRingBuffer_Resize_grow(t *testing.T) {
	r := components.NewRingBuffer[int](3)
	r.Append(1)
	r.Append(2)
	r.Append(3)
	r.Resize(5)

	require.Equal(t, 5, r.Cap())
	require.Equal(t, []int{1, 2, 3}, r.Slice())
}

func TestRingBuffer_Resize_shrink_keeps_newest(t *testing.T) {
	r := components.NewRingBuffer[int](5)
	r.Append(1)
	r.Append(2)
	r.Append(3)
	r.Append(4)
	r.Append(5)
	r.Resize(3)

	require.Equal(t, 3, r.Cap())
	require.Equal(t, []int{3, 4, 5}, r.Slice())
}

func TestRingBuffer_zero_capacity(t *testing.T) {
	r := components.NewRingBuffer[int](0)
	r.Append(1)

	require.Nil(t, r.Slice())
}

func TestRingBuffer_nil_is_safe(t *testing.T) {
	var r *components.RingBuffer[int]

	require.Equal(t, 0, r.Len())
	require.Equal(t, 0, r.Cap())
	require.Nil(t, r.Slice())

	_, ok := r.GetAt(0)
	require.False(t, ok)
}

func TestRingBuffer_Slice_after_wrap(t *testing.T) {
	r := components.NewRingBuffer[int](3)
	for i := range 10 {
		r.Append(i)
	}

	require.Equal(t, []int{7, 8, 9}, r.Slice())
}
