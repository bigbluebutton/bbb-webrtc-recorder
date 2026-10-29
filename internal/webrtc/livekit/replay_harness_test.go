package livekit

import (
	"io"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/types"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

// replayResult is what one replay measured.
type replayResult struct {
	bufferRequests []time.Duration // onLoss calls, from the replay's start
	writerRequests []time.Duration // the writer's own requests
	videoFrames    int             // frames the writer wrote
	audioFrames    int
	packets        int    // packets the track buffers released to the writer
	packetsLost    uint64 // summed over the tracks' jitter buffers
	packetsDropped uint64
	webm           *oracle.Summary
	webmErr        error
	// The Opus track's lag behind its read times and its media clock, set
	// when exactly one Opus trace is replayed.
	audioLag    *oracle.Lag
	audioLagErr error
	audioClock  *types.MediaClockStats
}

func (r replayResult) requests() []time.Duration {
	return append(append([]time.Duration(nil), r.bufferRequests...), r.writerRequests...)
}

// countingRecorder is a WebmRecorder that counts the packets pushed to it.
type countingRecorder struct {
	*recorder.WebmRecorder

	mu     sync.Mutex
	pushed int
}

func (r *countingRecorder) count() {
	r.mu.Lock()
	r.pushed++
	r.mu.Unlock()
}

func (r *countingRecorder) PushVideoAt(p *rtp.Packet, at time.Time) {
	r.count()
	r.WebmRecorder.PushVideoAt(p, at)
}

func (r *countingRecorder) PushAudioAt(p *rtp.Packet, at time.Time) {
	r.count()
	r.WebmRecorder.PushAudioAt(p, at)
}

// replayHarness runs tracks through readTrack, a production trackBuffer and
// a real WebmRecorder, and records each keyframe request where it is made.
// Build it inside the synctest bubble that runs it.
type replayHarness struct {
	*loopHarness
	rec     *countingRecorder
	file    string
	start   time.Time
	infos   []trackInfo
	buffers []*trackBuffer

	mu             sync.Mutex
	bufferRequests []time.Duration
	writerRequests []time.Duration
}

func newReplayHarness(t *testing.T, infos ...trackInfo) *replayHarness {
	cfg := (&config.Config{}).GetDefaults().Recorder
	file := filepath.Join(t.TempDir(), "replay.webm")
	rec := &countingRecorder{WebmRecorder: recorder.NewWebmRecorder(
		file, 0o600, cfg.VideoPacketQueueSize, cfg.AudioPacketQueueSize, cfg.UseCustomSampler, cfg.WriteIVFCopy)}

	ids := make([]string, 0, len(infos))
	for _, info := range infos {
		ids = append(ids, info.id)
	}

	h := &replayHarness{loopHarness: newLoopHarness(rec, ids...), rec: rec, file: file, start: time.Now(), infos: infos}
	for _, info := range infos {
		if info.kind == TrackKindVideo {
			rec.SetHasVideo(true)
		} else {
			rec.SetHasAudio(true)
		}

		h.buffers = append(h.buffers, h.newBuffer(info, func() { h.record(&h.bufferRequests) }))
	}

	// After registerTrackBuffer, which hands the writer the adapter itself.
	rec.SetKeyframeRequester(writerRequester{h})

	return h
}

func (h *replayHarness) record(to *[]time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	*to = append(*to, time.Since(h.start))
}

// run reads srcs, one per info, until every track ends, with onRequest
// called for each request leaving the adapter's queue. It then closes the
// recorder and reads the WebM back.
func (h *replayHarness) run(onRequest func(ssrc uint32), srcs ...trackSource) replayResult {
	stop := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			select {
			case ssrc := <-h.w.keyframeRequestChan:
				if onRequest != nil {
					onRequest(ssrc)
				}
			case <-stop:
				return
			}
		}
	}()

	done := make(chan struct{}, len(srcs))
	for i, src := range srcs {
		go func() {
			h.w.readTrack(src, h.infos[i], h.buffers[i])
			done <- struct{}{}
		}()
	}

	for range srcs {
		<-done
	}

	close(stop)
	<-drained

	r := replayResult{}
	for _, b := range h.buffers {
		s := b.stats()
		r.packetsLost += s.PacketsLost
		r.packetsDropped += s.PacketsDropped
	}

	if st := h.rec.GetStats(); st != nil {
		if st.Video != nil {
			r.videoFrames = st.Video.WrittenSamples
		}
		if st.Audio != nil {
			r.audioFrames = st.Audio.WrittenSamples
		}
	}

	h.rec.Close()
	r.webm, r.webmErr = oracle.SummarizeWebM(h.file)

	h.mu.Lock()
	r.bufferRequests = append(r.bufferRequests, h.bufferRequests...)
	r.writerRequests = append(r.writerRequests, h.writerRequests...)
	h.mu.Unlock()

	h.rec.mu.Lock()
	r.packets = h.rec.pushed
	h.rec.mu.Unlock()

	return r
}

// writerRequester is the writer's keyframe requester: it records each
// request the writer makes and passes it on to the adapter, which stays the
// tracker the writer consults first.
type writerRequester struct{ h *replayHarness }

func (r writerRequester) RequestKeyframe() {
	r.h.record(&r.h.writerRequests)
	r.h.w.RequestKeyframe()
}

func (r writerRequester) RequestKeyframeForSSRC(ssrc uint32) {
	r.h.record(&r.h.writerRequests)
	r.h.w.RequestKeyframeForSSRC(ssrc)
}

func (r writerRequester) KeyframeRequestedSince(ssrc uint32, since time.Time) bool {
	return r.h.w.KeyframeRequestedSince(ssrc, since)
}

// settledSource hands out each read once every other goroutine of the
// bubble is blocked, so a jitter buffer timer that fires at the instant of a
// read always runs before it. Sources sharing turn take their reads one at a
// time; which of two reads due at the same instant goes first is still up to
// the scheduler.
type settledSource struct {
	trackSource
	turn chan struct{}
}

func (s settledSource) ReadRTP() (*rtp.Packet, interceptor.Attributes, error) {
	pkt, attrs, err := s.trackSource.ReadRTP()

	s.turn <- struct{}{}
	synctest.Wait()
	<-s.turn

	return pkt, attrs, err
}

// cameraFill is the payload pacedCamera puts behind each VP8 header.
var cameraFill = make([]byte, 100)

// vp8Frame is a VP8 frame of n packets from seq, all with timestamp ts and
// picture ID pic, the first carrying a 640x480 key frame header if key, and
// each payload ending in fill.
func vp8Frame(ssrc uint32, seq, pic uint16, ts uint32, n int, key bool, fill []byte) []*rtp.Packet {
	pkts := make([]*rtp.Packet, 0, n)
	for j := range n {
		payload := corpus.VP8Payload(pic&0x7fff, j == 0, key, 640, 480)
		pkts = append(pkts, &rtp.Packet{
			Header: rtp.Header{Version: 2, PayloadType: 96, SSRC: ssrc, SequenceNumber: seq + uint16(j),
				Timestamp: ts, Marker: j == n-1},
			Payload: append(payload, fill...),
		})
	}

	return pkts
}

// pacedCamera is a trackSource modelling a publisher's camera: a frame
// every 1/fps, and a key frame sent respond after each keyframe request
// reaches it, its packets spread over keySpread by a FIFO pacer, so the
// frames captured meanwhile follow its last packet. Frame lostFrame never
// gets its second packet.
type pacedCamera struct {
	ssrc         uint32
	fps          float64
	frames       int
	deltaPackets int
	keyPackets   int
	keySpread    time.Duration
	respond      time.Duration
	lostFrame    int

	start         time.Time
	deadline      time.Time
	staleDeadline bool // a read came without a deadline ahead, as in fakeTrack
	queue         []trackRead
	next          int // the next frame to capture
	seq           uint16
	lastAt        time.Duration

	mu      sync.Mutex
	pending []time.Duration // requests not answered yet, from start
	keys    int             // key frames sent after the first
}

func (c *pacedCamera) request(uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append(c.pending, time.Since(c.start))
}

func (c *pacedCamera) SetReadDeadline(deadline time.Time) error {
	c.deadline = deadline

	return nil
}

// sleepUntil waits for offset at, or until the read deadline if that comes
// first, which it reports. Like settledSource, it returns only once the rest
// of the bubble is blocked.
func (c *pacedCamera) sleepUntil(at time.Duration) (timedOut bool) {
	due := c.start.Add(at)
	if !c.deadline.IsZero() && c.deadline.Before(due) {
		due, timedOut = c.deadline, true
	}

	time.Sleep(time.Until(due))
	synctest.Wait()

	return timedOut
}

func (c *pacedCamera) ReadRTP() (*rtp.Packet, interceptor.Attributes, error) {
	if !c.deadline.After(time.Now()) {
		c.staleDeadline = true

		return nil, nil, io.EOF
	}

	if len(c.queue) == 0 {
		if c.next == c.frames {
			return nil, nil, io.EOF
		}

		at := max(time.Duration(float64(c.next)*float64(time.Second)/c.fps), c.lastAt)
		if c.sleepUntil(at) {
			return nil, nil, readTimeout{}
		}

		c.capture(at)
	}

	head := c.queue[0]
	if c.sleepUntil(head.at) {
		return nil, nil, readTimeout{}
	}

	c.queue = c.queue[1:]

	return head.pkt, nil, nil
}

// capture queues frame c.next, sent at offset at: a key frame if it is the
// first or a request is due by then.
func (c *pacedCamera) capture(at time.Duration) {
	f := c.next
	c.next++

	c.mu.Lock()
	key := f == 0
	// One key frame answers every request that reached the encoder by then.
	answered := 0
	for answered < len(c.pending) && c.pending[answered]+c.respond <= at {
		answered++
	}
	if answered > 0 && f > 0 {
		key = true
		c.keys++
	}
	c.pending = c.pending[answered:]
	c.mu.Unlock()

	n, spread := c.deltaPackets, time.Duration(0)
	if key {
		n = c.keyPackets
		// The first is not paced, so that only the loss can start a loop.
		if f > 0 {
			spread = c.keySpread
		}
	}

	ts := uint32(float64(f) * 90000 / c.fps)
	for j, p := range vp8Frame(c.ssrc, c.seq, uint16(f)&0x7fff, ts, n, key, cameraFill) {
		pAt := at
		if n > 1 {
			pAt += spread * time.Duration(j) / time.Duration(n-1)
		}
		c.lastAt = pAt

		if f == c.lostFrame && j == 1 {
			continue
		}
		c.queue = append(c.queue, trackRead{at: pAt, pkt: p})
	}

	c.seq += uint16(n)
}
