package livekit

import (
	"sync"
	"time"

	"github.com/livekit/media-sdk/jitter"
	"github.com/pion/rtp"
)

const (
	seqWindowSize = 2048
	// The jitter buffer treats a packet this far from the previous one as a
	// stream discontinuity, so the duplicate window restarts there too.
	seqResetDistance = 3000
)

// trackBuffer runs a track's packets through a jitter buffer and returns the
// released packets to the caller of push. The buffer also releases samples
// from its own timer goroutine; those are held until the next push, so the
// recorder is only ever fed from the track's read loop.
type trackBuffer struct {
	jb           *jitter.Buffer
	seen         seqWindow
	lossInterval time.Duration

	mu       sync.Mutex
	ready    []*rtp.Packet
	lastLoss time.Time
	ended    bool
}

// newTrackBuffer calls onLoss at most once per lossInterval.
func newTrackBuffer(depacketizer rtp.Depacketizer, latency, lossInterval time.Duration, onLoss func()) *trackBuffer {
	b := &trackBuffer{lossInterval: lossInterval}
	b.jb = jitter.NewBuffer(
		paddingDepacketizer{depacketizer},
		latency,
		b.enqueue,
		jitter.WithPacketLossHandler(func(_, _ uint64) {
			if b.lossDue() {
				onLoss()
			}
		}),
		// TODO: plug logger via jitter.WithLogger(logger)
	)

	return b
}

// push adds pkt to the jitter buffer and returns, in order, every packet the
// buffer has released since the previous call.
func (b *trackBuffer) push(pkt *rtp.Packet) []*rtp.Packet {
	// The jitter buffer keeps duplicates, and popping the second copy
	// underflows its loss count by 65535. RTX probes resend recent packets.
	if !b.seen.duplicate(pkt.SequenceNumber) {
		// media-sdk v0.1.1 stamps Push with mono.Now(), which has no
		// monotonic reading, but computes expiry from time.Now(), so a
		// wall-clock step would shift every expiry. Go back to jb.Push once
		// the pin includes 1373fc1, which moves expiry to mono.Now().
		b.jb.PushAt(pkt, time.Now())
	}

	return b.takeReady()
}

// flush is push for a track that has ended: it also releases the complete
// frames the jitter buffer holds and drops the rest. A frame it drops
// requests no keyframe, since none would arrive.
func (b *trackBuffer) flush() []*rtp.Packet {
	b.mu.Lock()
	b.ended = true
	b.mu.Unlock()

	b.jb.Flush()

	return b.takeReady()
}

func (b *trackBuffer) takeReady() []*rtp.Packet {
	b.mu.Lock()
	defer b.mu.Unlock()

	ready := b.ready
	b.ready = nil

	return ready
}

// enqueue runs with the jitter buffer's lock held, on the pushing goroutine
// or on the buffer's timer goroutine.
func (b *trackBuffer) enqueue(sample []jitter.ExtPacket) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, p := range sample {
		if len(p.Payload) > 0 {
			b.ready = append(b.ready, p.Packet)
		}
	}
}

// lossDue rate-limits the buffer's loss reports, which repeat once per timer
// pass while a frame's packets expire, and again for the gap.
func (b *trackBuffer) lossDue() bool {
	now := time.Now()

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.ended || (!b.lastLoss.IsZero() && now.Sub(b.lastLoss) < b.lossInterval) {
		return false
	}

	b.lastLoss = now

	return true
}

func (b *trackBuffer) stats() *jitter.BufferStats {
	return b.jb.Stats()
}

func (b *trackBuffer) close() {
	b.jb.Close()
}

// seqWindow records which of the last seqWindowSize sequence numbers were
// seen. Only the pushing goroutine uses it.
type seqWindow struct {
	started bool
	head    uint16
	bits    [seqWindowSize / 64]uint64
}

// duplicate records seq and reports whether it had been seen already.
func (w *seqWindow) duplicate(seq uint16) bool {
	dist := int(int16(seq - w.head))

	switch {
	case !w.started || dist > seqResetDistance || dist < -seqResetDistance:
		w.bits = [seqWindowSize / 64]uint64{}
		w.started = true
		w.head = seq
	case dist > 0:
		for i := 1; i <= dist && i <= seqWindowSize; i++ {
			w.clear(w.head + uint16(i))
		}
		w.head = seq
	case -dist >= seqWindowSize:
		// Too old to tell. The window must cover the video latency times the
		// packet rate, 1024 packets/s at 2 s, so the buffer drops it as late.
		return false
	case w.has(seq):
		return true
	}

	w.set(seq)

	return false
}

func (w *seqWindow) has(seq uint16) bool {
	i := seq % seqWindowSize
	return w.bits[i/64]&(1<<(i%64)) != 0
}

func (w *seqWindow) set(seq uint16) {
	i := seq % seqWindowSize
	w.bits[i/64] |= 1 << (i % 64)
}

func (w *seqWindow) clear(seq uint16) {
	i := seq % seqWindowSize
	w.bits[i/64] &^= 1 << (i % 64)
}

// paddingDepacketizer makes a packet with no payload a sample of its own. An
// empty VP8 payload is not a partition head, so the jitter buffer would hold
// the frames behind a padding-only packet for a full latency window, then
// drop the padding and report a loss, which requests a keyframe.
type paddingDepacketizer struct {
	rtp.Depacketizer
}

func (d paddingDepacketizer) IsPartitionHead(payload []byte) bool {
	return len(payload) == 0 || d.Depacketizer.IsPartitionHead(payload)
}

func (d paddingDepacketizer) IsPartitionTail(marker bool, payload []byte) bool {
	return len(payload) == 0 || d.Depacketizer.IsPartitionTail(marker, payload)
}
