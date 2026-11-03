package recorder

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const audioTicks = opusSampleRate / 50 // a 20 ms Opus frame

type spanFrame struct {
	ts       uint32
	pts      time.Duration
	arrivals []time.Duration // since t0
}

func runSpan(t0 time.Time, r spanRules, frames []spanFrame) *mediaSpan {
	var s mediaSpan
	var prev time.Duration

	for i, f := range frames {
		for _, a := range f.arrivals {
			s.arrived(f.ts, t0.Add(a))
		}

		step := f.pts - prev
		if i == 0 {
			step = 0
		}
		prev = f.pts

		s.wrote(f.ts, f.pts, step, r)
	}

	return &s
}

// audioFrames are n 20 ms frames from pts 0, frame k read at at(k).
func audioFrames(n int, at func(k int) time.Duration) []spanFrame {
	frames := make([]spanFrame, n)
	for k := range frames {
		frames[k] = spanFrame{
			ts:       uint32(1000 + k*audioTicks),
			pts:      time.Duration(k) * 20 * time.Millisecond,
			arrivals: []time.Duration{at(k)},
		}
	}

	return frames
}

// cameraFrames are 90 kHz frames at pts, in ms, frame k read late(k) after
// its PTS.
func cameraFrames(pts []int, late func(k int) time.Duration) []spanFrame {
	frames := make([]spanFrame, len(pts))
	for k, p := range pts {
		at := time.Duration(p) * time.Millisecond
		frames[k] = spanFrame{ts: uint32(p * 90), pts: at, arrivals: []time.Duration{at + late(k)}}
	}

	return frames
}

// ptsFrom returns the PTS from, from+step, ... up to to, in ms.
func ptsFrom(from, step, to int) []int {
	var pts []int
	for p := from; p <= to; p += step {
		pts = append(pts, p)
	}

	return pts
}

// lateFirst reads the first frames late by ms each, and the rest 2 ms late.
func lateFirst(ms ...int) func(k int) time.Duration {
	return func(k int) time.Duration {
		if k < len(ms) {
			return time.Duration(ms[k]) * time.Millisecond
		}

		return 2 * time.Millisecond
	}
}

func TestMediaSpanIsEmptyUntilAFrameIsWritten(t *testing.T) {
	var s mediaSpan
	s.arrived(1000, time.Now())

	_, ok := s.span()
	assert.False(t, ok)
}

// A frame's packets can arrive out of order, e.g. its first one repaired by
// a retransmission: the frame arrived with its earliest packet.
func TestMediaSpanStartsAtTheFirstFramesEarliestPacket(t *testing.T) {
	t0 := time.Now()
	s := runSpan(t0, videoSpan, []spanFrame{
		{ts: 90000, pts: 0, arrivals: []time.Duration{40 * time.Millisecond, 10 * time.Millisecond, 12 * time.Millisecond}},
	})

	span, ok := s.span()
	require.True(t, ok)
	assert.Equal(t, t0.Add(10*time.Millisecond), span.Start)
	assert.False(t, span.Settled)
}

// Packets of frames dropped before the first written one (a camera waiting
// for its key frame) do not move the start.
func TestMediaSpanIgnoresFramesNotWritten(t *testing.T) {
	t0 := time.Now()
	var s mediaSpan

	for k := range 30 {
		s.arrived(uint32(k*3000), t0.Add(time.Duration(k)*33*time.Millisecond))
	}

	s.arrived(30*3000, t0.Add(time.Second))
	s.wrote(30*3000, 0, 0, videoSpan)

	span, ok := s.span()
	require.True(t, ok)
	assert.Equal(t, t0.Add(time.Second), span.Start)
}

// The first read finds five frames waiting, sent 20 ms apart: the start goes
// back to when the first of them would have arrived.
func TestMediaSpanTakesOffAStartBacklog(t *testing.T) {
	t0 := time.Now()
	s := runSpan(t0, audioSpan, audioFrames(110, func(k int) time.Duration {
		if k < 5 {
			return 80 * time.Millisecond
		}

		return time.Duration(k) * 20 * time.Millisecond
	}))

	span, ok := s.span()
	require.True(t, ok)
	assert.Equal(t, t0, span.Start)
	assert.True(t, span.Settled)
}

// A later frame that met less network delay than the first one shows the
// first was late.
func TestMediaSpanTakesTheLeastDelayedFrame(t *testing.T) {
	t0 := time.Now()
	s := runSpan(t0, audioSpan, audioFrames(50, func(k int) time.Duration {
		if k == 0 {
			return 30 * time.Millisecond
		}

		return time.Duration(k)*20*time.Millisecond + 5*time.Millisecond
	}))

	span, _ := s.span()
	assert.Equal(t, t0.Add(5*time.Millisecond), span.Start)
}

// Frames past a microphone's window keep the start, however early they are,
// and still extend the end.
func TestMediaSpanSettlesAfterItsWindow(t *testing.T) {
	t0 := time.Now()
	s := runSpan(t0, audioSpan, audioFrames(150, func(k int) time.Duration {
		if k*20 > int(MediaSpanWindow/time.Millisecond) {
			return time.Duration(k)*20*time.Millisecond - 50*time.Millisecond
		}

		return time.Duration(k) * 20 * time.Millisecond
	}))

	span, _ := s.span()
	assert.Equal(t, t0, span.Start)
	assert.Equal(t, t0.Add(150*20*time.Millisecond), span.End)
	assert.True(t, span.Settled)
}

// A microphone and a camera start behind a 300 ms path delay, which clears
// from 1.2 s to 1.6 s into their media. Both starts drop the delay, so the
// two files stay in lip sync.
func TestMediaSpanTakesAPathDelayOffBothKinds(t *testing.T) {
	t0 := time.Now()
	delay := func(pts time.Duration) time.Duration {
		switch {
		case pts < 1200*time.Millisecond:
			return 300 * time.Millisecond
		case pts < 1600*time.Millisecond:
			return 300 * time.Millisecond * (1600*time.Millisecond - pts) / (400 * time.Millisecond)
		default:
			return 0
		}
	}

	mic := runSpan(t0, audioSpan, audioFrames(150, func(k int) time.Duration {
		pts := time.Duration(k) * 20 * time.Millisecond
		return pts + delay(pts) + 2*time.Millisecond
	}))

	pts := ptsFrom(0, 66, 3000)
	cam := runSpan(t0, videoSpan, cameraFrames(pts, func(k int) time.Duration {
		return delay(time.Duration(pts[k])*time.Millisecond) + 2*time.Millisecond
	}))

	for name, s := range map[string]*mediaSpan{"microphone": mic, "camera": cam} {
		span, _ := s.span()
		assert.Equal(t, t0.Add(2*time.Millisecond), span.Start, name)
		assert.True(t, span.Settled, name)
	}
}

// A microphone's 300 ms timestamp jump with no gap in its arrivals settles
// the start.
func TestMediaSpanSettlesAtATimestampJump(t *testing.T) {
	t0 := time.Now()
	frames := audioFrames(5, func(k int) time.Duration { return time.Duration(k) * 20 * time.Millisecond })

	for k := range 20 {
		frames = append(frames, spanFrame{
			ts:       uint32(1000 + (5+k)*audioTicks + 300*48),
			pts:      time.Duration(5+k)*20*time.Millisecond + 300*time.Millisecond,
			arrivals: []time.Duration{time.Duration(5+k) * 20 * time.Millisecond},
		})
	}

	span, _ := runSpan(t0, audioSpan, frames).span()
	assert.Equal(t, t0, span.Start)
	assert.True(t, span.Settled)
}

// A frame after a long pause lasts 1 s at most, or 120 ms from a microphone.
func TestMediaSpanCapsTheLastFramesDuration(t *testing.T) {
	t0 := time.Now()
	s := runSpan(t0, videoSpan, []spanFrame{
		{ts: 0, pts: 0, arrivals: []time.Duration{0}},
		{ts: 3000, pts: 33 * time.Millisecond, arrivals: []time.Duration{33 * time.Millisecond}},
		{ts: 3000 + 5*90000, pts: 5033 * time.Millisecond, arrivals: []time.Duration{5033 * time.Millisecond}},
	})

	span, _ := s.span()
	assert.Equal(t, t0.Add(6033*time.Millisecond), span.End)

	dtx := audioFrames(10, func(k int) time.Duration { return time.Duration(k) * 20 * time.Millisecond })
	dtx = append(dtx, spanFrame{ts: 1000 + 580*48, pts: 580 * time.Millisecond, arrivals: []time.Duration{580 * time.Millisecond}})

	span, _ = runSpan(t0, audioSpan, dtx).span()
	assert.Equal(t, t0.Add(700*time.Millisecond), span.End)
}

func TestMediaSpanIsRelativeToTheFirstFrameWritten(t *testing.T) {
	t0 := time.Now()
	s := runSpan(t0, videoSpan, []spanFrame{
		{ts: 9000, pts: 100 * time.Millisecond, arrivals: []time.Duration{0}},
		{ts: 12000, pts: 133 * time.Millisecond, arrivals: []time.Duration{33 * time.Millisecond}},
	})

	span, _ := s.span()
	assert.Equal(t, t0, span.Start)
	assert.Equal(t, t0.Add(66*time.Millisecond), span.End)
}

// Chromium sends a camera's first frames late, at BBB's low profile (10
// fps) or captured at 5 or 7.5 fps, as in low light. A 15 fps camera whose
// first frame alone is late takes the same start.
func TestMediaSpanTakesOffACameraBacklog(t *testing.T) {
	for name, frames := range map[string][]spanFrame{
		"10 fps":  cameraFrames(ptsFrom(0, 100, 2900), lateFirst(342, 322, 262, 222, 142, 82)),
		"5 fps":   cameraFrames(ptsFrom(0, 200, 3800), lateFirst(321, 222, 61)),
		"7.5 fps": cameraFrames(append([]int{0, 150, 300, 400, 549}, ptsFrom(700, 150, 2500)...), lateFirst(378, 308, 218, 178, 69)),
		"15 fps":  cameraFrames(ptsFrom(0, 67, 2948), lateFirst(70)),
	} {
		t0 := time.Now()
		span, _ := runSpan(t0, videoSpan, frames).span()
		assert.Equal(t, t0.Add(2*time.Millisecond), span.Start, name)
		assert.True(t, span.Settled, name)
	}
}

// At 30 or 40 kbps, Chromium sends a few late frames, then none for up to
// 700 ms. It is on time only 1 to 1.2 s in.
func TestMediaSpanWaitsForACameraThatSkipsFrames(t *testing.T) {
	for name, frames := range map[string][]spanFrame{
		"30 kbps": cameraFrames(append([]int{0, 100, 300}, ptsFrom(1001, 100, 2501)...), lateFirst(400, 540, 440)),
		"40 kbps": cameraFrames(append([]int{0, 99, 200, 400, 800}, ptsFrom(1200, 100, 2600)...), lateFirst(401, 502, 501, 421, 121)),
	} {
		t0 := time.Now()
		span, _ := runSpan(t0, videoSpan, frames).span()
		assert.Equal(t, t0.Add(2*time.Millisecond), span.Start, name)
		assert.True(t, span.Settled, name)
	}
}

// The frame at PTS 2 s is the last in the camera's window.
func TestMediaSpanSettlesACameraAfterItsWindow(t *testing.T) {
	pts := ptsFrom(0, 100, 3000)
	early := func(at int) func(k int) time.Duration {
		return func(k int) time.Duration {
			if pts[k] == at {
				return -400 * time.Millisecond
			}

			return 0
		}
	}

	t0 := time.Now()
	span, _ := runSpan(t0, videoSpan, cameraFrames(pts, early(2000))).span()
	assert.Equal(t, t0.Add(-400*time.Millisecond), span.Start)

	span, _ = runSpan(t0, videoSpan, cameraFrames(pts, early(2100))).span()
	assert.Equal(t, t0, span.Start)
	assert.True(t, span.Settled)
}

// A camera's frames read 100 ms late, a pause, then frames on time. The
// frames after the pause set the start. A video jump step under 1.6 s
// would settle the start at the 1.6 s pause.
func TestMediaSpanKeepsACameraWindowOpenAcrossAPause(t *testing.T) {
	for name, resume := range map[string]int{"500 ms": 800, "1.6 s": 1900} {
		var pts []int
		for k := range 10 {
			pts = append(pts, k*1000/30)
		}
		paused := len(pts)
		for k := 0; pts[len(pts)-1] < 2500; k++ {
			pts = append(pts, resume+k*1000/30)
		}

		t0 := time.Now()
		span, _ := runSpan(t0, videoSpan, cameraFrames(pts, func(k int) time.Duration {
			if k < paused {
				return 100 * time.Millisecond
			}

			return 0
		})).span()
		assert.Equal(t, t0, span.Start, name)
		assert.True(t, span.Settled, name)
	}
}

// A camera's PTS jump with no gap in its arrivals. Inside the window, the
// frames after it set the start, so they play where they arrived. Past the
// window, the start settles at the jump.
func TestMediaSpanFollowsACameraJumpInItsWindow(t *testing.T) {
	jumped := func(jump time.Duration) []spanFrame {
		frames := make([]spanFrame, 75)
		for k := range frames {
			at := time.Duration(k*1000/30) * time.Millisecond
			pts := at
			if k >= 10 {
				pts += jump
			}
			frames[k] = spanFrame{ts: uint32(pts / time.Millisecond * 90), pts: pts, arrivals: []time.Duration{at}}
		}

		return frames
	}

	t0 := time.Now()
	span, _ := runSpan(t0, videoSpan, jumped(500*time.Millisecond)).span()
	assert.Equal(t, t0.Add(-500*time.Millisecond), span.Start)
	assert.True(t, span.Settled)

	// Up to the jump's frame only.
	past := jumped(2500 * time.Millisecond)[:11]
	span, _ = runSpan(t0, videoSpan, past).span()
	assert.Equal(t, t0, span.Start)
	assert.True(t, span.Settled)
}

// A camera's first frames are held, then come in at once. Held until 1.5 s
// in, the start moves back 1 s, no further; until 800 ms in, all the way.
func TestMediaSpanMovesTheStartBackAtMostOneSecond(t *testing.T) {
	var pts []int
	for k := range 90 {
		pts = append(pts, k*1000/30)
	}
	stalled := func(stall int) func(k int) time.Duration {
		return func(k int) time.Duration {
			return time.Duration(max(stall-pts[k], 0)) * time.Millisecond
		}
	}

	t0 := time.Now()
	span, _ := runSpan(t0, videoSpan, cameraFrames(pts, stalled(1500))).span()
	assert.Equal(t, t0.Add(500*time.Millisecond), span.Start)
	assert.True(t, span.Settled)

	span, _ = runSpan(t0, videoSpan, cameraFrames(pts, stalled(800))).span()
	assert.Equal(t, t0, span.Start)

	// A microphone's start takes the same limit.
	span, _ = runSpan(t0, audioSpan, audioFrames(150, func(k int) time.Duration {
		return max(1500*time.Millisecond, time.Duration(k)*20*time.Millisecond)
	})).span()
	assert.Equal(t, t0.Add(500*time.Millisecond), span.Start)
}

// Opus packets can be 120 ms long, so a microphone's step of 120 ms leaves
// its start open.
func TestMediaSpanKeepsAudioOpenAtA120msStep(t *testing.T) {
	var frames []spanFrame
	for p := 0; p <= 960; p += 120 {
		at := p
		if p == 0 {
			at = 50
		}
		frames = append(frames, spanFrame{
			ts:       uint32(1000 + p*opusSampleRate/1000),
			pts:      time.Duration(p) * time.Millisecond,
			arrivals: []time.Duration{time.Duration(at) * time.Millisecond},
		})
	}

	t0 := time.Now()
	span, _ := runSpan(t0, audioSpan, frames).span()
	assert.Equal(t, t0, span.Start)
}

// A microphone's PTS step over 120 ms settles the start, even with no
// matching gap in its arrivals. Here it is a 120 ms gap MediaClock inserts,
// or a DTX step whose next frame is read 30 ms early.
func TestMediaSpanSettlesAudioAtAGapAheadOfItsArrivals(t *testing.T) {
	frame := func(pts, at int) spanFrame {
		return spanFrame{
			ts:       uint32(1000 + pts*opusSampleRate/1000),
			pts:      time.Duration(pts) * time.Millisecond,
			arrivals: []time.Duration{time.Duration(at) * time.Millisecond},
		}
	}

	var gap []spanFrame
	for p := 0; p <= 200; p += 20 {
		gap = append(gap, frame(p, p))
	}
	for p := 340; p <= 1500; p += 20 {
		gap = append(gap, frame(p, p-120))
	}

	var dtx []spanFrame
	for p := 0; p <= 180; p += 20 {
		dtx = append(dtx, frame(p, p))
	}
	dtx = append(dtx, frame(580, 550))
	for p := 980; p <= 2600; p += 400 {
		dtx = append(dtx, frame(p, p))
	}

	for name, frames := range map[string][]spanFrame{"MediaClock gap": gap, "DTX": dtx} {
		t0 := time.Now()
		span, _ := runSpan(t0, audioSpan, frames).span()
		assert.Equal(t, t0, span.Start, name)
		assert.True(t, span.Settled, name)
	}
}
