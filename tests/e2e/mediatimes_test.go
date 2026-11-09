//go:build e2e && linux

package e2e

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/at-wat/ebml-go/mkvcore"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
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

type webmBlock struct {
	PTS  int64 // ms
	Data []byte
}

// readBlocks reads the blocks of the WebM at path, which must hold one track.
func readBlocks(t *testing.T, path string) []webmBlock {
	t.Helper()

	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	readers, err := mkvcore.NewSimpleBlockReader(f)
	require.NoError(t, err)
	require.Len(t, readers, 1)

	var blocks []webmBlock
	for {
		data, _, pts, err := readers[0].Read()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		blocks = append(blocks, webmBlock{PTS: pts, Data: data})
	}
	require.NotEmpty(t, blocks)

	return blocks
}

// readTaggedBlocks is readBlocks for a track of frames the publisher tagged.
func readTaggedBlocks(t *testing.T, path string) []taggedBlock {
	t.Helper()

	var blocks []taggedBlock
	for _, b := range readBlocks(t, path) {
		frame, ok := TaggedFrame(b.Data)
		require.True(t, ok, "block at %d ms has no frame tag", b.PTS)
		blocks = append(blocks, taggedBlock{PTS: b.PTS, Frame: frame})
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
	for _, m := range d.Messages()[from:] {
		var e events.RecordingRtpStatusChanged
		if m.ID == events.RecordingRtpStatusChangedKey && m.SessionID == sessionID &&
			json.Unmarshal(m.Raw, &e) == nil && e.Status == "flowing" {
			return m.At
		}
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
func TestMediaTimestampsPlaceEachFrame(t *testing.T) {
	const (
		cameraFPS     = 15
		keyframeDelay = 300 * time.Millisecond
		recordFor     = 6 * time.Second
		// Loopback adds about a millisecond from the publisher's write to the
		// recorder's read. LiveKit's forwarding and the read loop add a few
		// more, most for a key frame's packets. PTS are whole ms.
		placementBound = 40 * time.Millisecond
		// Both tracks take the same path: their placements may differ by
		// that path's jitter only.
		lipSyncBound = 15.0 // ms
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

	time.Sleep(recordFor)

	stops := map[string]*events.RecordingStopped{}
	for _, rec := range []*Recording{recCam, recMic} {
		stops[rec.SessionID], err = s.Driver.StopAndWait(ctx, rec.SessionID)
		require.NoError(t, err)
	}

	require.True(t, dropped.Load(), "the first PLI key frame lost no packet")

	medians := map[string]float64{}
	for _, c := range []struct {
		name    string
		rec     *Recording
		track   *PubTrack
		maxStep time.Duration
	}{{"camera", recCam, cam, maxVideoStep}, {"microphone", recMic, mic, maxAudioStep}} {
		blocks := readTaggedBlocks(t, c.rec.Path)
		sends := frameSends(c.track)
		firstSent, ok := sends[blocks[0].Frame]
		require.True(t, ok, "%s: first block's frame %d was not sent", c.name, blocks[0].Frame)

		flowingAt := firstFlowing(s.Driver, c.rec.From, c.rec.SessionID)
		require.False(t, flowingAt.IsZero())
		t.Logf("%s: %d blocks; a start stamped on flowing places the first frame %v from where it was sent",
			c.name, len(blocks), flowingAt.Sub(firstSent))
		if c.name == "camera" {
			require.GreaterOrEqual(t, firstSent.Sub(flowingAt), keyframeDelay,
				"the camera's file must start after the key frame that follows the loss")
		}

		// The recorder sends recordingMediaStarted before recordingStopped,
		// which has already arrived, so a short wait is enough.
		waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
		var started events.RecordingMediaStarted
		_, err := s.Driver.waitDecode(waitCtx, c.rec.From, events.RecordingMediaStartedKey, c.rec.SessionID, &started)
		waitCancel()
		if !assert.NoError(t, err, "%s: recordingMediaStarted", c.name) {
			continue
		}

		start := time.UnixMilli(started.MediaStartUTC)
		var mono unix.Timespec
		require.NoError(t, unix.ClockGettime(unix.CLOCK_MONOTONIC, &mono))
		hostOffset := time.Duration(mono.Nano()).Milliseconds() - time.Now().UnixMilli()
		assert.InDelta(t, hostOffset, started.MediaStartHR-started.MediaStartUTC, 2,
			"%s: mediaStartHR is the host's CLOCK_MONOTONIC at mediaStartUTC", c.name)

		var errs []time.Duration
		for _, b := range blocks {
			sent, ok := sends[b.Frame]
			require.True(t, ok, "%s: frame %d was not sent", c.name, b.Frame)

			placed := start.Add(time.Duration(b.PTS-blocks[0].PTS) * time.Millisecond)
			errs = append(errs, placed.Sub(sent))
		}

		worst := slices.MaxFunc(errs, func(a, b time.Duration) int { return int(a.Abs() - b.Abs()) })
		medians[c.name] = medianMs(errs)
		t.Logf("%s: mediaStart places frames %.1f ms (median) from where they were sent, %v at worst",
			c.name, medians[c.name], worst)
		assert.LessOrEqual(t, worst.Abs(), placementBound, "%s: worst placement", c.name)

		pts := make([]int64, 0, len(blocks))
		for _, b := range blocks {
			pts = append(pts, b.PTS)
		}
		assertMediaEnd(t, c.name, started, stops[c.rec.SessionID], pts, c.maxStep)
	}

	if len(medians) == 2 {
		t.Logf("lip sync: the camera lands %.1f ms after the microphone", medians["camera"]-medians["microphone"])
		assert.InDelta(t, medians["microphone"], medians["camera"], lipSyncBound, "lip sync")
	}
}
