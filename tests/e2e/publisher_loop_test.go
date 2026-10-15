//go:build e2e && linux

package e2e

import (
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/pion/rtcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublisherSendLoop runs a video send loop on an unpublished track, whose
// writes go nowhere, and checks what it sent: the PLI response, a withheld
// packet, a pause and the schedule's loop. Times are only checked as lower
// bounds, on the scheduled send times, or with slack for a late wakeup.
func TestPublisherSendLoop(t *testing.T) {
	const (
		fps        = 50 // 20 ms frames, 1800 ticks
		frames     = 20 // one pass is 400 ms
		ticks      = 90000 / fps
		pliAt      = 100 * time.Millisecond
		delay      = 50 * time.Millisecond
		spread     = 30 * time.Millisecond
		pauseAt    = 200 * time.Millisecond
		resumeAt   = 340 * time.Millisecond
		stopAt     = 600 * time.Millisecond
		droppedSeq = 1000 + 7
		// How late Pause or Resume may run, or the loop read a frame: the
		// loop checks the pause when it wakes for a frame, not at its due time.
		pauseSlack = 40 * time.Millisecond
	)

	sched, err := corpus.GenerateVP8(corpus.VP8Options{FPS: fps, Frames: frames, StartSeq: 1000, StartTS: 5000})
	require.NoError(t, err)

	tr := newPubTrack(t, vp8Capability, corpus.KindVideo, TrackOptions{
		Schedule:       sched,
		KeyframeDelay:  delay,
		KeyframeSpread: spread,
		Drop:           func(s SentPacket) bool { return s.Seq == droppedSeq },
	})
	tr.startSending()

	time.Sleep(time.Until(tr.Start().Add(pliAt)))
	tr.onRTCP(&rtcp.PictureLossIndication{})
	tr.onRTCP(&rtcp.PictureLossIndication{}) // coalesces with the first
	time.Sleep(time.Until(tr.Start().Add(pauseAt)))
	tr.Pause()
	time.Sleep(time.Until(tr.Start().Add(resumeAt)))
	tr.Resume()
	time.Sleep(time.Until(tr.Start().Add(stopAt)))
	tr.Stop()

	sent := tr.Sent()
	require.NotEmpty(t, sent)

	plis := tr.PLIs()
	require.Len(t, plis, 2)

	var withheld []uint16
	var pliKeys []SentPacket
	seenFrames := map[int]bool{}

	for i, s := range sent {
		if i > 0 {
			assert.Equal(t, sent[i-1].Seq+1, s.Seq, "sequence numbers continue through drops, pauses and loops")
			assert.GreaterOrEqual(t, s.At, sent[i-1].At, "send order")
		}

		// Timestamps follow the frame index across passes of the schedule.
		assert.Equal(t, uint32(5000+s.Frame*ticks), s.TS, "frame %d", s.Frame)

		if s.Dropped {
			withheld = append(withheld, s.Seq)
		}

		if s.PLIKey {
			pliKeys = append(pliKeys, s)
		}

		seenFrames[s.Frame] = true
	}

	assert.Equal(t, []uint16{droppedSeq}, withheld)

	// One key frame for both PLIs: the template's four packets, due no
	// earlier than the first PLI plus the delay and paced over the spread.
	require.Len(t, pliKeys, 4)
	assert.GreaterOrEqual(t, pliKeys[0].At, plis[0]+delay)
	assert.GreaterOrEqual(t, pliKeys[0].Due, plis[0]+delay)
	assert.Equal(t, spread, pliKeys[3].Due-pliKeys[0].Due)
	assert.True(t, pliKeys[3].Marker)

	// The pause skips whole frames; the loop goes on past one pass, whose
	// first frame is due 60 ms after the resume.
	frameDur := time.Second / fps
	for f := int((pauseAt+pauseSlack)/frameDur) + 1; time.Duration(f)*frameDur < resumeAt-pauseSlack; f++ {
		assert.False(t, seenFrames[f], "frame %d is due during the pause", f)
	}
	assert.True(t, seenFrames[frames], "the second pass's first frame")
	_, keys := tr.SentBetween(0, stopAt)
	assert.Equal(t, 3, keys, "the schedule's key frame on each pass, and the PLI's")
}

func TestVP8PayloadHelpers(t *testing.T) {
	g, err := corpus.GenerateVP8(corpus.VP8Options{FPS: 1, Frames: 2, StartPictureID: 0x1234})
	require.NoError(t, err)

	frames := scheduleFrames(g.Records, true)
	require.Len(t, frames, 2)
	assert.True(t, frames[0].key)
	assert.False(t, frames[1].key)
	assert.Equal(t, 4, vp8DescriptorLen(frames[0].recs[0].Pay))

	id, ok := vp8PictureID(frames[1].recs[0].Pay)
	require.True(t, ok)
	assert.Equal(t, uint16(0x1235), id)

	p := frames[1].recs[0].Packet().Payload
	setVP8PictureID(p, 0x7FFF)
	id, _ = vp8PictureID(p)
	assert.Equal(t, uint16(0x7FFF), id)
	assert.False(t, isVP8KeyFrameStart(p))
}
