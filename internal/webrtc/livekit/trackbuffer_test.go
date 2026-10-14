package livekit

import (
	"cmp"
	"slices"
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
	// A gap's request comes no earlier than gapKeyframeDelay, which leaves
	// the NACKs sent before it their retransmits, and on a read soon after.
	gapRequestMin = 200 * time.Millisecond
	gapRequestMax = 260 * time.Millisecond
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

// A publisher's pacer spreads a key frame over hundreds of ms.
func TestTrackBufferPacedKeyframe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
	})
}

func TestJitterLatencyByKind(t *testing.T) {
	video := jitterLatencyFor(TrackKindVideo)
	assert.Greater(t, video, time.Second, "paced key frames")
	assert.Greater(t, video, lossKeyframeInterval, "see lossKeyframeInterval")
	assert.Equal(t, 200*time.Millisecond, jitterLatencyFor(TrackKindAudio))
}

// Paced packets expire on separate timer passes, one every 36 ms.
func TestTrackBufferLossKeyframeInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var log lossLog
		b := newVideoBuffer(&log)
		defer b.close()

		start := time.Now()
		// Frame tails over 684 ms: one request.
		for i := range 20 {
			if i > 0 {
				time.Sleep(36 * time.Millisecond)
			}
			b.push(vp8Packet(uint16(1+i), uint32(3000*(1+i)), false, true))
		}
		// A tail expiring 1.2 s after that request makes the next one.
		time.Sleep(time.Until(start.Add(1200 * time.Millisecond)))
		b.push(vp8Packet(21, 3000*21, false, true))

		time.Sleep(time.Until(start.Add(684*time.Millisecond + videoJitterLatency)))
		synctest.Wait()
		assert.Equal(t, uint64(20), b.stats().PacketsDropped)
		if !assert.Len(t, log.times(), 1) {
			return
		}

		time.Sleep(time.Until(start.Add(1200*time.Millisecond + videoJitterLatency)))
		synctest.Wait()
		assert.Equal(t, uint64(21), b.stats().PacketsDropped)
		assert.Len(t, log.times(), 2)
	})
}

func TestTrackBufferVideoRetransmitInsideWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
	})
}

func TestTrackBufferVideoRetransmitOutsideWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, losses := newCountingBuffer(&codecs.VP8Packet{}, videoJitterLatency, time.Hour)
		defer b.close()

		assert.Equal(t, []uint16{1, 2}, seqNums(pushAll(b,
			vp8Packet(1, 3000, true, false), vp8Packet(2, 3000, false, true))))
		assert.Empty(t, pushAll(b,
			vp8Packet(3, 6000, true, false), vp8Packet(5, 6000, false, true), vp8Packet(6, 9000, true, true)))

		// The incomplete frame expires and the frame behind it is released.
		time.Sleep(videoJitterLatency)
		synctest.Wait()
		assert.Equal(t, uint64(2), b.stats().PacketsDropped)
		assert.Equal(t, []uint16{6}, seqNums(b.push(vp8Packet(4, 6000, false, false))))

		stats := b.stats()
		assert.Equal(t, uint64(3), stats.PacketsDropped)
		assert.Equal(t, uint64(1), stats.PacketsLost)
		assert.Equal(t, int32(1), losses.Load())
	})
}

func TestTrackBufferRetransmitIntoRecycledEntry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, losses := newCountingBuffer(&codecs.VP8Packet{}, videoJitterLatency, lossKeyframeInterval)
		defer b.close()

		frame := func(seq uint16) *rtp.Packet { return vp8Packet(seq, 3000*uint32(seq), true, true) }

		assert.Equal(t, []uint16{1, 2, 3, 4}, seqNums(pushAll(b,
			vp8Packet(1, 3000, true, false), vp8Packet(2, 3000, false, false), vp8Packet(3, 3000, false, true),
			frame(4))))
		// 5 and 8 are missing. 6, 7 and 9 take the entries 1-4 left, one of
		// which held the packet that started the stream.
		assert.Empty(t, pushAll(b, frame(6), frame(7), frame(9)))

		time.Sleep(60 * time.Millisecond)
		assert.Equal(t, []uint16{5, 6, 7}, seqNums(b.push(frame(5))))
		assert.Equal(t, []uint16{8, 9}, seqNums(b.push(frame(8))))

		stats := b.stats()
		assert.Equal(t, uint64(0), stats.PacketsDropped)
		assert.Equal(t, uint64(0), stats.PacketsLost)
		assert.Equal(t, int32(0), losses.Load())
	})
}

func TestTrackBufferDropsDuplicates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
	})
}

// A video frame that never completes holds every packet behind it for the
// full latency: at 700 packets/s, a copy of one of them can come 1050
// sequence numbers behind the newest.
func TestTrackBufferDropsDuplicateOfHeldPacketAtHighRate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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

		// Frame 1's packets expire one by one; the last arrived at 4/700 s.
		time.Sleep(time.Until(start.Add(4*time.Second/700 + videoJitterLatency)))
		synctest.Wait()
		assert.Equal(t, uint64(221), b.stats().SamplesPopped)
		ready = append(ready, b.push(vp8Packet(1111, 3000*223, true, true))...)

		want := make([]uint16, 0, 1106)
		for seq := uint16(6); seq <= 1111; seq++ {
			want = append(want, seq)
		}
		assert.Equal(t, want, seqNums(ready))

		stats := b.stats()
		assert.Equal(t, uint64(0), stats.PacketsLost)
		assert.Equal(t, uint64(4), stats.PacketsDropped)
	})
}

func TestTrackBufferMidFrameStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, losses := newCountingBuffer(&codecs.VP8Packet{}, videoJitterLatency, lossKeyframeInterval)
		defer b.close()

		// The tail of a frame whose start was never received, then a key frame.
		assert.Empty(t, pushAll(b,
			vp8Packet(1, 3000, false, false), vp8Packet(2, 3000, false, true),
			vp8Packet(3, 6000, true, false), vp8Packet(4, 6000, false, true)))

		time.Sleep(videoJitterLatency)
		synctest.Wait()
		assert.Equal(t, uint64(2), b.stats().PacketsDropped)
		assert.Equal(t, []uint16{3, 4, 5}, seqNums(b.push(vp8Packet(5, 9000, true, true))))

		assert.Equal(t, uint64(0), b.stats().PacketsLost)
		assert.Equal(t, int32(1), losses.Load())
	})
}

func TestTrackBufferCoalescesLossReports(t *testing.T) {
	for _, tc := range []struct {
		interval time.Duration
		losses   int32
	}{
		{interval: 0, losses: 2},
		{interval: time.Hour, losses: 1},
	} {
		synctest.Test(t, func(t *testing.T) {
			b, losses := newCountingBuffer(&codecs.VP8Packet{}, videoJitterLatency, tc.interval)
			defer b.close()

			// Two frame tails, dropped on separate timer passes.
			b.push(vp8Packet(1, 3000, false, true))
			time.Sleep(videoJitterLatency)
			synctest.Wait()
			assert.Equal(t, uint64(1), b.stats().PacketsDropped)
			b.push(vp8Packet(2, 6000, false, true))
			time.Sleep(videoJitterLatency)
			synctest.Wait()
			assert.Equal(t, uint64(2), b.stats().PacketsDropped)

			assert.Equal(t, tc.losses, losses.Load(), "interval %v", tc.interval)
		})
	}
}

func TestTrackBufferCloseWithPendingGaps(t *testing.T) {
	// The bubble fails the test if a buffer's timer goroutine outlives close.
	synctest.Test(t, func(t *testing.T) {
		buffers := make([]*trackBuffer, 0, 16)
		for range 16 {
			b, _ := newCountingBuffer(&codecs.VP8Packet{}, videoJitterLatency, 0)
			// An incomplete frame and a frame behind a gap, both waiting on the timer.
			pushAll(b, vp8Packet(1, 3000, true, false), vp8Packet(3, 6000, true, true))
			buffers = append(buffers, b)
		}

		for _, b := range buffers {
			b.close()
		}

		for _, b := range buffers {
			assert.Equal(t, uint64(2), b.stats().PacketsPushed)
		}
	})
}

func TestTrackBufferFlush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		a, _ := newCountingBuffer(&codecs.OpusPacket{}, audioJitterLatency, 0)
		defer a.close()

		assert.Equal(t, []uint16{1}, seqNums(a.push(opusPacket(1))))
		assert.Empty(t, a.push(opusPacket(3)))
		time.Sleep(audioJitterLatency)
		synctest.Wait()
		assert.Equal(t, uint64(1), a.stats().PacketsLost)
		assert.Equal(t, []uint16{3}, seqNums(a.flush()))
	})
}

type lossLog struct {
	mu sync.Mutex
	at []time.Time
}

func (l *lossLog) add() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.at = append(l.at, time.Now())
}

func (l *lossLog) times() []time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.at)
}

type arrival struct {
	at  time.Duration
	pkt *rtp.Packet
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// frames is n single-packet VP8 frames from seq0, frame i sent at i*10 ms.
// The lost frames never arrive on time; late[i] adds a copy of frame i at
// that offset: a retransmit if i is lost, a duplicate otherwise.
func frames(seq0 uint16, n int, lost []int, late map[int]time.Duration) []arrival {
	var out []arrival
	for i := range n {
		pkt := func() *rtp.Packet { return vp8Packet(seq0+uint16(i), uint32(3000*(i+1)), true, true) }
		if !slices.Contains(lost, i) {
			out = append(out, arrival{ms(10 * i), pkt()})
		}
		if at, ok := late[i]; ok {
			out = append(out, arrival{at, pkt()})
		}
	}

	return out
}

// replay pushes each packet at its offset from start, and returns what the
// buffer handed out. A timer that fires at a push's instant runs first.
func replay(b *trackBuffer, start time.Time, arrivals []arrival) []uint16 {
	slices.SortStableFunc(arrivals, func(x, y arrival) int { return cmp.Compare(x.at, y.at) })

	var out []uint16
	for _, a := range arrivals {
		time.Sleep(time.Until(start.Add(a.at)))
		synctest.Wait()
		out = append(out, seqNums(b.push(a.pkt))...)
	}

	return out
}

func newVideoBuffer(log *lossLog) *trackBuffer {
	return newTrackBuffer(&codecs.VP8Packet{}, videoJitterLatency, lossKeyframeInterval, log.add)
}

func assertGapRequest(t *testing.T, call, gap time.Time) {
	t.Helper()
	assert.GreaterOrEqual(t, call.Sub(gap), gapRequestMin)
	assert.Less(t, call.Sub(gap), gapRequestMax)
}

func TestTrackBufferRequestsKeyframeForOpenGap(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seq0  uint16
		lost  []int
		late  map[int]time.Duration
		gapAt time.Duration // the first packet past the gap
	}{
		{name: "unrepaired", seq0: 100, lost: []int{10}, gapAt: ms(110)},
		{name: "burst, one never retransmitted", seq0: 100, lost: []int{10, 11, 12},
			late: map[int]time.Duration{10: ms(250), 12: ms(260)}, gapAt: ms(130)},
		{name: "duplicate behind the gap", seq0: 100, lost: []int{10},
			late: map[int]time.Duration{11: ms(160)}, gapAt: ms(110)},
		// 65535 is retransmitted, 0 is not.
		{name: "across the wrap", seq0: 65530, lost: []int{5, 6},
			late: map[int]time.Duration{5: ms(200)}, gapAt: ms(70)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var log lossLog
				b := newVideoBuffer(&log)
				defer b.close()

				start := time.Now()
				replay(b, start, frames(tc.seq0, 60, tc.lost, tc.late))

				calls := log.times()
				if assert.Len(t, calls, 1) {
					assertGapRequest(t, calls[0], start.Add(tc.gapAt))
				}
			})
		})
	}
}

func TestTrackBufferNoKeyframeRequestWithoutOpenGap(t *testing.T) {
	// A 14-packet key frame paced over 468 ms, then 100 fps frames.
	var paced []arrival
	for i := range 14 {
		paced = append(paced, arrival{ms(36 * i), vp8Packet(uint16(1+i), 3000, i == 0, i == 13)})
	}
	for i := range 30 {
		paced = append(paced, arrival{ms(478 + 10*i), vp8Packet(uint16(15+i), uint32(3000*(2+i)), true, true)})
	}

	// The jitter buffer starts a new stream on a jump of exactly this size.
	reset := frames(100, 15, nil, nil)
	for _, a := range frames(114+seqResetDistance, 60, nil, nil) {
		a.at += ms(150)
		reset = append(reset, a)
	}

	for _, tc := range []struct {
		name     string
		arrivals []arrival
	}{
		// 150 and 190 ms after the originals.
		{"retransmits", frames(100, 70, []int{10, 30}, map[int]time.Duration{10: ms(250), 30: ms(490)})},
		{"retransmitted burst", frames(100, 50, []int{10, 11, 12, 13, 14},
			map[int]time.Duration{10: ms(250), 11: ms(251), 12: ms(252), 13: ms(253), 14: ms(254)})},
		{"reordered", frames(100, 50, []int{10, 20}, map[int]time.Duration{10: ms(140), 20: ms(215)})},
		// An RTX probe resends the last 6 packets.
		{"duplicates", frames(100, 50, nil,
			map[int]time.Duration{15: ms(201), 16: ms(201), 17: ms(202), 18: ms(202), 19: ms(203), 20: ms(203)})},
		{"paced key frame", paced},
		{"sequence reset", reset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var log lossLog
				b := newVideoBuffer(&log)
				defer b.close()

				want := make([]uint16, 0, len(tc.arrivals))
				for _, a := range tc.arrivals {
					want = append(want, a.pkt.SequenceNumber)
				}
				slices.Sort(want)
				want = slices.Compact(want)

				assert.Equal(t, want, replay(b, time.Now(), tc.arrivals))
				assert.Empty(t, log.times())

				stats := b.stats()
				assert.Equal(t, uint64(0), stats.PacketsLost)
				assert.Equal(t, uint64(0), stats.PacketsDropped)
			})
		})
	}
}

// bursts is n frames of 8 packets from seq 1000, frame f read at f*interval
// and its packets 1 ms apart, the way a low-rate camera or screen share
// arrives. Packet 2 of frame 2 is lost, and packet 3 reveals it.
func bursts(n int, interval time.Duration) (arrivals []arrival, reveal time.Duration) {
	for f := range n {
		for p := range 8 {
			if f == 2 && p == 2 {
				continue
			}
			pkt := vp8Packet(1000+uint16(8*f+p), uint32(3000*(f+1)), p == 0, p == 7)
			arrivals = append(arrivals, arrival{time.Duration(f)*interval + ms(p), pkt})
		}
	}

	return arrivals, 2*interval + ms(3)
}

// server-sdk-go NACKs a gap on the first read 20 ms after it opens, at a low
// frame rate the next frame's first packet. The retransmit lands a 70 ms
// round trip later, in band, or over RTX, which pion hands out right after
// the next packet on the main stream. Neither may bring a request.
func TestTrackBufferLowFrameRateRetransmit(t *testing.T) {
	for _, tc := range []struct {
		fps      string
		interval time.Duration
		// When the request for a loss that is never repaired goes out, after
		// the packet that reveals it.
		request [2]time.Duration
	}{
		// The second packet of the frame after the NACK's.
		{"3.4", ms(294), [2]time.Duration{ms(580), ms(620)}},
		{"5", ms(200), [2]time.Duration{ms(390), ms(430)}},
		// The first packet 200 ms after the reveal.
		{"15", ms(67), [2]time.Duration{ms(200), ms(240)}},
		{"30", ms(33), [2]time.Duration{ms(200), ms(240)}},
	} {
		for _, repair := range []string{"in band", "RTX", "none"} {
			t.Run(tc.fps+" fps, "+repair, func(t *testing.T) {
				t.Parallel()

				n := int((tc.request[1]+2*tc.interval)/tc.interval) + 3
				arrivals, reveal := bursts(n, tc.interval)
				nack := time.Duration(0)
				for _, a := range arrivals {
					if a.at >= reveal+20*time.Millisecond {
						nack = a.at
						break
					}
				}

				lost := vp8Packet(1000+8*2+2, 3000*3, false, false)
				switch landed := nack + 70*time.Millisecond; repair {
				case "in band":
					arrivals = append(arrivals, arrival{landed, lost})
				case "RTX":
					// Appended last, so replay's stable sort keeps it after
					// the main packet read at the same offset.
					for _, a := range arrivals {
						if a.at >= landed {
							arrivals = append(arrivals, arrival{a.at, lost})
							break
						}
					}
				}

				synctest.Test(t, func(t *testing.T) {
					var log lossLog
					b := newVideoBuffer(&log)
					defer b.close()

					start := time.Now()
					replay(b, start, arrivals)

					calls := log.times()
					if repair != "none" {
						assert.Empty(t, calls)
						assert.Equal(t, uint64(0), b.stats().PacketsLost)
						assert.Equal(t, uint64(0), b.stats().PacketsDropped)

						return
					}

					if assert.Len(t, calls, 1) {
						assert.GreaterOrEqual(t, calls[0].Sub(start.Add(reveal)), tc.request[0])
						assert.Less(t, calls[0].Sub(start.Add(reveal)), tc.request[1])
					}
				})
			})
		}
	}
}

// The jitter buffer reports the gap again when it gives up on it, 2 s after
// it opened. A second gap waits for the 1 s limit, not for that.
func TestTrackBufferGapRequestThenExpiry(t *testing.T) {
	t.Run("one gap", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var log lossLog
			b := newVideoBuffer(&log)
			defer b.close()

			start := time.Now()
			replay(b, start, frames(100, 240, []int{10}, nil))

			calls := log.times()
			if assert.Len(t, calls, 1) {
				assertGapRequest(t, calls[0], start.Add(ms(110)))
			}
			assert.Equal(t, uint64(1), b.stats().PacketsLost, "the jitter buffer gave up on it")
		})
	})

	// A gap that the request's own push reveals expires a timer wake-up past
	// the request's latency, for a packet that arrived before the request.
	t.Run("gap revealed by the request push", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var log lossLog
			b := newVideoBuffer(&log)
			defer b.close()

			start := time.Now()
			// Frame 32, at 320 ms, sends the request for 10 and reveals 31.
			replay(b, start, frames(100, 60, []int{10, 31}, nil))
			time.Sleep(time.Until(start.Add(videoJitterLatency + 600*time.Millisecond)))
			synctest.Wait()

			calls := log.times()
			if assert.Len(t, calls, 1) {
				assertGapRequest(t, calls[0], start.Add(ms(110)))
			}
			assert.Equal(t, uint64(2), b.stats().PacketsLost, "the jitter buffer gave up on both")
		})
	})

	t.Run("second gap within the limit", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var log lossLog
			b := newVideoBuffer(&log)
			defer b.close()

			start := time.Now()
			replay(b, start, frames(100, 150, []int{10, 50}, nil))

			calls := log.times()
			if assert.Len(t, calls, 2) {
				assertGapRequest(t, calls[0], start.Add(ms(110)))
				assert.GreaterOrEqual(t, calls[1].Sub(calls[0]), lossKeyframeInterval)
				assert.Less(t, calls[1].Sub(calls[0]), lossKeyframeInterval+60*time.Millisecond)
			}
		})
	})
}

// A request that no key frame follows is repeated, under the same limit. A
// key frame answers it from its first packet: at a low frame rate a paced
// one can still be arriving when the next request would go out.
func TestTrackBufferRepeatsUnansweredRequest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   bool
		calls int
	}{
		{"no key frame", false, 2},
		{"key frame paced over 1.2 s", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// 3.4 fps. Packet 1 of frame 2 is lost, and the request goes out
				// at frame 4, at about 1180 ms. Frame 5 starts 290 ms later and
				// takes 1.2 s.
				var arrivals []arrival
				for f := range 8 {
					for j, pkt := range writerFrame(0, uint16(1000+4*f), uint16(100+f), f, f == 0 || (f == 5 && tc.key)) {
						at := ms(294*f + j)
						switch {
						case f == 2 && j == 1:
							continue
						case f == 5:
							at = ms(1470 + 400*j)
						case f > 5:
							at = ms(2680 + 294*(f-6) + j)
						}
						arrivals = append(arrivals, arrival{at, pkt})
					}
				}

				var log lossLog
				b := newVideoBuffer(&log)
				defer b.close()

				replay(b, time.Now(), arrivals)

				calls := log.times()
				if assert.Len(t, calls, tc.calls) && tc.calls > 1 {
					assert.GreaterOrEqual(t, calls[1].Sub(calls[0]), lossKeyframeInterval)
				}
			})
		})
	}
}

// Only a key frame that starts after the gaps a request covered answers it,
// not a late retransmit of an earlier key frame's first packet.
func TestTrackBufferRepeatIgnoresEarlierKeyframe(t *testing.T) {
	for _, tc := range []struct {
		name string
		late bool
	}{
		{"earlier key frame start never arrives", false},
		{"earlier key frame start arrives late", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// 3.4 fps. Frame 1 is a key frame whose first packet comes at
				// 1000 ms or never; packet 1 of frame 2 never comes.
				var arrivals []arrival
				for f := range 8 {
					for j, pkt := range writerFrame(0, uint16(1000+4*f), uint16(100+f), f, f <= 1) {
						switch {
						case f == 1 && j == 0:
							if tc.late {
								arrivals = append(arrivals, arrival{ms(1000), pkt})
							}

							continue
						case f == 2 && j == 1:
							continue
						}
						arrivals = append(arrivals, arrival{ms(294*f + j), pkt})
					}
				}

				var log lossLog
				b := newVideoBuffer(&log)
				defer b.close()

				replay(b, time.Now(), arrivals)

				assert.Len(t, log.times(), 2)
			})
		})
	}
}

// A jump past the discontinuity distance starts a new stream: a gap before
// it is dropped, and the new stream's gaps are timed from scratch.
func TestTrackBufferGapRequestAfterSeqReset(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var log lossLog
		b := newVideoBuffer(&log)
		defer b.close()

		arrivals := frames(100, 15, []int{10}, nil)
		for _, a := range frames(10150, 80, []int{50}, nil) {
			a.at += ms(150)
			arrivals = append(arrivals, a)
		}

		start := time.Now()
		replay(b, start, arrivals)

		calls := log.times()
		if assert.Len(t, calls, 1) {
			assertGapRequest(t, calls[0], start.Add(ms(150+510)))
		}
	})
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

	head := uint16(1 + seqWindowSize)
	assert.False(t, w.duplicate(head-seqResetDistance), "so does one of exactly the reset distance")
	assert.False(t, w.duplicate(head), "which forgets the old head")
}

func TestSeqGaps(t *testing.T) {
	var g seqGaps
	t0 := time.Now()
	at := func(n int) time.Time { return t0.Add(ms(n)) }
	var none time.Time

	g.add(65533, at(0))
	g.add(1, at(10))
	g.add(65535, at(20))
	g.read(at(29))
	assert.True(t, g.open[0].nacked.IsZero(), "the first NACK waits 20 ms")
	g.read(at(30))
	assert.Equal(t, at(30), g.open[0].nacked)
	g.read(at(129))
	g.read(at(210))
	assert.False(t, g.due(at(210), none), "no read 100 ms after the NACK before this one")
	g.read(at(211))
	assert.True(t, g.due(at(211), none), "65534 and 0 still missing")
	g.add(0, at(220))
	g.add(65534, at(230))
	g.read(at(400))
	assert.False(t, g.due(at(400), none), "all retransmitted")

	// At a low frame rate one read sends the first NACK, and the next is
	// the first past its round trip.
	g.add(10, at(1000))
	g.read(at(1300))
	g.read(at(1600))
	assert.False(t, g.due(at(1600), none), "an RTX retransmit may come right after this read")
	g.read(at(1601))
	assert.True(t, g.due(at(1601), none))
	assert.False(t, g.due(at(1601), at(1000)), "a request at 1000 ms covers it")
	assert.Empty(t, g.open)

	g.add(12, at(2000))
	g.add(20000, at(2010))
	g.read(at(2300))
	g.read(at(2400))
	assert.False(t, g.due(at(2400), none), "a jump drops the open gaps")

	for _, jump := range []int{seqResetDistance, -seqResetDistance} {
		var r seqGaps
		r.add(100, at(0))
		r.add(102, at(1))
		r.add(uint16(102+jump), at(2))
		assert.Empty(t, r.open, "a jump of exactly %d drops the open gaps", jump)
	}

	// Every other packet lost: the gaps past the limit merge into the last.
	for i := range maxOpenGaps + 2 {
		g.add(20000+uint16(2*(i+1)), at(100+i))
	}
	if assert.Len(t, g.open, maxOpenGaps) {
		last := g.open[maxOpenGaps-1]
		assert.Equal(t, at(100+maxOpenGaps-1), last.since)
		assert.Equal(t, 3, last.missing)
	}
	for i := range maxOpenGaps + 2 {
		g.add(20000+uint16(2*i+1), at(200))
	}
	assert.Empty(t, g.open)
}
