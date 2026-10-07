//go:build e2e && linux

package e2e

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// The frame layout of trackbuffer_test.go's bursts: 8 packets read 1 ms
	// apart, the third lost, so the in-process request bounds apply as measured.
	lossDeltaPackets  = 8
	lossDeltaSpreadUs = 7000
	lossDropIndex     = 2

	// Long enough that the schedule, whose only key frame is its first, does
	// not loop during a run.
	lossScheduleLen = time.Minute

	// From the recorder's keyframe request through LiveKit to the publisher,
	// and from its key frame back to the recorder, on loopback.
	pliPathAllowance = 100 * time.Millisecond
	// A loaded runner delays the recorder's reads, and so its request.
	readSlack = 50 * time.Millisecond

	// As in the baseline: subscription plus LiveKit's PLI before the first
	// frame, and the frames read but not yet written at stop.
	lossStartupAllowance = time.Second
	lossStopTailFrames   = 2
)

// lossScenario is one camera with one unrepaired mid-frame loss.
type lossScenario struct {
	name string
	fps  float64
	// keyPackets and keySpread shape the publisher's PLI key frames.
	keyPackets int
	keySpread  time.Duration
	// lossAfter is when the loss is armed, after the session flows; the
	// session stops lossToStop after the loss, or recordFor after it starts.
	lossAfter  time.Duration
	lossToStop time.Duration
	recordFor  time.Duration
	// request is trackbuffer_test.go's in-process window for the request
	// after the packet that reveals the loss (TestTrackBufferLowFrameRateRetransmit).
	request [2]time.Duration
}

func (sc lossScenario) frameInterval() time.Duration {
	return time.Duration(float64(time.Second) / sc.fps)
}

// requestBound is the latest the request may follow the reveal on the real
// path: LiveKit's forwarding moves each burst by a few ms, which can push
// the read that crosses the in-process deadline to the next frame's.
func (sc lossScenario) requestBound() time.Duration {
	return sc.request[1] + sc.frameInterval() + readSlack
}

// lossOutage bounds the span of frames the loss costs: the lost frame up to
// the publisher's key frame, due on its first frame after the PLI, plus the
// pacing that delays the frames behind that key frame.
func (sc lossScenario) lossOutage() time.Duration {
	return sc.requestBound() + pliPathAllowance + sc.frameInterval() + sc.keySpread
}

// minFrames is the frame count of span at the scenario's rate, minus the
// stop tail; 0 if span is not positive.
func (sc lossScenario) minFrames(span time.Duration) int {
	return max(0, int(sc.fps*span.Seconds())-lossStopTailFrames)
}

// midFrameDrop withholds one packet, index of its frame, from the first
// delta frame sent once it is armed.
type midFrameDrop struct {
	index int
	armed atomic.Bool
	fired atomic.Bool
}

func (d *midFrameDrop) drop(s SentPacket) bool {
	return d.armed.Load() && !s.Key && s.Index == d.index && d.fired.CompareAndSwap(false, true)
}

// lossRun is what runLoss recorded and read back.
type lossRun struct {
	Stack     *Stack
	Cam       *PubTrack
	Rec       *Recording
	WebM      *oracle.Track // empty when the recorder wrote no file
	Trace     *traceLoss
	Dropped   SentPacket
	DropAt    time.Time
	StartSent time.Time // startRecording sent
	Started   time.Time // startRecording answered
	StopSent  time.Time
}

// runLoss records sc's camera on its own stack, arms the loss, then stops
// the session and reads its output back.
func runLoss(t *testing.T, sc lossScenario) *lossRun {
	t.Helper()

	s := NewStack(t, StackOptions{})
	room := "e2e-" + sc.name + "-" + uuid.NewString()[:8]
	pub := s.NewPublisher(room, "e2e-publisher")

	sched, err := corpus.GenerateVP8(corpus.VP8Options{
		FPS: sc.fps, Duration: lossScheduleLen, KeyPackets: sc.keyPackets,
		DeltaPackets: lossDeltaPackets, SpreadUs: lossDeltaSpreadUs,
	})
	require.NoError(t, err)

	drop := &midFrameDrop{index: lossDropIndex}
	cam := pub.PublishVP8(TrackOptions{
		Name: "camera", Schedule: sched, KeyframeSpread: sc.keySpread, Drop: drop.drop,
	})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	require.NoError(t, pub.WaitPublished(ctx, cam))

	r := &lossRun{Stack: s, Cam: cam, StartSent: time.Now()}

	r.Rec, err = s.Driver.StartRecording(ctx, room, cam.SID)
	require.NoError(t, err)
	r.Started = time.Now()

	_, err = s.Driver.WaitRtpStatus(ctx, r.Rec.From, r.Rec.SessionID, "flowing")
	require.NoError(t, err)

	time.Sleep(sc.lossAfter)
	drop.armed.Store(true)

	r.Dropped, err = waitDropped(ctx, cam)
	require.NoError(t, err)
	r.DropAt = cam.Start().Add(r.Dropped.At)

	stopAt := r.DropAt.Add(sc.lossToStop)
	if sc.recordFor > 0 {
		stopAt = r.Started.Add(sc.recordFor)
	}

	time.Sleep(time.Until(stopAt))

	r.StopSent = time.Now()
	st, err := s.Driver.StopAndWait(ctx, r.Rec.SessionID)
	require.NoError(t, err)
	assert.Equal(t, events.StopReasonNormal, st.Reason)

	// The recorder creates the file with its first frame, so a keyframe loop
	// from the start leaves none; its trace and the publisher still tell.
	if _, err := os.Stat(r.Rec.Path); err != nil {
		assert.Fail(t, "no WebM written", "%v", err)

		trace, err := corpus.ReadFile(TracePath(r.Rec.Path, cam.SID))
		require.NoError(t, err, "trace capture")

		r.WebM, r.Trace = &oracle.Track{Codec: oracle.CodecVP8, Monotonic: true}, readTraceLoss(trace)

		return r
	}

	out := ReadOutput(t, r.Rec, cam.SID)
	require.Equal(t, oracle.CodecVP8, out.WebM.Codec)
	r.WebM, r.Trace = out.WebM, readTraceLoss(out.Trace)

	return r
}

// lossGap returns the trace's one unrepaired gap, failing t unless it is
// the withheld packet: one packet, at the withheld packet's index inside a
// frame. Gaps LiveKit repaired, e.g. a loopback overflow, are only logged.
func (r *lossRun) lossGap(t *testing.T) traceGap {
	t.Helper()

	l := r.Trace
	require.NotEmpty(t, l.Keys, "key frames read")

	for _, g := range l.Gaps {
		if g.Repaired == g.Missing {
			t.Logf("repaired gap: %d packets at %.1f ms", g.Missing, float64(g.RevealUs)/1000)
		}
	}

	gaps := l.Unrepaired()
	require.Len(t, gaps, 1, "unrepaired sequence gaps: only the withheld packet")

	g := gaps[0]
	require.Equal(t, 1, g.Missing, "packets missing at the gap")
	require.Equal(t, r.Dropped.Index, g.Index, "the missing packet's index in its frame (-1: not inside one)")

	return g
}

func waitDropped(ctx context.Context, tr *PubTrack) (SentPacket, error) {
	for {
		for _, s := range tr.Sent() {
			if s.Dropped {
				return s, nil
			}
		}

		select {
		case <-ctx.Done():
			return SentPacket{}, fmt.Errorf("no packet withheld: %w", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// traceLoss is what a capture trace shows of a track's losses, in the
// sequence numbers and timestamps LiveKit rewrote for the recorder.
type traceLoss struct {
	Gaps []traceGap // every sequence jump on the main stream, repaired or not
	Keys []traceKey // key frame starts, read order
	PLIs []corpus.Record
}

// traceGap is a sequence jump on the main stream.
type traceGap struct {
	RevealUs int64  // read time of the packet after the gap
	TS       uint32 // that packet's timestamp
	PrevTS   uint32 // the timestamp of the media packet before the gap
	Missing  int
	// Repaired counts the missing packets read later, as RTX or in band:
	// either way the loss was not the recorder's to recover.
	Repaired int
	// Index is the first missing packet's position in its frame, for a gap
	// inside one frame (PrevTS == TS); -1 otherwise.
	Index int
}

type traceKey struct {
	AtUs int64
	TS   uint32
}

func readTraceLoss(tr *corpus.Trace) *traceLoss {
	out := &traceLoss{}

	var (
		started    bool
		head       int64 // highest extended sequence number read on the main stream
		frameStart int64 // extended sequence number of the current frame's first packet
		lastTS     uint32
		missing    = map[int64]int{} // extended sequence number -> its gap's index in out.Gaps
	)

	// Padding counts for the sequence but never starts a frame. An RTX probe
	// carries no original sequence number, so it never repairs a gap.
	for _, r := range tr.Records {
		if r.T == corpus.RecordPLI {
			out.PLIs = append(out.PLIs, r)
			continue
		}

		if r.T != corpus.RecordRTP {
			continue
		}

		if !started {
			if r.RTX {
				continue
			}

			started, head, frameStart, lastTS = true, int64(r.Seq), int64(r.Seq), r.TS

			if !r.Pad && isVP8KeyFrameStart(r.Pay) {
				out.Keys = append(out.Keys, traceKey{AtUs: r.AtUs, TS: r.TS})
			}

			continue
		}

		ext := head + int64(int16(r.Seq-uint16(head)))

		if r.RTX || ext <= head {
			if g, ok := missing[ext]; ok && (!r.RTX || !r.Pad) {
				out.Gaps[g].Repaired++
				delete(missing, ext)
			}

			continue
		}

		if ext > head+1 {
			g := traceGap{RevealUs: r.AtUs, TS: r.TS, PrevTS: lastTS, Missing: int(ext - head - 1), Index: -1}
			if !r.Pad && r.TS == lastTS {
				g.Index = int(head + 1 - frameStart)
			}

			for m := head + 1; m < ext; m++ {
				missing[m] = len(out.Gaps)
			}

			out.Gaps = append(out.Gaps, g)
		}

		head = ext

		if r.Pad {
			continue
		}

		if r.TS != lastTS {
			frameStart, lastTS = ext, r.TS
		}

		if isVP8KeyFrameStart(r.Pay) {
			out.Keys = append(out.Keys, traceKey{AtUs: r.AtUs, TS: r.TS})
		}
	}

	return out
}

// Unrepaired returns the gaps some of whose packets were never read.
func (l *traceLoss) Unrepaired() []traceGap {
	var out []traceGap

	for _, g := range l.Gaps {
		if g.Repaired < g.Missing {
			out = append(out, g)
		}
	}

	return out
}

// RepairedPackets counts the missing packets read later.
func (l *traceLoss) RepairedPackets() int {
	n := 0
	for _, g := range l.Gaps {
		n += g.Repaired
	}

	return n
}

// requestsSince returns the pli records read at or after atUs, of reason if
// it is not empty.
func (l *traceLoss) requestsSince(atUs int64, reason string) []corpus.Record {
	var out []corpus.Record

	for _, r := range l.PLIs {
		if r.AtUs >= atUs && (reason == "" || r.Reason == reason) {
			out = append(out, r)
		}
	}

	return out
}

// keyAfter returns the first key frame read after atUs.
func (l *traceLoss) keyAfter(atUs int64) (traceKey, bool) {
	for _, k := range l.Keys {
		if k.AtUs > atUs {
			return k, true
		}
	}

	return traceKey{}, false
}

// ptsOf maps a trace timestamp to the WebM PTS of its frame. The recorder
// starts the file at the first key frame it reads and steps PTS by RTP
// timestamp deltas, truncated to ms.
func (l *traceLoss) ptsOf(webm *oracle.Track, ts uint32) int64 {
	return webm.FirstPTS + int64(ts-l.Keys[0].TS)/90
}

// hasPTS reports whether webm wrote a frame within 1 ms of pts.
func hasPTS(webm *oracle.Track, pts int64) bool {
	for _, p := range webm.PTS {
		if p >= pts-1 && p <= pts+1 {
			return true
		}
	}

	return false
}

// framesAfter counts the frames webm wrote with a PTS after pts.
func framesAfter(webm *oracle.Track, pts int64) int {
	n := 0

	for _, p := range webm.PTS {
		if p > pts {
			n++
		}
	}

	return n
}

// plisSince counts the PLIs the publisher received at or after d from its start.
func plisSince(tr *PubTrack, d time.Duration) int {
	n := 0

	for _, at := range tr.PLIs() {
		if at >= d {
			n++
		}
	}

	return n
}

// pliKeyAfter returns the first packet of the first key frame the
// publisher sent for a PLI at or after d from its start.
func pliKeyAfter(tr *PubTrack, d time.Duration) (SentPacket, bool) {
	for _, s := range tr.Sent() {
		if s.PLIKey && s.At >= d {
			return s, true
		}
	}

	return SentPacket{}, false
}

// pliKeysSince counts the key frames the publisher sent for PLIs at or
// after d from its start.
func pliKeysSince(tr *PubTrack, d time.Duration) int {
	n, last := 0, -1

	for _, s := range tr.Sent() {
		if s.PLIKey && s.At >= d && s.Frame != last {
			last = s.Frame
			n++
		}
	}

	return n
}

func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}
