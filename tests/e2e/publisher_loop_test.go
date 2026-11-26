//go:build e2e && linux

package e2e

import (
	"encoding/binary"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
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
	tr.startSending(time.Now())

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

// TestPublisherSendLoopH264 runs an H.264 send loop as TestPublisherSendLoop
// does, and depacketizes what it wrote. The PLI's key frame must be the
// schedule's, parameter sets first. Each frame's first slice must carry the
// frame's tag, and the parameter sets must reach the receiver unchanged.
func TestPublisherSendLoopH264(t *testing.T) {
	const (
		fps    = 50
		frames = 10
		pliAt  = 50 * time.Millisecond
		stopAt = 300 * time.Millisecond
	)

	sched, err := corpus.GenerateH264(corpus.H264Options{
		FPS: fps, Frames: frames, Slices: 2, KeySlicePackets: 3, DeltaSlicePackets: 2,
	})
	require.NoError(t, err)

	tr := newPubTrack(t, h264Capability, corpus.KindVideo, TrackOptions{Schedule: sched, TagFrames: true})

	var (
		mu      sync.Mutex
		written []*rtp.Packet
	)
	tr.tap = func(p *rtp.Packet) {
		mu.Lock()
		written = append(written, p.Clone())
		mu.Unlock()
	}

	tr.startSending(time.Now())
	time.Sleep(time.Until(tr.Start().Add(pliAt)))
	tr.onRTCP(&rtcp.PictureLossIndication{})
	time.Sleep(time.Until(tr.Start().Add(stopAt)))
	tr.Stop()

	sent := map[uint16]SentPacket{}
	for _, s := range tr.Sent() {
		sent[s.Seq] = s
	}

	byFrame := map[int][]*rtp.Packet{}
	var order []int
	for _, p := range written {
		f := sent[p.SequenceNumber].Frame
		byFrame[f] = append(byFrame[f], p)
		// The stop can cut the last frame short: only whole frames count.
		if p.Marker {
			order = append(order, f)
		}
	}
	require.Greater(t, len(order), frames, "the loop wrote one pass and more")

	pliKeys := 0
	for _, f := range order {
		first := sent[byFrame[f][0].SequenceNumber]
		nals := depacketizeH264(t, byFrame[f])

		types := make([]byte, len(nals))
		for i, n := range nals {
			types[i] = n[0] & 0x1f
		}

		if first.Key {
			assert.True(t, corpus.H264KeyFrameStart(byFrame[f][0].Payload), "frame %d: LiveKit's key frame start", f)
			require.GreaterOrEqual(t, len(nals), 3, "frame %d: NAL units", f)
			assert.Equal(t, corpus.H264SPS(640, 360), nals[0], "frame %d: the SPS", f)
			assert.Equal(t, []byte{8, 5}, types[1:3], "frame %d: a PPS, then the IDR slice", f)
		} else {
			assert.NotContains(t, types, byte(5), "frame %d: an IDR slice in a delta frame", f)
		}

		if first.PLIKey {
			pliKeys++
		}

		slice := slices.IndexFunc(nals, func(n []byte) bool { return n[0]&0x1f == 1 || n[0]&0x1f == 5 })
		require.GreaterOrEqual(t, slice, 0, "frame %d: no slice", f)
		tag, ok := corpus.TaggedFrame(nals[slice])
		assert.True(t, ok, "frame %d: no tag in its first slice", f)
		assert.Equal(t, uint32(f), tag, "frame %d: its first slice's tag", f)
	}

	assert.Equal(t, 1, pliKeys, "key frames sent for the PLI")
}

// depacketizeH264 returns the NAL units of one frame's packets, in order.
func depacketizeH264(t *testing.T, packets []*rtp.Packet) [][]byte {
	t.Helper()

	d := codecs.H264Packet{IsAVC: true}
	var avcc []byte
	for _, p := range packets {
		out, err := d.Unmarshal(p.Payload)
		require.NoError(t, err, "seq %d", p.SequenceNumber)
		avcc = append(avcc, out...)
	}

	var nals [][]byte
	for len(avcc) > 0 {
		require.GreaterOrEqual(t, len(avcc), 4)
		n := int(binary.BigEndian.Uint32(avcc))
		require.LessOrEqual(t, 4+n, len(avcc))
		nals = append(nals, avcc[4:4+n])
		avcc = avcc[4+n:]
	}

	return nals
}

func TestScheduleFramesFindsKeyFrames(t *testing.T) {
	g, err := corpus.GenerateVP8(corpus.VP8Options{FPS: 1, Frames: 2})
	require.NoError(t, err)

	frames := scheduleFrames(g.Records, keyFrameStart(corpus.CodecVP8))
	require.Len(t, frames, 2)
	assert.True(t, frames[0].key)
	assert.False(t, frames[1].key)

	// In mode 0 the SPS travels alone, ahead of the PPS and the IDR.
	for _, mode0 := range []bool{false, true} {
		h, err := corpus.GenerateH264(corpus.H264Options{FPS: 1, Frames: 3, KeyInterval: 2, Mode0: mode0})
		require.NoError(t, err)

		frames := scheduleFrames(h.Records, keyFrameStart(corpus.CodecH264))
		require.Len(t, frames, 3)
		assert.Equal(t, []bool{true, false, true}, []bool{frames[0].key, frames[1].key, frames[2].key}, "mode 0: %v", mode0)
	}
}
