package oracle

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFencePinsAndReportsChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fence.json")

	f, err := NewFence(path, true)
	require.NoError(t, err)
	diff, err := f.Check("a", map[string]int{"n": 1})
	require.NoError(t, err)
	assert.Empty(t, diff)
	_, err = f.Check("b", "x")
	require.NoError(t, err)
	unmeasured, err := f.Close()
	require.NoError(t, err)
	assert.Empty(t, unmeasured)

	f, err = NewFence(path, false)
	require.NoError(t, err)
	diff, err = f.Check("a", map[string]int{"n": 1})
	require.NoError(t, err)
	assert.Empty(t, diff, "the pinned value")

	diff, err = f.Check("c", 1)
	require.NoError(t, err)
	assert.Contains(t, diff, "no value pinned")

	unmeasured, err = f.Close()
	require.NoError(t, err)
	assert.Equal(t, []string{"b"}, unmeasured)

	f, err = NewFence(path, false)
	require.NoError(t, err)
	diff, err = f.Check("a", map[string]int{"n": 2})
	require.NoError(t, err)
	assert.Equal(t, `a: pinned {"n":1}, measured {"n":2}`, diff)
}

// A -run filter measures only some names, and updating from that run must
// keep the pins of the others.
func TestFenceUpdateKeepsThePinsItDoesNotMeasure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fence.json")

	f, err := NewFence(path, true)
	require.NoError(t, err)
	for _, name := range []string{"a", "b"} {
		_, err = f.Check(name, 1)
		require.NoError(t, err)
	}
	_, err = f.Close()
	require.NoError(t, err)

	f, err = NewFence(path, true)
	require.NoError(t, err)
	_, err = f.Check("a", 2)
	require.NoError(t, err)
	unmeasured, err := f.Close()
	require.NoError(t, err)
	assert.Equal(t, []string{"b"}, unmeasured)

	f, err = NewFence(path, false)
	require.NoError(t, err)
	diff, err := f.Check("a", 2)
	require.NoError(t, err)
	assert.Empty(t, diff, "the value measured")
	diff, err = f.Check("b", 1)
	require.NoError(t, err)
	assert.Empty(t, diff, "the pin the update did not measure")
}

func TestFenceWithoutAFilePinsNothing(t *testing.T) {
	f, err := NewFence(filepath.Join(t.TempDir(), "missing.json"), false)
	require.NoError(t, err)

	unmeasured, err := f.Close()
	require.NoError(t, err)
	assert.Empty(t, unmeasured)
}
