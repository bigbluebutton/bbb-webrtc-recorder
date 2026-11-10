//go:build e2e && linux

package e2e

import (
	"context"
	"math"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// taggedBlock is a WebM block and the publisher frame it holds.
type taggedBlock struct {
	PTS   int64 // ms
	Frame int
}

// readBlocks reads every block of the WebM at path, which must hold one
// track.
func readBlocks(t *testing.T, path string) []oracle.Block {
	t.Helper()

	tracks, blocks, err := oracle.ReadBlocks(path)
	require.NoError(t, err)
	require.Len(t, tracks, 1)

	for _, b := range blocks {
		require.False(t, b.Group, "the recorder writes simple blocks only")
	}
	require.NotEmpty(t, blocks)

	return blocks
}

// readTaggedBlocks is readBlocks for a track of frames the publisher tagged.
func readTaggedBlocks(t *testing.T, path string) []taggedBlock {
	t.Helper()

	var blocks []taggedBlock
	for _, b := range readBlocks(t, path) {
		v, ok := corpus.TaggedFrame(b.Data)
		require.True(t, ok, "block at %d ms has no frame tag", b.PTS)
		blocks = append(blocks, taggedBlock{PTS: b.PTS, Frame: int(v)})
	}

	return blocks
}

// frameSends maps each frame tr sent to when its first packet was due. A
// late wake of the send loop does not move it. Behind a paced key frame
// (KeyframeSpread), the next frame's due time is later than its RTP
// timestamp. So do not check placement on a paced track: those frames
// would read as up to KeyframeSpread early.
func frameSends(tr *PubTrack) map[int]time.Time {
	start := tr.Start()
	sends := map[int]time.Time{}

	for _, p := range tr.Sent() {
		if p.Index == 0 {
			sends[p.Frame] = start.Add(p.Due)
		}
	}

	return sends
}

// firstFlowing returns when the driver received sessionID's first flowing
// event. Without media timestamps, bbb-webrtc-sfu stamps the recording's
// start when it handles that event.
func firstFlowing(d *Driver, from int, sessionID string) time.Time {
	msgs := d.Messages()[from:]
	if i := slices.IndexFunc(msgs, rtpStatusMatch(sessionID, "flowing")); i >= 0 {
		return msgs[i].At
	}

	return time.Time{}
}

func medianMs(ds []time.Duration) float64 {
	s := slices.Clone(ds)
	slices.Sort(s)

	return float64(s[len(s)/2]) / float64(time.Millisecond)
}

// The recorder's caps on the last frame's duration in mediaEnd.
const (
	maxVideoStep = time.Second
	maxAudioStep = 120 * time.Millisecond
)

func maxStepOf(kind corpus.Kind) time.Duration {
	if kind == corpus.KindVideo {
		return maxVideoStep
	}

	return maxAudioStep
}

const (
	// A file places all its blocks with one offset: mediaStart comes from
	// the least delayed of its first frames, and each PTS from RTP time. On
	// loopback that offset is the path's delay, about 1 ms, plus up to 2 ms
	// of rounding to whole ms. 15 ms stays under one 20 ms Opus frame.
	framePlacementBound = 15 * time.Millisecond
	// Two tracks of one publisher take the same path: their placements may
	// differ by that path's jitter only.
	frameLipSyncBound = 15 * time.Millisecond
)

// mediaStartHR less mediaStartUTC moves only when the wall clock steps. Each
// is truncated to the ms.
const hostClockStep = 2 * time.Millisecond

// hostClockOffset returns the host's CLOCK_MONOTONIC less its wall clock, in
// ms. The test can be preempted between the two reads, so it reads the wall
// clock before and after the monotonic one and takes the midpoint. Of a few
// tries, it keeps the one with the reads closest together.
func hostClockOffset(t *testing.T) float64 {
	t.Helper()

	var (
		offset float64
		width  = time.Duration(math.MaxInt64)
	)

	for range 5 {
		var mono unix.Timespec

		before := time.Now()
		require.NoError(t, unix.ClockGettime(unix.CLOCK_MONOTONIC, &mono))
		after := time.Now()

		if w := after.Sub(before); w < width {
			width = w
			wall := before.Add(w / 2)
			offset = float64(mono.Nano()-wall.UnixNano()) / float64(time.Millisecond)
		}
	}

	return offset
}

// placement is where record-and-playback plays a file's blocks: its first
// at mediaStart, the rest by their PTS since the first.
type placement struct {
	Blocks    []taggedBlock
	Started   events.RecordingMediaStarted
	StartedAt int             // recordingMediaStarted's message index
	Errs      []time.Duration // per block: when it plays minus its frame's time in sends
	Worst     time.Duration
	Median    float64 // ms
}

// checkPlacement checks that every block of rec's file plays within
// framePlacementBound of its frame's time in sends, the due time frameSends
// gives. It also checks that stop carries the file's media span. It returns
// false, a failure already reported, when the recorder sent no
// recordingMediaStarted for rec.
func checkPlacement(ctx context.Context, t *testing.T, d *Driver, name string, rec *Recording,
	sends map[int]time.Time, stop *events.RecordingStopped, maxStep time.Duration,
) (placement, bool) {
	t.Helper()

	p := placement{Blocks: readTaggedBlocks(t, rec.Path)}
	for _, b := range p.Blocks {
		_, ok := sends[b.Frame]
		require.True(t, ok, "%s: frame %d was not sent", name, b.Frame)
	}

	var err error
	p.Started, p.StartedAt, err = d.WaitMediaStarted(ctx, rec)
	if !assert.NoError(t, err, "%s: recordingMediaStarted", name) {
		return p, false
	}

	start := time.UnixMilli(p.Started.MediaStartUTC)
	pts := make([]int64, 0, len(p.Blocks))
	for _, b := range p.Blocks {
		placed := start.Add(time.Duration(b.PTS-p.Blocks[0].PTS) * time.Millisecond)
		p.Errs = append(p.Errs, placed.Sub(sends[b.Frame]))
		pts = append(pts, b.PTS)
	}

	p.Worst = slices.MaxFunc(p.Errs, func(a, b time.Duration) int { return int(a.Abs() - b.Abs()) })
	p.Median = medianMs(p.Errs)
	t.Logf("%s: %d blocks; mediaStart places frames %.1f ms (median) from where they were sent, %v at worst",
		name, len(p.Blocks), p.Median, p.Worst)
	assert.LessOrEqual(t, p.Worst.Abs(), framePlacementBound, "%s: worst placement", name)

	assertMediaEnd(t, name, p.Started, stop, pts, maxStep)

	return p, true
}

// assertLipSync checks that a camera and a microphone recorded together
// play their frames equally far from when they were sent. It does nothing
// when either file has no recordingMediaStarted (checkPlacement returned
// false).
func assertLipSync(t *testing.T, label string, cam, mic placement) {
	t.Helper()

	if len(cam.Errs) == 0 || len(mic.Errs) == 0 {
		return
	}

	t.Logf("%s: the camera lands %.1f ms after the microphone", label, cam.Median-mic.Median)
	assert.InDelta(t, mic.Median, cam.Median, ms(frameLipSyncBound), label)
}

// maxWakeLag is how late tr's send loop woke for a frame, at worst. The
// placement checks measure from when each frame was due, so one late frame
// does not count against the recorder. mediaStart comes from the least-late
// of the file's first frames, so a lag on all of them moves every block. A
// large lag shows a loaded runner.
func maxWakeLag(tr *PubTrack) time.Duration {
	var lag time.Duration

	for _, p := range tr.Sent() {
		if p.Index == 0 {
			lag = max(lag, p.At-p.Due)
		}
	}

	return lag
}

// assertMediaEnd checks that recordingStopped repeats mediaStart. Its
// mediaEnd must add the file's media, first PTS to last, and the last
// frame's duration: the step before it, up to maxStep. PTS and event times
// are whole ms and the recorder's step is not, so mediaEnd and that sum
// agree within 2 ms.
func assertMediaEnd(t *testing.T, name string, started events.RecordingMediaStarted, stop *events.RecordingStopped,
	pts []int64, maxStep time.Duration,
) {
	t.Helper()

	require.GreaterOrEqual(t, len(pts), 2, "%s: blocks", name)
	assert.Equal(t, started.MediaStartUTC, stop.MediaStartUTC, "%s: the stop's mediaStartUTC", name)
	assert.Equal(t, started.MediaStartHR, stop.MediaStartHR, "%s: the stop's mediaStartHR", name)

	n := len(pts)
	want := pts[n-1] - pts[0] + min(pts[n-1]-pts[n-2], maxStep.Milliseconds())
	assert.InDelta(t, want, stop.MediaEndUTC-stop.MediaStartUTC, 2, "%s: mediaEnd", name)
	assert.InDelta(t, stop.MediaEndUTC-stop.MediaStartUTC, stop.MediaEndHR-stop.MediaStartHR, 1,
		"%s: mediaEnd in both clocks", name)
}

// TestMediaTimestampsPlaceEachFrame records a camera and a microphone from
// one publisher, one session each, as bbb-webrtc-sfu does.
// record-and-playback plays a file's first frame at its start event's time.
// The rest follow by their PTS. So every frame must land where it was sent,
// at mediaStart plus its PTS since the first block.
//
// The first key frame the recorder gets loses a packet. The camera's file
// then starts at the next key frame, but the flowing event came with the
// first.
//
// Midway, the camera is muted for 3 s, as BBB's away mode does: its file
// runs on with a gap.
func TestMediaTimestampsPlaceEachFrame(t *testing.T) {
	const (
		cameraFPS     = 15
		keyframeDelay = 300 * time.Millisecond
		beforeMute    = 2 * time.Second
		muteFor       = 3 * time.Second
		afterMute     = 4 * time.Second
	)

	s := NewStack(t, StackOptions{})
	room := "e2e-mediatimes-" + uuid.NewString()[:8]
	pub := s.NewPublisher(room, "e2e-publisher")

	video, err := corpus.GenerateVP8(corpus.VP8Options{FPS: cameraFPS, Duration: 30 * time.Second})
	require.NoError(t, err)
	audio, err := corpus.GenerateOpus(corpus.OpusOptions{Duration: 30 * time.Second})
	require.NoError(t, err)

	var armed, dropped atomic.Bool
	cam := pub.PublishVP8(TrackOptions{
		Name: "camera", Schedule: video, KeyframeDelay: keyframeDelay, TagFrames: true,
		Drop: func(p SentPacket) bool {
			return armed.Load() && p.PLIKey && p.Index == 1 && dropped.CompareAndSwap(false, true)
		},
	})
	mic := pub.PublishOpus(TrackOptions{Name: "microphone", Schedule: audio, TagFrames: true})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	status, err := s.Driver.WaitRecorderStatus(ctx, 0)
	require.NoError(t, err)
	assert.Contains(t, status.Capabilities, events.CapabilityMediaTimestamps)

	require.NoError(t, pub.WaitPublished(ctx, cam, mic))

	armed.Store(true)
	recCam, err := s.Driver.StartRecording(ctx, room, cam.SID)
	require.NoError(t, err)
	recMic, err := s.Driver.StartRecording(ctx, room, mic.SID)
	require.NoError(t, err)

	for _, rec := range []*Recording{recCam, recMic} {
		_, err = s.Driver.WaitRtpStatus(ctx, rec.From, rec.SessionID, "flowing")
		require.NoError(t, err)
	}

	time.Sleep(beforeMute)
	cam.Pub.SetMuted(true)
	time.Sleep(muteFor)
	cam.Pub.SetMuted(false)
	time.Sleep(afterMute)

	stops := map[string]*events.RecordingStopped{}
	for _, rec := range []*Recording{recCam, recMic} {
		stops[rec.SessionID], err = s.Driver.StopAndWait(ctx, rec.SessionID)
		require.NoError(t, err)
	}

	require.True(t, dropped.Load(), "the first PLI key frame lost no packet")

	places := map[string]placement{}
	for _, c := range []struct {
		name    string
		rec     *Recording
		track   *PubTrack
		maxStep time.Duration
	}{{"camera", recCam, cam, maxVideoStep}, {"microphone", recMic, mic, maxAudioStep}} {
		sends := frameSends(c.track)
		p, ok := checkPlacement(ctx, t, s.Driver, c.name, c.rec, sends, stops[c.rec.SessionID], c.maxStep)
		places[c.name] = p
		if ok {
			assert.InDelta(t, hostClockOffset(t), p.Started.MediaStartHR-p.Started.MediaStartUTC, float64(hostClockStep.Milliseconds()),
				"%s: mediaStartHR is the host's CLOCK_MONOTONIC at mediaStartUTC", c.name)
		}

		firstSent := sends[p.Blocks[0].Frame]
		flowingAt := firstFlowing(s.Driver, c.rec.From, c.rec.SessionID)
		require.False(t, flowingAt.IsZero())
		t.Logf("%s: a start stamped on flowing places the first frame %v from where it was sent",
			c.name, flowingAt.Sub(firstSent))
		if c.name == "camera" {
			require.GreaterOrEqual(t, firstSent.Sub(flowingAt), keyframeDelay,
				"the camera's file must start after the key frame that follows the loss")

			var gap time.Duration
			for i := 1; i < len(p.Blocks); i++ {
				gap = max(gap, sends[p.Blocks[i].Frame].Sub(sends[p.Blocks[i-1].Frame]))
			}
			t.Logf("camera: the mute left %v between two blocks' sends", gap)
			assert.Greater(t, gap, maxVideoStep, "the camera's file must hold the mute's gap")
		}
	}

	assertLipSync(t, "lip sync", places["camera"], places["microphone"])
}
