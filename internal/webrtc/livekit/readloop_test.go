package livekit

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/appstats"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/types"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/utils"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// trackRead is one scripted read of a fakeTrack: pkt, or err in its place,
// handed out at offset at from the track's creation.
type trackRead struct {
	at  time.Duration
	pkt *rtp.Packet
	err error
	// sr, if set, goes to onRTCP at offset at instead of being read.
	sr *rtcp.SenderReport
}

// readsAt is pkts, all read at offset at.
func readsAt(at time.Duration, pkts ...*rtp.Packet) []trackRead {
	reads := make([]trackRead, 0, len(pkts))
	for _, p := range pkts {
		reads = append(reads, trackRead{at: at, pkt: p})
	}

	return reads
}

// readTimeout is what pion's packetio returns once a read deadline passes.
type readTimeout struct{}

func (readTimeout) Error() string   { return "i/o timeout" }
func (readTimeout) Timeout() bool   { return true }
func (readTimeout) Temporary() bool { return true }

// fakeTrack is a trackSource playing its reads, in order of at, on the clock
// of the synctest bubble it was made in. A read whose deadline passes before
// the next item is due times out and leaves the item queued; an item already
// due is handed out at once, as from a socket's backlog. Past the script,
// reads return io.EOF. Like a pion track, it has a single reader.
type fakeTrack struct {
	start    time.Time
	reads    []trackRead
	deadline time.Time
	timeouts int // reads that timed out
	// A read with no deadline ahead ends the track instead of timing out at
	// once forever, which would spin without advancing the bubble's clock.
	staleDeadline bool
	// onRTCP gets the sender reports, as lksdk's RTCP callback would.
	onRTCP func(rtcp.Packet)
}

func newFakeTrack(reads ...[]trackRead) *fakeTrack {
	t := &fakeTrack{start: time.Now()}
	for _, r := range reads {
		t.reads = append(t.reads, r...)
	}

	return t
}

func (t *fakeTrack) SetReadDeadline(deadline time.Time) error {
	t.deadline = deadline

	return nil
}

func (t *fakeTrack) ReadRTP() (*rtp.Packet, interceptor.Attributes, error) {
	for first := true; ; first = false {
		if len(t.reads) == 0 {
			return nil, nil, io.EOF
		}

		if !t.deadline.After(time.Now()) {
			// Handing on a sender report can bring this read to its deadline.
			if !first {
				t.timeouts++

				return nil, nil, readTimeout{}
			}

			t.staleDeadline = true

			return nil, nil, io.EOF
		}

		next := t.reads[0]
		due := t.start.Add(next.at)
		if t.deadline.Before(due) {
			time.Sleep(time.Until(t.deadline))
			t.timeouts++

			return nil, nil, readTimeout{}
		}

		time.Sleep(time.Until(due))
		t.reads = t.reads[1:]

		if next.sr == nil {
			return next.pkt, nil, next.err
		}

		if t.onRTCP != nil {
			t.onRTCP(next.sr)
		}
	}
}

// pushRecorder is a mockRecorder that keeps the sequence numbers pushed to
// it and their arrivals, and panics on the first packet panicOn matches.
type pushRecorder struct {
	mockRecorder
	panicOn func(*rtp.Packet) bool

	mu       sync.Mutex
	video    []uint16
	audio    []uint16
	arrivals map[uint16]time.Time
}

func newPushRecorder() *pushRecorder {
	return &pushRecorder{mockRecorder: mockRecorder{
		videoStats: &types.RecorderTrackStats{},
		audioStats: &types.RecorderTrackStats{},
		filePath:   "test.webm",
	}}
}

func (r *pushRecorder) push(seqs *[]uint16, p *rtp.Packet, at time.Time) {
	if r.panicOn != nil && r.panicOn(p) {
		panic("pushRecorder: scripted panic")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	*seqs = append(*seqs, p.SequenceNumber)

	if r.arrivals == nil {
		r.arrivals = map[uint16]time.Time{}
	}
	r.arrivals[p.SequenceNumber] = at
}

func (r *pushRecorder) PushVideoAt(p *rtp.Packet, at time.Time) { r.push(&r.video, p, at) }
func (r *pushRecorder) PushAudioAt(p *rtp.Packet, at time.Time) { r.push(&r.audio, p, at) }

// loopHarness is a LiveKitWebRTC without a room, to drive readTrack directly,
// and what it reported. Build it inside the synctest bubble that runs it: the
// track buffers it makes run timers on that bubble's clock.
type loopHarness struct {
	w *LiveKitWebRTC

	mu     sync.Mutex
	states []utils.ConnectionState // each connection state callback
	flows  []bool                  // each flow callback's isFlowing
}

func newLoopHarness(rec recorder.Recorder, trackIDs ...string) *loopHarness {
	h := &loopHarness{}
	h.w = &LiveKitWebRTC{
		ctx:                  context.WithValue(context.Background(), "session", "test-session"), //nolint:staticcheck // SA1029: the key livekit.go reads
		cfg:                  (&config.Config{}).GetDefaults().LiveKit,
		rec:                  rec,
		trackIds:             trackIDs,
		remoteTrackPubs:      map[string]*lksdk.RemoteTrackPublication{},
		participantIDs:       map[string]string{},
		pliStats:             map[uint32]pliTracker{},
		jitterBuffers:        map[string]*trackBuffer{},
		videoBuffers:         map[uint32]*trackBuffer{},
		flowState:            map[string]*trackFlowState{},
		trackStats:           map[string]*appstats.AdapterTrackStats{},
		rtpWriters:           map[string]*recorder.RTPWriter{},
		startTs:              time.Now(),
		keyframeRequestChan:  make(chan uint32, 100),
		requestKeyframeCtx:   context.Background(),
		pendingSubscriptions: map[string]time.Time{},
	}
	h.w.constraints = newConstraintBook(h.w.cfg.PreferredVideoQuality)
	h.w.sendVideoQuality = sendPubVideoQuality
	h.w.initTrackStats()
	h.w.connStateCallback = func(state utils.ConnectionState) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.states = append(h.states, state)
	}
	h.w.flowCallback = func(isFlowing bool, _ time.Duration, _ bool) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.flows = append(h.flows, isFlowing)
	}

	return h
}

// newBuffer makes and registers info's track buffer as onTrackSubscribed does.
// onLoss, if set, sees each keyframe request a video buffer queues.
func (h *loopHarness) newBuffer(info trackInfo, onLoss func()) *trackBuffer {
	isVideo := info.kind == TrackKindVideo
	var depacketizer rtp.Depacketizer = &codecs.OpusPacket{}
	if info.mime == MimeTypeVP8 {
		depacketizer = &codecs.VP8Packet{}
	}

	b := newTrackBuffer(depacketizer, jitterLatencyFor(info.kind), lossKeyframeInterval, func() {
		if isVideo {
			if onLoss != nil {
				onLoss()
			}
			h.w.queueKeyframeRequest(info.ssrc, "packet_drop")
		}
	})
	h.w.registerTrackBuffer(info.id, isVideo, info.ssrc, b)

	return b
}

// run reads src through readTrack until the track ends.
func (h *loopHarness) run(src trackSource, info trackInfo) {
	h.w.readTrack(src, info, h.newBuffer(info, nil))
}

// reported waits for the flow events still being delivered. Call it inside
// the bubble.
func (h *loopHarness) reported() (states []utils.ConnectionState, flows []bool) {
	synctest.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]utils.ConnectionState(nil), h.states...), append([]bool(nil), h.flows...)
}

var (
	audioInfo = trackInfo{id: "audio", kind: TrackKindAudio, mime: MimeTypeOpus, source: "microphone", ssrc: 1111, pubMime: "audio/opus"}
	videoInfo = trackInfo{id: "video", kind: TrackKindVideo, mime: MimeTypeVP8, source: "camera", ssrc: 1234, pubMime: "video/VP8"}
)

// audioReads is opus packets from seq, one every 20 ms from offset at.
func audioReads(at time.Duration, seq uint16, n int) []trackRead {
	reads := make([]trackRead, 0, n)
	for i := range n {
		reads = append(reads, trackRead{at: at + time.Duration(i)*20*time.Millisecond, pkt: opusPacket(seq + uint16(i))})
	}

	return reads
}

func seqRange(from uint16, n int) []uint16 {
	seqs := make([]uint16, 0, n)
	for i := range n {
		seqs = append(seqs, from+uint16(i))
	}

	return seqs
}

func TestReadTrackPushesInOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := newPushRecorder()
		h := newLoopHarness(rec, audioInfo.id)
		reads := audioReads(0, 100, 50)
		// 110 comes in after 111.
		reads[10].pkt, reads[11].pkt = reads[11].pkt, reads[10].pkt

		src := newFakeTrack(reads)

		h.run(src, audioInfo)

		assert.False(t, src.staleDeadline)
		assert.Equal(t, seqRange(100, 50), rec.audio)
		assert.Empty(t, rec.video)
		states, _ := h.reported()
		assert.Equal(t, []utils.ConnectionState{utils.ConnectionStateClosed}, states)
		assert.Equal(t, uint16(149), h.w.trackStats[audioInfo.id].LastSeqNum)
	})
}

func TestReadTrackTimeoutStopsFlow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := newPushRecorder()
		// Unlike mime, so the metric shows which of the two it is labelled with.
		info := audioInfo
		info.pubMime = "audio/OPUS"
		h := newLoopHarness(rec, info.id)
		// A timeout before the first packet finds no flow to stop; the 1 s
		// gap after 1 s of packets stops it.
		src := newFakeTrack(audioReads(700*time.Millisecond, 0, 50), audioReads(2700*time.Millisecond, 50, 10))
		readErrors := appstats.RTPReadErrors.WithLabelValues(info.source, string(info.kind), info.pubMime, readTimeout{}.Error())
		before := testutil.ToFloat64(readErrors)

		h.run(src, info)

		assert.False(t, src.staleDeadline)
		states, flows := h.reported()
		assert.Equal(t, []bool{true, false, true}, flows)
		assert.Equal(t, []utils.ConnectionState{utils.ConnectionStateClosed}, states)
		assert.Equal(t, seqRange(0, 60), rec.audio)
		assert.Equal(t, 3, src.timeouts, "one before the first packet, two in the gap")
		assert.Equal(t, src.timeouts, h.w.trackStats[info.id].RTPReadErrors)
		assert.Equal(t, float64(src.timeouts), testutil.ToFloat64(readErrors)-before, "read errors counted under pubMime")
	})
}

func TestReadTrackFlushesOnEOF(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const lost = 5
		rec := newPushRecorder()
		h := newLoopHarness(rec, videoInfo.id)
		var reads []trackRead
		var want []uint16
		seq := uint16(1000)
		for f := range 10 {
			for j, p := range writerFrame(videoInfo.ssrc, seq, uint16(100+f), f, f == 0) {
				if f == lost && j == 1 {
					continue
				}
				reads = append(reads, readsAt(time.Duration(f)*time.Second/30, p)...)
				if f != lost {
					want = append(want, p.SequenceNumber)
				}
			}
			seq += 4
		}
		// The track ends well within the video latency of the loss, while
		// the frames behind it wait in the jitter buffer.
		reads = append(reads, trackRead{at: 400 * time.Millisecond, err: io.EOF})

		src := newFakeTrack(reads)

		h.run(src, videoInfo)

		assert.False(t, src.staleDeadline)
		assert.Equal(t, want, rec.video, "the frames behind the loss, without the incomplete one")
		assert.Equal(t, want[len(want)-1], h.w.trackStats[videoInfo.id].LastSeqNum, "the flush updates the track stats")
		states, _ := h.reported()
		assert.Equal(t, []utils.ConnectionState{utils.ConnectionStateClosed}, states)
	})
}

func TestReadTrackPanicReportsFailed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := newPushRecorder()
		rec.panicOn = func(p *rtp.Packet) bool { return p.SequenceNumber == 110 }
		h := newLoopHarness(rec, audioInfo.id)
		src := newFakeTrack(audioReads(0, 100, 50))

		require.NotPanics(t, func() { h.run(src, audioInfo) })

		assert.False(t, src.staleDeadline)
		assert.Equal(t, seqRange(100, 10), rec.audio)
		assert.Len(t, src.reads, 39, "the loop stops at the panic")
		states, _ := h.reported()
		assert.Equal(t, []utils.ConnectionState{utils.ConnectionStateFailed}, states)
	})
}

// Run with -race: each track's first packet adds its flow-state entry while
// other tracks' read errors look theirs up.
func TestReadErrorReadsFlowStateUnderLock(t *testing.T) {
	h := newLoopHarness(newPushRecorder(), audioInfo.id)
	h.w.updateFlowState(audioInfo.id, 1, time.Now())
	h.w.updateFlowState(audioInfo.id, 2, time.Now())

	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 200 {
			h.w.updateFlowState(fmt.Sprintf("track-%d", i), 1, time.Now())
		}
	})
	for range 200 {
		assert.NoError(t, h.w.handleReadRTPError(readTimeout{}, audioInfo))
	}
	wg.Wait()
}

// Reads keep coming while they carry only padding, such as LiveKit's probes
// and mute padding: only the flow check sees no media arrive.
func TestReadTrackFlowCheckMarksStall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLoopHarness(newPushRecorder(), videoInfo.id)
		var reads []trackRead
		seq := uint16(100)
		for i := range 40 {
			at := time.Duration(i) * 100 * time.Millisecond
			if i >= 10 && i < 30 {
				reads = append(reads, readsAt(at, &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Padding: true}})...)
				seq++
				continue
			}

			ts := uint32(3000 * (i + 1))
			reads = append(reads, readsAt(at, vp8Packet(seq, ts, true, false), vp8Packet(seq+1, ts, false, true))...)
			seq += 2
		}
		reads = append(reads, trackRead{at: 4 * time.Second, err: io.EOF})
		src := newFakeTrack(reads)

		h.run(src, videoInfo)

		assert.False(t, src.staleDeadline)
		assert.Zero(t, src.timeouts, "no read waits past its deadline")
		_, flows := h.reported()
		assert.Equal(t, []bool{true, false, true}, flows)
	})
}

// closeOrderRecorder records the audio sequence numbers pushed before and
// after Close.
type closeOrderRecorder struct {
	*mockRecorder

	mu     sync.Mutex
	closed bool
	pushed []uint16 // pushed before Close
	late   []uint16 // pushed after Close
}

func (r *closeOrderRecorder) PushAudioAt(p *rtp.Packet, _ time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		r.late = append(r.late, p.SequenceNumber)
	} else {
		r.pushed = append(r.pushed, p.SequenceNumber)
	}
}

func (r *closeOrderRecorder) Close() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closed = true

	return 0
}

// endAt returns a read that ends the track (io.EOF) at offset at.
func endAt(at time.Duration) []trackRead {
	return []trackRead{{at: at, err: io.EOF}}
}

// After a packet loss, the jitter buffer releases the frames that follow only
// when the next packet arrives or the track ends. If the source is quiet, only
// the track's end delivers them, so Close must wait for the reader before it
// closes the recorder.
func TestCloseWaitsForTheTracksLastFrames(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &closeOrderRecorder{mockRecorder: &mockRecorder{}}
		h := newLoopHarness(rec, audioInfo.id)

		// Seq 105 is lost.
		src := newFakeTrack(audioReads(0, 100, 5), audioReads(120*time.Millisecond, 106, 10), endAt(700*time.Millisecond))

		h.w.startReader(src, audioInfo, h.newBuffer(audioInfo, nil))
		time.Sleep(600 * time.Millisecond) // the source is quiet, the gap expired
		h.w.Close()
		time.Sleep(200 * time.Millisecond) // past the track's end
		synctest.Wait()

		rec.mu.Lock()
		defer rec.mu.Unlock()
		assert.Len(t, rec.pushed, 15, "frames the recorder got before it closed")
		assert.Empty(t, rec.late, "frames that reached the recorder after it closed")
	})
}

// Close must not wait forever for a reader that never ends.
func TestCloseDoesNotWaitForAStuckReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &closeOrderRecorder{mockRecorder: &mockRecorder{}}
		h := newLoopHarness(rec, audioInfo.id)
		src := newFakeTrack(endAt(time.Minute))

		h.w.startReader(src, audioInfo, h.newBuffer(audioInfo, nil))
		start := time.Now()
		h.w.Close()
		assert.Equal(t, readersCloseTimeout, time.Since(start))

		time.Sleep(time.Minute) // let the reader end inside the bubble
		synctest.Wait()
	})
}

// A track subscribed after Close stopped waiting must not be added to the
// readers WaitGroup: that Add races with the abandoned Wait, which -race
// reports. The first reader must outlive the timeout for the race to show.
func TestReaderStartedAfterCloseIsNotWaitedFor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLoopHarness(&closeOrderRecorder{mockRecorder: &mockRecorder{}}, audioInfo.id)
		src := newFakeTrack(endAt(readersCloseTimeout + time.Second))
		h.w.startReader(src, audioInfo, h.newBuffer(audioInfo, nil))

		lateBuffer := h.newBuffer(audioInfo, nil)
		go func() {
			time.Sleep(readersCloseTimeout + 2*time.Second) // the first reader has ended
			h.w.startReader(newFakeTrack(endAt(10*time.Millisecond)), audioInfo, lateBuffer)
		}()

		h.w.Close()
		time.Sleep(time.Minute) // let both readers end inside the bubble
		synctest.Wait()
	})
}
