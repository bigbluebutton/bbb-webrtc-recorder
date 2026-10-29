package events

import (
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// The host's CLOCK_MONOTONIC at one time.Now(). Go keeps a monotonic reading
// in every time.Now() but does not expose its value, so MonotonicMs counts
// from this pair.
var monotonicBase = sync.OnceValues(func() (time.Time, time.Duration) {
	var ts unix.Timespec

	before := time.Now()
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		// mediaStartHR and mediaEndHR are then wrong. The SFU checks each
		// against the UTC time sent with it, and derives it from that UTC
		// time when they disagree.
		log.WithError(err).Error("Cannot read CLOCK_MONOTONIC")

		return before, 0
	}
	after := time.Now()

	return before.Add(after.Sub(before) / 2), time.Duration(ts.Nano())
})

// MonotonicMs returns t on the host's CLOCK_MONOTONIC, in milliseconds. That
// is the clock of the timestamp field in BigBlueButton's recording events:
// akka-apps and bbb-webrtc-sfu read it too, so record-and-playback can
// compare their times. t must carry a monotonic reading, as from time.Now().
func MonotonicMs(t time.Time) int64 {
	at, mono := monotonicBase()

	return (mono + t.Sub(at)).Milliseconds()
}
