package livekit

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
)

const (
	testLatency = 200 * time.Millisecond
	// Real-clock tests run with a short latency so a gap expires quickly.
	shortLatency = 20 * time.Millisecond
)

func vp8Packet(seq uint16, ts uint32, start, end bool) *rtp.Packet {
	descriptor := byte(0x00)
	if start {
		descriptor = 0x10
	}

	return &rtp.Packet{
		Header:  rtp.Header{SequenceNumber: seq, Timestamp: ts, Marker: end},
		Payload: []byte{descriptor, 0xAA},
	}
}

func opusPacket(seq uint16) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{SequenceNumber: seq, Timestamp: uint32(seq) * 960},
		Payload: []byte{0xFC, 0xAA},
	}
}

func seqNums(packets []*rtp.Packet) []uint16 {
	seqs := make([]uint16, 0, len(packets))
	for _, p := range packets {
		seqs = append(seqs, p.SequenceNumber)
	}

	return seqs
}

func pushAll(b *trackBuffer, packets ...*rtp.Packet) []*rtp.Packet {
	var ready []*rtp.Packet
	for _, p := range packets {
		ready = append(ready, b.push(p)...)
	}

	return ready
}

func newCountingBuffer(depacketizer rtp.Depacketizer, latency, lossInterval time.Duration) (*trackBuffer, *atomic.Int32) {
	losses := &atomic.Int32{}

	return newTrackBuffer(depacketizer, latency, lossInterval, func() { losses.Add(1) }), losses
}

// eventually polls cond on the real clock for up to a second.
func eventually(cond func() bool) bool {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}

		time.Sleep(time.Millisecond)
	}

	return cond()
}

func TestTrackBufferPaddingDoesNotHoldFrames(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, losses := newCountingBuffer(&codecs.VP8Packet{}, testLatency, 0)
		defer b.close()

		assert.Empty(t, b.push(vp8Packet(1, 3000, true, false)))
		assert.Equal(t, []uint16{1, 2}, seqNums(b.push(vp8Packet(2, 3000, false, true))))

		padding := &rtp.Packet{Header: rtp.Header{SequenceNumber: 3, Timestamp: 3000, Padding: true}}
		assert.Empty(t, b.push(padding))

		assert.Equal(t, []uint16{4}, seqNums(b.push(vp8Packet(4, 6000, true, true))))

		stats := b.stats()
		assert.Equal(t, uint64(0), stats.PacketsDropped)
		assert.Equal(t, uint64(0), stats.PacketsLost)
		assert.Equal(t, int32(0), losses.Load())
	})
}

func TestTrackBufferReleasesAfterLatencyInOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, losses := newCountingBuffer(&codecs.OpusPacket{}, testLatency, 0)
		defer b.close()

		assert.Equal(t, []uint16{1}, seqNums(b.push(opusPacket(1))))
		// 2 is missing: 3 waits for it.
		assert.Empty(t, b.push(opusPacket(3)))

		time.Sleep(testLatency + time.Millisecond)
		synctest.Wait()

		// Released by the buffer's timer, but only handed out by the next push.
		assert.Equal(t, int32(1), losses.Load())
		assert.Equal(t, []uint16{3, 4}, seqNums(b.push(opusPacket(4))))

		// A packet that arrives after the buffer gave up on it is dropped.
		assert.Empty(t, b.push(opusPacket(2)))
		assert.Equal(t, int32(2), losses.Load())

		stats := b.stats()
		assert.Equal(t, uint64(1), stats.PacketsLost)
		assert.Equal(t, uint64(1), stats.PacketsDropped)
	})
}

// A publisher's pacer spreads a key frame over hundreds of ms. Real clock:
// under synctest, media-sdk v0.1.1 spins on a head that expires on its timer.
func TestTrackBufferPacedKeyframe(t *testing.T) {
	b, losses := newCountingBuffer(&codecs.VP8Packet{}, videoJitterLatency, lossKeyframeInterval)
	defer b.close()

	var ready []*rtp.Packet
	for i := range 14 {
		if i > 0 {
			time.Sleep(36 * time.Millisecond)
		}
		ready = append(ready, b.push(vp8Packet(uint16(1+i), 3000, i == 0, i == 13))...)
	}

	assert.Equal(t, []uint16{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}, seqNums(ready))

	stats := b.stats()
	assert.Equal(t, uint64(1), stats.SamplesPopped)
	assert.Equal(t, uint64(0), stats.PacketsDropped)
	assert.Equal(t, uint64(0), stats.PacketsLost)
	assert.Equal(t, int32(0), losses.Load())
}

func TestJitterLatencyByKind(t *testing.T) {
	video := jitterLatencyFor(TrackKindVideo)
	assert.Greater(t, video, time.Second, "paced key frames")
	assert.Greater(t, video, lossKeyframeInterval, "see lossKeyframeInterval")
	assert.Equal(t, 200*time.Millisecond, jitterLatencyFor(TrackKindAudio))
}

// Paced packets expire on separate timer passes, one every 36 ms.
func TestTrackBufferLossKeyframeInterval(t *testing.T) {
	var mu sync.Mutex
	var calls []time.Time
	b := newTrackBuffer(&codecs.VP8Packet{}, shortLatency, lossKeyframeInterval, func() {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, time.Now())
	})
	defer b.close()

	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(calls)
	}

	// Frame tails over 684 ms: one request.
	start := time.Now()
	for i := range 20 {
		time.Sleep(time.Until(start.Add(time.Duration(i) * 36 * time.Millisecond)))
		b.push(vp8Packet(uint16(1+i), uint32(3000*(1+i)), false, true))
	}
	assert.True(t, eventually(func() bool { return b.stats().PacketsDropped == 20 }))
	if !assert.Equal(t, 1, count()) {
		return
	}

	// A tail more than a second after that request makes the next one.
	mu.Lock()
	first := calls[0]
	mu.Unlock()
	time.Sleep(time.Until(first.Add(1200 * time.Millisecond)))
	b.push(vp8Packet(21, 3000*21, false, true))
	assert.True(t, eventually(func() bool { return b.stats().PacketsDropped == 21 }))
	assert.Equal(t, 2, count())
}

func TestTrackBufferVideoRetransmitInsideWindow(t *testing.T) {
	b, losses := newCountingBuffer(&codecs.VP8Packet{}, testLatency, 0)
	defer b.close()

	assert.Equal(t, []uint16{1, 2}, seqNums(pushAll(b,
		vp8Packet(1, 3000, true, false), vp8Packet(2, 3000, false, true))))
	// 4 is missing: its frame and the frame behind it wait for it.
	assert.Empty(t, pushAll(b,
		vp8Packet(3, 6000, true, false), vp8Packet(5, 6000, false, true), vp8Packet(6, 9000, true, true)))
	assert.Equal(t, []uint16{3, 4, 5, 6}, seqNums(b.push(vp8Packet(4, 6000, false, false))))

	stats := b.stats()
	assert.Equal(t, uint64(0), stats.PacketsDropped)
	assert.Equal(t, uint64(0), stats.PacketsLost)
	assert.Equal(t, int32(0), losses.Load())
}

func TestTrackBufferVideoRetransmitOutsideWindow(t *testing.T) {
	b, losses := newCountingBuffer(&codecs.VP8Packet{}, shortLatency, time.Hour)
	defer b.close()

	assert.Equal(t, []uint16{1, 2}, seqNums(pushAll(b,
		vp8Packet(1, 3000, true, false), vp8Packet(2, 3000, false, true))))
	assert.Empty(t, pushAll(b,
		vp8Packet(3, 6000, true, false), vp8Packet(5, 6000, false, true), vp8Packet(6, 9000, true, true)))

	// The incomplete frame expires and the frame behind it is released.
	assert.True(t, eventually(func() bool { return b.stats().PacketsDropped == 2 }))
	assert.Equal(t, []uint16{6}, seqNums(b.push(vp8Packet(4, 6000, false, false))))

	// PacketsLost is not checked: media-sdk v0.1.1 recycles packet entries
	// without clearing their discont flag, which can hide this gap.
	assert.Equal(t, uint64(3), b.stats().PacketsDropped)
	assert.Equal(t, int32(1), losses.Load())
}

func TestTrackBufferDropsDuplicates(t *testing.T) {
	b, losses := newCountingBuffer(&codecs.VP8Packet{}, testLatency, 0)
	defer b.close()

	assert.Equal(t, []uint16{1, 2}, seqNums(pushAll(b,
		vp8Packet(1, 3000, true, false), vp8Packet(2, 3000, false, true))))
	// A duplicate inside a frame still being assembled.
	assert.Empty(t, pushAll(b,
		vp8Packet(3, 6000, true, false), vp8Packet(3, 6000, true, false), vp8Packet(4, 6000, false, false)))
	assert.Equal(t, []uint16{3, 4, 5}, seqNums(b.push(vp8Packet(5, 6000, false, true))))
	// A duplicate of a packet already handed out.
	assert.Empty(t, b.push(vp8Packet(4, 6000, false, false)))

	stats := b.stats()
	assert.Equal(t, uint64(5), stats.PacketsPushed)
	assert.Equal(t, uint64(0), stats.PacketsDropped)
	assert.Equal(t, uint64(0), stats.PacketsLost)
	assert.Equal(t, int32(0), losses.Load())
}

// A video frame that never completes holds every packet behind it for the
// full latency: at 700 packets/s, a copy of one of them can come 1050
// sequence numbers behind the newest.
func TestTrackBufferDropsDuplicateOfHeldPacketAtHighRate(t *testing.T) {
	b, _ := newCountingBuffer(&codecs.VP8Packet{}, videoJitterLatency, lossKeyframeInterval)
	defer b.close()

	var ready []*rtp.Packet
	start := time.Now()
	// 222 frames of 5 packets; packet 3 never arrives.
	for i := range 222 * 5 {
		time.Sleep(time.Until(start.Add(time.Duration(i) * time.Second / 700)))
		if i == 2 {
			continue
		}
		ready = append(ready, b.push(vp8Packet(uint16(1+i), uint32(3000*(1+i/5)), i%5 == 0, i%5 == 4))...)
	}
	ready = append(ready, b.push(vp8Packet(60, 3000*12, false, true))...)

	time.Sleep(time.Until(start.Add(videoJitterLatency)))
	assert.True(t, eventually(func() bool { return b.stats().SamplesPopped == 221 }))
	ready = append(ready, b.push(vp8Packet(1111, 3000*223, true, true))...)

	want := make([]uint16, 0, 1106)
	for seq := uint16(6); seq <= 1111; seq++ {
		want = append(want, seq)
	}
	assert.Equal(t, want, seqNums(ready))

	stats := b.stats()
	assert.Equal(t, uint64(0), stats.PacketsLost)
	assert.Equal(t, uint64(4), stats.PacketsDropped)
}

func TestTrackBufferMidFrameStart(t *testing.T) {
	b, losses := newCountingBuffer(&codecs.VP8Packet{}, shortLatency, lossKeyframeInterval)
	defer b.close()

	// The tail of a frame whose start was never received, then a key frame.
	assert.Empty(t, pushAll(b,
		vp8Packet(1, 3000, false, false), vp8Packet(2, 3000, false, true),
		vp8Packet(3, 6000, true, false), vp8Packet(4, 6000, false, true)))

	assert.True(t, eventually(func() bool { return b.stats().PacketsDropped == 2 }))
	assert.Equal(t, []uint16{3, 4, 5}, seqNums(b.push(vp8Packet(5, 9000, true, true))))

	assert.Equal(t, uint64(0), b.stats().PacketsLost)
	assert.Equal(t, int32(1), losses.Load())
}

func TestTrackBufferCoalescesLossReports(t *testing.T) {
	for _, tc := range []struct {
		interval time.Duration
		losses   int32
	}{
		{interval: 0, losses: 2},
		{interval: time.Hour, losses: 1},
	} {
		b, losses := newCountingBuffer(&codecs.VP8Packet{}, shortLatency, tc.interval)

		// Two frame tails, dropped on separate timer passes.
		b.push(vp8Packet(1, 3000, false, true))
		assert.True(t, eventually(func() bool { return b.stats().PacketsDropped == 1 }))
		b.push(vp8Packet(2, 6000, false, true))
		assert.True(t, eventually(func() bool { return b.stats().PacketsDropped == 2 }))

		assert.Equal(t, tc.losses, losses.Load(), "interval %v", tc.interval)
		b.close()
	}
}

func TestTrackBufferCloseWithPendingGaps(t *testing.T) {
	before := runtime.NumGoroutine()

	buffers := make([]*trackBuffer, 0, 16)
	for range 16 {
		b, _ := newCountingBuffer(&codecs.VP8Packet{}, shortLatency, 0)
		// An incomplete frame and a frame behind a gap, both waiting on the timer.
		pushAll(b, vp8Packet(1, 3000, true, false), vp8Packet(3, 6000, true, true))
		buffers = append(buffers, b)
	}

	for _, b := range buffers {
		b.close()
	}

	assert.True(t, eventually(func() bool { return runtime.NumGoroutine() <= before }),
		"goroutines: %d before, %d after close", before, runtime.NumGoroutine())

	for _, b := range buffers {
		assert.Equal(t, uint64(2), b.stats().PacketsPushed)
	}
}

func TestTrackBufferFlush(t *testing.T) {
	b, losses := newCountingBuffer(&codecs.VP8Packet{}, videoJitterLatency, lossKeyframeInterval)
	defer b.close()

	assert.Equal(t, []uint16{1, 2}, seqNums(pushAll(b,
		vp8Packet(1, 3000, true, false), vp8Packet(2, 3000, false, true))))
	// 4 never arrives: its frame and the frame behind it are held.
	assert.Empty(t, pushAll(b,
		vp8Packet(3, 6000, true, false), vp8Packet(5, 6000, false, true), vp8Packet(6, 9000, true, true)))

	assert.Equal(t, []uint16{6}, seqNums(b.flush()))
	assert.Equal(t, uint64(2), b.stats().PacketsDropped)
	assert.Equal(t, int32(0), losses.Load())

	// Packets the timer released after the last push come out too.
	a, _ := newCountingBuffer(&codecs.OpusPacket{}, shortLatency, 0)
	defer a.close()

	assert.Equal(t, []uint16{1}, seqNums(a.push(opusPacket(1))))
	assert.Empty(t, a.push(opusPacket(3)))
	assert.True(t, eventually(func() bool { return a.stats().PacketsLost == 1 }))
	assert.Equal(t, []uint16{3}, seqNums(a.flush()))
}

func TestSeqWindow(t *testing.T) {
	var w seqWindow

	assert.False(t, w.duplicate(65534))
	assert.False(t, w.duplicate(65535))
	assert.False(t, w.duplicate(1))
	assert.True(t, w.duplicate(65535), "duplicate across the wrap")
	assert.False(t, w.duplicate(0), "first copy of a packet behind the head")
	assert.True(t, w.duplicate(0))
	assert.True(t, w.duplicate(1))

	assert.False(t, w.duplicate(1+seqWindowSize), "head moves one window ahead")
	assert.False(t, w.duplicate(1), "older than the window: unknown")

	assert.False(t, w.duplicate(40000), "a jump restarts the window")
	assert.True(t, w.duplicate(40000))
	assert.False(t, w.duplicate(1+seqWindowSize), "so does a jump back")
}
