package events

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// CLOCK_MONOTONIC, read between two time.Now() calls, lies between
// MonotonicMs of each. The 1 ms slack covers the truncation to ms and the
// time monotonicBase takes to read the clock.
func TestMonotonicMsReadsTheHostMonotonicClock(t *testing.T) {
	var ts unix.Timespec

	before := time.Now()
	require.NoError(t, unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts))
	after := time.Now()

	host := time.Duration(ts.Nano()).Milliseconds()

	assert.LessOrEqual(t, MonotonicMs(before)-1, host)
	assert.LessOrEqual(t, host, MonotonicMs(after)+1)
}

func TestMonotonicMsFollowsTheMonotonicReading(t *testing.T) {
	t0 := time.Now()
	t1 := t0.Add(1500 * time.Millisecond)

	assert.Equal(t, int64(1500), MonotonicMs(t1)-MonotonicMs(t0))
}
