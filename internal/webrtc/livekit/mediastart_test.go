package livekit

import (
	"cmp"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A packet the buffer holds behind a loss keeps the time it was pushed.
func TestTrackBufferReleasesPacketsWithTheirArrival(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newTrackBuffer(&codecs.OpusPacket{}, testLatency, lossKeyframeInterval, func() {})
		defer b.close()

		t0 := time.Now()
		assert.Len(t, b.pushExt(opusPacket(100), t0), 1)
		assert.Empty(t, b.pushExt(opusPacket(102), t0.Add(30*time.Millisecond)))

		released := b.pushExt(opusPacket(101), t0.Add(50*time.Millisecond))
		require.Len(t, released, 2)
		assert.Equal(t, uint16(101), released[0].SequenceNumber)
		assert.Equal(t, t0.Add(50*time.Millisecond), released[0].ReceivedAt)
		assert.Equal(t, uint16(102), released[1].SequenceNumber)
		assert.Equal(t, t0.Add(30*time.Millisecond), released[1].ReceivedAt)
	})
}

func TestReadTrackPushesEachPacketWithItsArrival(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := newPushRecorder()
		h := newLoopHarness(rec, audioInfo.id)
		reads := audioReads(0, 100, 50)
		// 110 comes in after 111, 15 ms later.
		reads[10], reads[11] = reads[11], reads[10]
		reads[11].at = reads[10].at + 15*time.Millisecond

		src := newFakeTrack(reads)
		h.run(src, audioInfo)

		require.Equal(t, seqRange(100, 50), rec.audio)
		for _, r := range reads {
			assert.Equal(t, src.start.Add(r.at), rec.arrivals[r.pkt.SequenceNumber], "seq %d", r.pkt.SequenceNumber)
		}
	})
}

// The buffer holds the key frame until its late middle packet comes in.
func TestReadTrackPushesEachVideoPacketWithItsArrival(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := newPushRecorder()
		h := newLoopHarness(rec, videoInfo.id)
		reads := keyFrameReads(300*time.Millisecond, 30)
		late := reads[1]
		late.at += 80 * time.Millisecond
		reads = append(append([]trackRead{reads[0], reads[2]}, late), reads[3:]...)

		src := newFakeTrack(reads)
		h.run(src, videoInfo)

		require.Equal(t, seqRange(1000, len(reads)), rec.video)
		for _, r := range reads {
			assert.Equal(t, src.start.Add(r.at), rec.arrivals[r.pkt.SequenceNumber], "seq %d", r.pkt.SequenceNumber)
		}
	})
}

// mediaStartHarness is a loopHarness over a real WebmRecorder that keeps
// what the media start callback and the flow callback reported, in order.
type mediaStartHarness struct {
	*loopHarness

	mu     sync.Mutex
	events []string
	starts []time.Time
	sentAt []time.Time // when each media start reached the callback
}

func newMediaStartHarness(t *testing.T, info trackInfo) *mediaStartHarness {
	rec := recorder.NewWebmRecorder(filepath.Join(t.TempDir(), "track.webm"), 0o600, 256, 32, true, false)
	rec.SetHasAudio(info.kind == TrackKindAudio)
	rec.SetHasVideo(info.kind == TrackKindVideo)

	h := &mediaStartHarness{loopHarness: newLoopHarness(rec, info.id)}
	h.w.flowCallback = func(isFlowing bool, _ time.Duration, _ bool) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if isFlowing {
			h.events = append(h.events, "flowing")
		}
	}
	h.w.mediaStartCallback = func(start time.Time) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.events = append(h.events, "mediaStart")
		h.starts = append(h.starts, start)
		h.sentAt = append(h.sentAt, time.Now())
	}

	return h
}

// reported closes the recorder, whose writer runs goroutines in the bubble,
// and waits for the events still being delivered.
func (h *mediaStartHarness) reported() ([]string, []time.Time) {
	h.w.rec.Close()
	synctest.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]string(nil), h.events...), append([]time.Time(nil), h.starts...)
}

// pinnedMediaStartFinal is mediaStartFinal as recordingMediaStarted's doc
// gives it. A change to mediaStartFinal must change that doc too.
const pinnedMediaStartFinal = 4500 * time.Millisecond

// assertSentFinal checks how old the unsettled start was when it went out:
// from pinnedMediaStartFinal to one notFlowingTicker more.
func (h *mediaStartHarness) assertSentFinal(t *testing.T, start time.Time) {
	t.Helper()

	age := h.sentAt[0].Sub(start)
	assert.GreaterOrEqual(t, age, pinnedMediaStartFinal)
	assert.Less(t, age, pinnedMediaStartFinal+notFlowingTicker)
}

// The media start goes out once, when the recorder settles it, after the
// flowing event the first packets made. The first read finds five frames
// waiting, all read together. The start is 300 ms, when the first frame
// was due, not 380 ms, when the five were read.
func TestAdapterReportsTheMediaStartOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newMediaStartHarness(t, audioInfo)
		reads := audioReads(300*time.Millisecond, 100, 150)
		for i := range 5 {
			reads[i].at = reads[4].at
		}

		src := newFakeTrack(reads)
		h.run(src, audioInfo)

		events, starts := h.reported()
		assert.Equal(t, []string{"flowing", "mediaStart"}, events)
		require.Len(t, starts, 1)
		assert.Equal(t, src.start.Add(300*time.Millisecond), starts[0])
	})
}

// A recording too short to settle its start reports none: the stop carries
// the media span.
func TestAdapterReportsNoUnsettledMediaStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newMediaStartHarness(t, audioInfo)
		src := newFakeTrack(audioReads(0, 100, 10))

		h.run(src, audioInfo)

		events, _ := h.reported()
		assert.Equal(t, []string{"flowing"}, events)
	})
}

// videoReads is a camera from offset at: a key frame, then delta frames,
// of three packets each, read together.
func videoReads(at time.Duration, fps, frames int) []trackRead {
	var reads []trackRead
	seq := uint16(1000)
	for f := range frames {
		for j := range 3 {
			payload := append(corpus.VP8Payload(uint16(f+1), j == 0, f == 0, 640, 480), make([]byte, 64)...)
			reads = append(reads, trackRead{
				at: at + time.Duration(f)*time.Second/time.Duration(fps),
				pkt: &rtp.Packet{
					Header:  rtp.Header{SequenceNumber: seq, Timestamp: uint32(f * 90000 / fps), Marker: j == 2},
					Payload: payload,
				},
			})
			seq++
		}
	}

	return reads
}

// keyFrameReads is a 5 fps camera whose frames after the key frame are read
// 100 ms later than their pace. So only the key frame's earliest packet can
// time the media start.
func keyFrameReads(at time.Duration, frames int) []trackRead {
	reads := videoReads(at, 5, frames)
	for i := 3; i < len(reads); i++ {
		reads[i].at += 100 * time.Millisecond
	}

	return reads
}

// A camera reports its media start too. Its key frame's middle packet comes
// in first, and the buffer holds the frame until the other two come in.
func TestAdapterTimesAHeldKeyFrameByItsEarliestPacket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newMediaStartHarness(t, videoInfo)
		reads := keyFrameReads(300*time.Millisecond, 30)
		first, last := reads[0], reads[2]
		first.at += 80 * time.Millisecond
		last.at += 40 * time.Millisecond
		reads = append([]trackRead{reads[1], last, first}, reads[3:]...)

		src := newFakeTrack(reads)
		h.run(src, videoInfo)

		_, starts := h.reported()
		require.Len(t, starts, 1)
		assert.Equal(t, src.start.Add(300*time.Millisecond), starts[0])
	})
}

// The frame at PTS 2 s, the last in the camera's window, moves the start
// back 30 ms by its middle packet. Its first packet is 480 ms late. Its
// last packet and the later frames are read almost videoJitterLatency
// later. So it is written 4.46 s after the 350 ms start, near the latest a
// frame from the window can be.
//
// No packet is read in between, so the camera stops flowing and the ticks
// come every 100 ms, the last 10 ms before the write. A mediaStartFinal of
// 4.45 s or less reports the 350 ms start. The camera starts off the ticks,
// since a tick at a read's instant can run before or after it.
func TestAdapterReportsTheStartAHeldFrameMoves(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newMediaStartHarness(t, videoInfo)
		reads := videoReads(350*time.Millisecond, 10, 50)
		last := 3 * int(recorder.MediaSpanWindow/(100*time.Millisecond))
		frame := reads[last : last+3]
		frame[1].at -= 30 * time.Millisecond
		frame[0].at += 480 * time.Millisecond
		frame[2].at = frame[0].at + videoJitterLatency - 20*time.Millisecond
		behind := frame[2].at + 10*time.Millisecond - reads[last+3].at
		for i := last + 3; i < len(reads); i++ {
			reads[i].at += behind
		}
		slices.SortStableFunc(reads, func(a, b trackRead) int { return cmp.Compare(a.at, b.at) })

		src := newFakeTrack(reads)
		h.run(src, videoInfo)

		_, starts := h.reported()
		require.Len(t, starts, 1)
		assert.Equal(t, src.start.Add(320*time.Millisecond), starts[0])
		span, ok := h.w.rec.MediaSpan()
		require.True(t, ok)
		assert.Equal(t, starts[0], span.Start)
	})
}

// A camera that captures at 5 fps sends frames 200 ms apart. Chromium sends
// its first ones late, and the frames that caught up set the start.
func TestAdapterReportsALowFrameRateCameraStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newMediaStartHarness(t, videoInfo)
		reads := videoReads(300*time.Millisecond, 5, 30)
		for i, late := range []time.Duration{321, 222, 61} {
			for j := range 3 {
				reads[3*i+j].at += late * time.Millisecond
			}
		}

		src := newFakeTrack(reads)
		h.run(src, videoInfo)

		events, starts := h.reported()
		assert.Equal(t, []string{"flowing", "mediaStart"}, events)
		require.Len(t, starts, 1)
		assert.Equal(t, src.start.Add(300*time.Millisecond), starts[0])
	})
}

// A camera pauses 500 ms in. Like the paused microphone of
// TestAdapterReportsTheMediaStartOfAPausedMicrophone, it writes no frame to
// settle its start. So the start goes out at mediaStartFinal.
func TestAdapterReportsTheMediaStartOfAPausedCamera(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newMediaStartHarness(t, videoInfo)
		reads := videoReads(300*time.Millisecond, 10, 16)
		for i := 18; i < len(reads); i++ {
			reads[i].at += 10*time.Second - 900*time.Millisecond
		}

		src := newFakeTrack(reads)
		h.run(src, videoInfo)

		events, starts := h.reported()
		assert.Equal(t, []string{"flowing", "mediaStart", "flowing"}, events)
		require.Len(t, starts, 1)
		assert.Equal(t, src.start.Add(300*time.Millisecond), starts[0])

		h.assertSentFinal(t, starts[0])
	})
}

// A microphone that mutes 200 ms in writes no frame to settle its start.
// The start goes out at mediaStartFinal, the same age as a camera's.
func TestAdapterReportsTheMediaStartOfAPausedMicrophone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newMediaStartHarness(t, audioInfo)
		src := newFakeTrack(audioReads(300*time.Millisecond, 100, 10), audioReads(10*time.Second, 110, 5))

		h.run(src, audioInfo)

		events, starts := h.reported()
		assert.Equal(t, []string{"flowing", "mediaStart", "flowing"}, events)
		require.Len(t, starts, 1)
		assert.Equal(t, src.start.Add(300*time.Millisecond), starts[0])

		h.assertSentFinal(t, starts[0])
	})
}
