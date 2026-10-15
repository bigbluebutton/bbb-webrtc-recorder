//go:build e2e && linux

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// The recorder's trackbuffer.go gapKeyframeDelay: a gap is never requested
// for sooner after the packet that reveals it.
const gapRequestDelay = 200 * time.Millisecond

const reasonPacketDrop = "packet_drop"

// TestUnrepairedLossRequestsKeyframe withholds one packet from the middle of a
// camera frame. LiveKit cannot repair it, so the recorder must ask for a key
// frame early, by its sequence gap, not when the jitter buffer gives up on the
// frame.
func TestUnrepairedLossRequestsKeyframe(t *testing.T) {
	for _, sc := range []lossScenario{
		{
			name: "s2-15fps", fps: 15, lossAfter: 3 * time.Second, lossToStop: 5 * time.Second,
			request: [2]time.Duration{200 * time.Millisecond, 240 * time.Millisecond},
		},
		{
			name: "s2-5fps", fps: 5, lossAfter: 3 * time.Second, lossToStop: 6 * time.Second,
			request: [2]time.Duration{390 * time.Millisecond, 430 * time.Millisecond},
		},
	} {
		t.Run(fmt.Sprintf("%gfps", sc.fps), func(t *testing.T) {
			r := runLoss(t, sc)
			webm, l := r.WebM, r.Trace
			gap := r.lossGap(t)

			assert.Equal(t, len(l.PLIs), len(l.requestsSince(gap.RevealUs, "")), "keyframe requests before the loss")

			var delay time.Duration
			reqs := l.requestsSince(gap.RevealUs, reasonPacketDrop)

			if assert.NotEmpty(t, reqs, "buffer request after the loss") {
				delay = time.Duration(reqs[0].AtUs-gap.RevealUs) * time.Microsecond
				assert.GreaterOrEqual(t, delay, gapRequestDelay, "request after the reveal")
				assert.Less(t, delay, sc.requestBound(), "request after the reveal")
			}

			assert.LessOrEqual(t, len(l.requestsSince(gap.RevealUs, "")), 2, "keyframe requests after the loss")

			pubPLIs := plisSince(r.Cam, r.Dropped.At)
			assert.GreaterOrEqual(t, pubPLIs, 1, "publisher PLIs after the loss")
			assert.GreaterOrEqual(t, pliKeysSince(r.Cam, r.Dropped.At), 1, "key frames sent for a PLI after the loss")

			assertResponseKey(t, r, gap)

			lossPTS := l.ptsOf(webm, gap.TS)
			after := framesAfter(webm, lossPTS)
			minAfter := sc.minFrames(r.StopSent.Sub(r.DropAt) - sc.lossOutage())
			assert.GreaterOrEqual(t, after, minAfter, "frames written after the loss")
			assert.NoError(t, webm.CheckMonotonic())
			assert.NoError(t, webm.CheckNoDuplicatePTS())

			t.Logf("closedloop %s: requestAfterRevealMs=%.1f bound=[%.0f,%.0f) requests=%v "+
				"publisherPLIsAfterLoss=%d framesAfterLoss=%d min=%d blocks=%d vp8Keyframes=%d maxGapMs=%d",
				sc.name, ms(delay), ms(gapRequestDelay), ms(sc.requestBound()), pliReasons(l),
				pubPLIs, after, minAfter, webm.Blocks, webm.VP8Keyframes, webm.MaxGapMs)
		})
	}
}

// TestPacedKeyframesNoLoop runs a camera at the fake device's mixed-flow rate
// whose PLI key frames are paced over 664 ms, the top of the 480-664 ms
// measured, and withholds one packet. A jitter latency the pacing outlasts
// drops every key frame the loss's request brings, which requests another:
// a keyframe loop, which writes no frames.
func TestPacedKeyframesNoLoop(t *testing.T) {
	sc := lossScenario{
		name: "s3", fps: 3.4, keyPackets: 12, keySpread: 664 * time.Millisecond,
		lossAfter: 6 * time.Second, recordFor: 20 * time.Second,
		request: [2]time.Duration{580 * time.Millisecond, 620 * time.Millisecond},
	}

	r := runLoss(t, sc)
	webm, l := r.WebM, r.Trace
	gap := r.lossGap(t)

	// Otherwise the bounds below hold for a loss the recorder never saw.
	var delay time.Duration
	if reqs := l.requestsSince(gap.RevealUs, reasonPacketDrop); assert.NotEmpty(t, reqs, "buffer request after the loss") {
		delay = time.Duration(reqs[0].AtUs-gap.RevealUs) * time.Microsecond
	}

	// The loop detector: one request for the loss, and room for one more,
	// e.g. at startup. A loop asks again for every key frame it drops.
	assert.LessOrEqual(t, len(l.PLIs), 2, "keyframe requests")

	// Each request reaches the publisher as one PLI, so the loss can bring
	// two at most. PLIs before it are not counted: LiveKit repeats its own,
	// throttled to one per 500 ms, until the downtrack's first key frame,
	// which the pacing and a slow runner can delay past one repeat.
	pubPLIs := plisSince(r.Cam, r.Dropped.At)
	assert.LessOrEqual(t, pubPLIs, 2, "publisher PLIs after the loss")

	assertResponseKey(t, r, gap)

	minFrames := sc.minFrames(r.StopSent.Sub(r.Started) - lossStartupAllowance - sc.lossOutage())
	assert.GreaterOrEqual(t, webm.Blocks, minFrames, "frames written")
	assert.NoError(t, webm.CheckMonotonic())
	assert.NoError(t, webm.CheckNoDuplicatePTS())

	t.Logf("closedloop %s: requestAfterRevealMs=%.1f requests=%v publisherPLIsAfterLoss=%d (all %v) blocks=%d min=%d "+
		"vp8Keyframes=%d maxGapMs=%d",
		sc.name, ms(delay), pliReasons(l), pubPLIs, durationsMs(r.Cam.PLIs()), webm.Blocks, minFrames,
		webm.VP8Keyframes, webm.MaxGapMs)
}

// assertResponseKey checks that the first key frame read after the loss
// is the one the publisher sent for its PLI, and that it reached the WebM.
func assertResponseKey(t *testing.T, r *lossRun, gap traceGap) {
	t.Helper()

	webm, l := r.WebM, r.Trace

	assert.GreaterOrEqual(t, webm.VP8Keyframes, 2, "VP8 key frames: the first and the loss's")

	key, ok := l.keyAfter(gap.RevealUs)
	if !assert.True(t, ok, "no key frame read after the loss") {
		return
	}

	// LiveKit shifts timestamps by a constant, so the key frame lies as far
	// from the lost packet's frame in the trace as it was sent.
	if sent, ok := pliKeyAfter(r.Cam, r.Dropped.At); assert.True(t, ok, "no key frame sent after the loss") {
		assert.Equal(t, sent.TS-r.Dropped.TS, key.TS-gap.TS, "key frame read after the loss: timestamp distance")
	}

	pts := l.ptsOf(webm, key.TS)
	assert.True(t, hasPTS(webm, pts), "the key frame read %.1f ms after the loss, PTS %d, is not in the WebM",
		float64(key.AtUs-gap.RevealUs)/1000, pts)
}

func pliReasons(l *traceLoss) []string {
	out := make([]string, len(l.PLIs))
	for i, r := range l.PLIs {
		out[i] = fmt.Sprintf("%s@%.1fms", r.Reason, float64(r.AtUs)/1000)
	}

	return out
}
