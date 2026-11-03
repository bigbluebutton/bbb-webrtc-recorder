package oracle

import (
	"fmt"
	"math"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
)

// Lag compares where a track's frames play in the file with when they were
// read. A frame's lag is its PTS since the first block, less its read time
// since the first frame's. It is negative when the file plays the frame
// earlier than it arrived, as when the sender's media clock runs slow.
//
// Frames read within 5 ms of each other came in a burst, as after a stall.
// A frame in a burst takes the next frame's read time less the timestamp
// step between them instead, when that is earlier and the step is at most
// two 20 ms frames. A wider step, such as a realignment or a DTX gap, does
// not show when the frame before it was sent. A frame outside a burst keeps
// its read time, so timestamps that run fast still show as lag.
type Lag struct {
	Frames  int
	MaxMs   float64 // the largest lag by magnitude, with its sign
	FinalMs float64 // the last frame's
	// StartMs is the backlog the first read found: how far a burst of reads
	// at the start moved the first frame's read back. StartAtUs is that
	// read, in µs on the trace's atUs timeline.
	StartMs   float64
	StartAtUs float64
}

// TimelineLag maps the blocks of t, in file order, to the frames read in
// tr: their distinct RTP timestamps in order, each at its first read. It
// needs one block per frame read, so it fails on a lost or dropped frame.
func TimelineLag(t *Track, tr *corpus.Trace) (Lag, error) {
	frames := framesRead(tr)
	if len(frames) == 0 || len(frames) != t.Blocks {
		return Lag{}, fmt.Errorf("%d frames read, %d blocks: the lag needs one block per frame", len(frames), t.Blocks)
	}

	const burstUs = 5000
	inBurst := make([]bool, len(frames))
	for k := 1; k < len(frames); k++ {
		if abs(frames[k].at-frames[k-1].at) <= burstUs {
			inBurst[k-1], inBurst[k] = true, true
		}
	}

	firstRead := frames[0].at
	usPerTick := 1e6 / float64(tr.Header.ClockRate)
	maxStep := int64(float64(tr.Header.ClockRate) * 0.04)
	for k := len(frames) - 2; k >= 0; k-- {
		if step := frames[k+1].ext - frames[k].ext; inBurst[k] && step <= maxStep {
			frames[k].at = min(frames[k].at, frames[k+1].at-int64(float64(step)*usPerTick))
		}
	}

	lag := Lag{Frames: len(frames), StartMs: float64(firstRead-frames[0].at) / 1000, StartAtUs: float64(frames[0].at)}
	for k, f := range frames {
		played := float64(t.PTS[k] - t.PTS[0])
		read := float64(time.Duration(f.at-frames[0].at)*time.Microsecond) / float64(time.Millisecond)
		l := played - read

		if math.Abs(l) > math.Abs(lag.MaxMs) {
			lag.MaxMs = l
		}
		lag.FinalMs = l
	}

	return lag, nil
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}

	return v
}
