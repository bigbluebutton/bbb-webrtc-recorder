package livekit

import (
	"slices"
	"sync"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/livekit/media-sdk/jitter"
	"github.com/pion/rtp"
)

const (
	seqWindowSize = 2048
	// The jitter buffer treats a jump of this size or more as a stream
	// discontinuity, so the duplicate window restarts there too.
	seqResetDistance = 3000
	// The age at which an open gap gets its keyframe request: time for its
	// first NACKs' retransmits, while the freeze stays at a few frames.
	// LiveKit still answers the NACKs sent before the request; it ignores
	// only those that reach it after the PLI.
	gapKeyframeDelay = 200 * time.Millisecond
	// server-sdk-go sends NACKs only while it reads a packet, a gap's first
	// on the first read this long after the gap opens.
	gapFirstNACK = 20 * time.Millisecond
	// The round trip a gap waits for after its first NACK, with margin over
	// the 70 ms the SDK's NACK queue assumes until the server reports one.
	gapRepairWindow = 100 * time.Millisecond
	// A gap stays open until a request or a retransmit, about a second at
	// most; this bounds the scan for each late packet.
	maxOpenGaps = 64
	// The jitter buffer reports a loss once its timer fires and it gets its
	// lock, a little after the expiry. Far shorter than a key frame's round
	// trip, so what arrives within it still precedes the key frame.
	lossReportSlack = 20 * time.Millisecond
)

// trackBuffer runs a track's packets through a jitter buffer and returns the
// released packets to the caller of push. The buffer also releases samples
// from its own timer goroutine; those are held until the next push, so the
// recorder is only ever fed from the track's read loop.
type trackBuffer struct {
	jb           *jitter.Buffer
	seen         seqWindow
	gaps         *seqGaps // nil when the jitter buffer gives up on a gap by gapKeyframeDelay
	latency      time.Duration
	lossInterval time.Duration
	onLoss       func()

	// Only the pushing goroutine uses these.
	keyframe    uint16 // the newest key frame's first packet
	hasKeyframe bool
	requested   uint16 // the last packet missing from the newest gap the last early request covered
	pending     bool   // that request has no key frame after it yet

	mu        sync.Mutex
	ready     []jitter.ExtPacket
	lastLoss  time.Time
	lastEarly time.Time
	ended     bool
}

// newTrackBuffer calls onLoss at most once per lossInterval: when the jitter
// buffer gives up on packets, or earlier, once a sequence gap outlives both
// gapKeyframeDelay and its first NACK's round trip, and again while no key
// frame follows that gap.
func newTrackBuffer(depacketizer rtp.Depacketizer, latency, lossInterval time.Duration, onLoss func()) *trackBuffer {
	b := &trackBuffer{latency: latency, lossInterval: lossInterval, onLoss: onLoss}
	if gapKeyframeDelay < latency {
		b.gaps = &seqGaps{}
	}

	b.jb = jitter.NewBuffer(
		paddingDepacketizer{depacketizer},
		latency,
		b.enqueue,
		jitter.WithPacketLossHandler(func(_, _ uint64) {
			if b.lossDue(false) {
				onLoss()
			}
		}),
		// TODO: plug logger via jitter.WithLogger(logger)
	)

	return b
}

// push is pushExt for a packet that arrives now, without the arrivals.
func (b *trackBuffer) push(pkt *rtp.Packet) []*rtp.Packet {
	return packetsOf(b.pushExt(pkt, time.Now()))
}

// pushExt adds pkt, which arrived at now, to the jitter buffer. It returns,
// in order and with their arrivals, the packets released since the last call.
func (b *trackBuffer) pushExt(pkt *rtp.Packet, now time.Time) []jitter.ExtPacket {
	if b.gaps != nil {
		b.gaps.read(now)
	}

	// The jitter buffer keeps duplicates, and popping the second copy
	// underflows its loss count by 65535. RTX probes resend recent packets.
	if !b.seen.duplicate(pkt.SequenceNumber) {
		if b.gaps != nil {
			b.gaps.add(pkt.SequenceNumber, now)

			// The first packet: a paced key frame can still be arriving when
			// a request would be due again.
			if recorder.IsVP8KeyFrame(pkt) && (!b.hasKeyframe || seqNewer(pkt.SequenceNumber, b.keyframe)) {
				b.keyframe, b.hasKeyframe = pkt.SequenceNumber, true
			}
		}

		// media-sdk v0.1.1 stamps Push with mono.Now(), which has no
		// monotonic reading, but computes expiry from time.Now(), so a
		// wall-clock step would shift every expiry. Go back to jb.Push once
		// the pin includes 1373fc1, which moves expiry to mono.Now().
		b.jb.PushAt(pkt, now)
	}

	if b.gaps != nil && b.gapDue(now) {
		b.onLoss()
	}

	return b.takeReady()
}

// gapDue reports whether a gap is due for a request, or the last early
// request is due again, and takes the loss report if so.
func (b *trackBuffer) gapDue(now time.Time) bool {
	b.mu.Lock()
	// A request brings a key frame that follows every gap already open, and
	// the jitter buffer reports the gaps it gives up on itself.
	settled := b.lastLoss
	b.mu.Unlock()

	if expired := now.Add(-b.latency); expired.After(settled) {
		settled = expired
	}

	// LiveKit drops a request that comes within its PLI throttle of another
	// subscriber's, whose key frame may have come before the gap. A late
	// retransmit of that key frame's first packet does not answer it.
	if b.pending && b.hasKeyframe && seqNewer(b.keyframe, b.requested) {
		b.pending = false
	}

	if !b.gaps.due(now, settled) && !b.pending {
		return false
	}

	if !b.lossDue(true) {
		return false
	}

	if n := len(b.gaps.open); n > 0 {
		gap := b.gaps.open[n-1]
		b.requested, b.pending = gap.first+gap.span, true
	}

	return true
}

// seqNewer reports whether seq comes after ref, taking a jump of
// seqResetDistance or more either way as a new stream, as the jitter buffer
// does.
func seqNewer(seq, ref uint16) bool {
	d := int(int16(seq - ref))

	return d > 0 || d <= -seqResetDistance
}

// flush is push for a track that has ended: it also releases the complete
// frames the jitter buffer holds and drops the rest. A frame it drops
// requests no keyframe, since none would arrive.
func (b *trackBuffer) flush() []*rtp.Packet {
	return packetsOf(b.flushExt())
}

// flushExt is flush, with each packet's arrival.
func (b *trackBuffer) flushExt() []jitter.ExtPacket {
	b.mu.Lock()
	b.ended = true
	b.mu.Unlock()

	b.jb.Flush()

	return b.takeReady()
}

func packetsOf(ext []jitter.ExtPacket) []*rtp.Packet {
	packets := make([]*rtp.Packet, 0, len(ext))
	for _, p := range ext {
		packets = append(packets, p.Packet)
	}

	return packets
}

func (b *trackBuffer) takeReady() []jitter.ExtPacket {
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
			b.ready = append(b.ready, p)
		}
	}
}

// lossDue rate-limits the loss reports: the jitter buffer's, which repeat
// once per timer pass while a frame's packets expire, and again for the gap,
// and the early ones for a gap that stays open.
func (b *trackBuffer) lossDue(early bool) bool {
	now := time.Now()

	b.mu.Lock()
	defer b.mu.Unlock()

	switch {
	case b.ended, !b.lastLoss.IsZero() && now.Sub(b.lastLoss) < b.lossInterval:
		return false
	// What the jitter buffer gives up on within its latency of an early
	// report arrived before that report, so the key frame it brings follows.
	case !early && !b.lastEarly.IsZero() && now.Sub(b.lastEarly) < b.latency+lossReportSlack:
		return false
	}

	b.lastLoss = now
	if early {
		b.lastEarly = now
	}

	return true
}

// requestedSince reports whether a loss report went out after since and its
// key frame is still due. The frames behind the losses it covers leave the
// jitter buffer about a latency after it, and reach the writer only with the
// next push: pushWait bounds the wait for it while packets flow.
func (b *trackBuffer) requestedSince(since time.Time, pushWait time.Duration) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.lastLoss.After(since) && time.Since(b.lastLoss) < b.latency+lossReportSlack+pushWait
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
	case !w.started || dist >= seqResetDistance || dist <= -seqResetDistance:
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

// seqGaps times the sequence numbers missing behind the newest packet. A
// frame that arrives slowly but in order leaves none, so a paced key frame
// never counts as a loss. Only the pushing goroutine uses it.
type seqGaps struct {
	started  bool
	head     uint16
	open     []seqGap // oldest first
	lastRead time.Time
	prevRead time.Time
}

type seqGap struct {
	first   uint16
	span    uint16 // last - first
	missing int
	since   time.Time
	nacked  time.Time // the read that sent its first NACK
}

// read records a packet read at now, duplicates included, as the SDK's NACK
// generator sees every read of the main stream. RTX packets are recorded
// too, though the generator never sees them.
func (g *seqGaps) read(now time.Time) {
	g.prevRead, g.lastRead = g.lastRead, now

	for i := range g.open {
		if gap := &g.open[i]; gap.nacked.IsZero() && now.Sub(gap.since) >= gapFirstNACK {
			gap.nacked = now
		}
	}
}

// add records seq, which is not a duplicate, arriving at now.
func (g *seqGaps) add(seq uint16, now time.Time) {
	dist := int(int16(seq - g.head))

	switch {
	case !g.started || dist >= seqResetDistance || dist <= -seqResetDistance:
		g.started = true
		g.head = seq
		g.open = g.open[:0]
	case dist > 1:
		if len(g.open) < maxOpenGaps {
			g.open = append(g.open, seqGap{
				first: g.head + 1, span: uint16(dist - 2), missing: dist - 1, since: now,
			})
		} else {
			// Past the cap, a loss shares the newest gap's times. Only the
			// push right after a request can merge it into a gap that request
			// settles, and the request's key frame follows that loss too.
			last := &g.open[len(g.open)-1]
			last.span = seq - 1 - last.first
			last.missing += dist - 1
		}

		g.head = seq
	case dist == 1:
		g.head = seq
	default:
		g.fill(seq)
	}
}

// fill records a late packet: a retransmit or a reordered packet.
func (g *seqGaps) fill(seq uint16) {
	for i := range g.open {
		gap := &g.open[i]
		if seq-gap.first > gap.span {
			continue
		}

		if gap.missing--; gap.missing <= 0 {
			g.open = slices.Delete(g.open, i, i+1)
		}

		return
	}
}

// due forgets the gaps opened at or before settled, and reports whether the
// oldest one left has been open for gapKeyframeDelay and its first NACK has
// had gapRepairWindow to be answered.
func (g *seqGaps) due(now, settled time.Time) bool {
	n := 0
	for n < len(g.open) && !g.open[n].since.After(settled) {
		n++
	}

	g.open = slices.Delete(g.open, 0, n)
	if len(g.open) == 0 {
		return false
	}

	// pion returns one queued RTX packet each time a read starts, so a
	// retransmit that lands while the loop waits comes right after the next
	// packet. The read before this one shows that the window passed with none
	// only while at most one RTX packet is queued, which nothing guarantees: a
	// burst loss or a probe queues several.
	gap := g.open[0]

	return now.Sub(gap.since) >= gapKeyframeDelay &&
		!gap.nacked.IsZero() && g.prevRead.Sub(gap.nacked) >= gapRepairWindow
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
