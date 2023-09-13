package ds

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOrderedMap(t *testing.T) {
	om := NewOrderedMap[string, int]()
	require.Equal(t, 0, om.Len())
	_, ok := om.Get("a")
	require.False(t, ok)
	require.False(t, om.Has("a"))
	om.Put("a", 1)
	require.True(t, om.Has("a"))
	require.Equal(t, 1, om.Len())
	val, ok := om.Get("a")
	require.Equal(t, 1, val)
	require.True(t, ok)
	require.Equal(t, 1, om.Len())
	om.Put("a", 2)
	val, ok = om.Get("a")
	require.Equal(t, 2, val)
	require.True(t, ok)
	require.Equal(t, 1, om.Len())
	om.Put("b", 3)
	val, ok = om.Get("b")
	require.Equal(t, 3, val)
	require.True(t, ok)
	require.Equal(t, 2, om.Len())

	require.Equal(t, []int{2, 3}, om.Values())
	require.Equal(t, []string{"a", "b"}, om.Keys())

	om.Delete("b")
	require.Equal(t, []int{2}, om.Values())
	require.Equal(t, []string{"a"}, om.Keys())

	// delete unknown key
	om.Delete("c")
}
