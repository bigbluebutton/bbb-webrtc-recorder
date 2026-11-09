//go:build e2e && linux

package e2e

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bbbCameraLayers are the layers BBB's client publishes a 640x480 camera
// with under its default profile, "medium" (200 kbps). The lower layer
// comes from the "low" profile (100 kbps), which livekit-client signals as
// LOW (rid q) at 451x338. The top layer is MEDIUM (rid h). The recorder asks
// for HIGH and gets h.
var bbbCameraLayers = []*livekit.VideoLayer{
	{Quality: livekit.VideoQuality_LOW, Width: 451, Height: 338, Bitrate: 100_000},
	{Quality: livekit.VideoQuality_MEDIUM, Width: 640, Height: 480, Bitrate: 200_000},
}

// TestSimulcastMediaTimestampsPlaceEachFrame records a camera published
// with BBB's default layers, and a microphone, one session each. Midway,
// the camera stops sending h for a while, as a browser does when its
// bandwidth estimate drops. LiveKit moves the recorder to q, then back to h
// once h has flowed again for long enough.
//
// LiveKit rewrites the timestamps of the layer it switches to, through the
// publisher's sender reports. Every frame must still land where it was
// sent, as in TestMediaTimestampsPlaceEachFrame, whichever layer it came
// from.
func TestSimulcastMediaTimestampsPlaceEachFrame(t *testing.T) {
	const (
		cameraFPS     = 20
		keyframeDelay = 100 * time.Millisecond
		// Before h stops, and for how long.
		beforePause = 3 * time.Second
		pauseFor    = 3 * time.Second
		// LiveKit takes h back once it has flowed for 10 s.
		upSwitchWait = 20 * time.Second
		afterUp      = 2 * time.Second
		// q's frames are tagged from here, h's from 0.
		qTagOffset = 1_000_000
		// As in TestMediaTimestampsPlaceEachFrame.
		placementBound = 40 * time.Millisecond
		lipSyncBound   = 15.0 // ms
	)

	s := NewStack(t, StackOptions{})
	room := "e2e-simulcast-" + uuid.NewString()[:8]
	pub := s.NewPublisher(room, "e2e-publisher")

	// Each layer is its own stream, with sequence numbers, timestamps and
	// picture IDs of its own, as libwebrtc sends them.
	q, err := corpus.GenerateVP8(corpus.VP8Options{
		FPS: cameraFPS, Duration: 30 * time.Second, Width: 451, Height: 338,
		KeyPackets: 2, DeltaPackets: 1, StartSeq: 31000, StartTS: 0x2a6b_19c4, StartPictureID: 7000,
	})
	require.NoError(t, err)
	h, err := corpus.GenerateVP8(corpus.VP8Options{
		FPS: cameraFPS, Duration: 30 * time.Second, Width: 640, Height: 480,
		StartSeq: 4000, StartTS: 0x8d10_7e22, StartPictureID: 21000,
	})
	require.NoError(t, err)
	audio, err := corpus.GenerateOpus(corpus.OpusOptions{Duration: 30 * time.Second})
	require.NoError(t, err)

	layers := pub.PublishVP8Simulcast("camera",
		TrackOptions{Layer: bbbCameraLayers[0], Schedule: q, KeyframeDelay: keyframeDelay, TagFrames: true, TagOffset: qTagOffset},
		TrackOptions{Layer: bbbCameraLayers[1], Schedule: h, KeyframeDelay: keyframeDelay, TagFrames: true},
	)
	camQ, camH := layers[0], layers[1]
	mic := pub.PublishOpus(TrackOptions{Name: "microphone", Schedule: audio, TagFrames: true})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	require.NoError(t, pub.WaitPublished(ctx, camH, mic))

	recCam, err := s.Driver.StartRecording(ctx, room, camH.SID)
	require.NoError(t, err)
	recMic, err := s.Driver.StartRecording(ctx, room, mic.SID)
	require.NoError(t, err)

	for _, rec := range []*Recording{recCam, recMic} {
		_, err = s.Driver.WaitRtpStatus(ctx, rec.From, rec.SessionID, "flowing")
		require.NoError(t, err)
	}

	time.Sleep(beforePause)
	camH.Pause()
	paused := time.Since(camH.Start())
	time.Sleep(pauseFor)
	camH.Resume()
	resumed := time.Since(camH.Start())

	// LiveKit asks h for a key frame when it takes it back.
	require.Eventually(t, func() bool {
		return slices.ContainsFunc(camH.PLIs(), func(at time.Duration) bool { return at > resumed })
	}, upSwitchWait, 100*time.Millisecond, "LiveKit did not take h back")
	time.Sleep(afterUp)

	stops := map[string]*events.RecordingStopped{}
	for _, rec := range []*Recording{recCam, recMic} {
		stops[rec.SessionID], err = s.Driver.StopAndWait(ctx, rec.SessionID)
		require.NoError(t, err)
	}

	// Frames by tag, with when their first packet was due.
	camSends := frameSends(camH)
	for frame, at := range frameSends(camQ) {
		camSends[qTagOffset+frame] = at
	}

	medians := map[string]float64{}
	for _, c := range []struct {
		name    string
		rec     *Recording
		sends   map[int]time.Time
		maxStep time.Duration
	}{{"camera", recCam, camSends, maxVideoStep}, {"microphone", recMic, frameSends(mic), maxAudioStep}} {
		blocks := readTaggedBlocks(t, c.rec.Path)

		// As in TestMediaTimestampsPlaceEachFrame.
		waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
		var started events.RecordingMediaStarted
		_, err := s.Driver.waitDecode(waitCtx, c.rec.From, events.RecordingMediaStartedKey, c.rec.SessionID, &started)
		waitCancel()
		require.NoError(t, err, "%s: recordingMediaStarted", c.name)
		start := time.UnixMilli(started.MediaStartUTC)

		var errs []time.Duration
		for _, b := range blocks {
			sent, ok := c.sends[b.Frame]
			require.True(t, ok, "%s: frame %d was not sent", c.name, b.Frame)

			placed := start.Add(time.Duration(b.PTS-blocks[0].PTS) * time.Millisecond)
			errs = append(errs, placed.Sub(sent))
		}

		worst := slices.MaxFunc(errs, func(a, b time.Duration) int { return int(a.Abs() - b.Abs()) })
		medians[c.name] = medianMs(errs)
		t.Logf("%s: %d blocks; mediaStart places frames %.1f ms (median) from where they were sent, %v at worst",
			c.name, len(blocks), medians[c.name], worst)
		assert.LessOrEqual(t, worst.Abs(), placementBound, "%s: worst placement", c.name)

		pts := make([]int64, 0, len(blocks))
		for _, b := range blocks {
			pts = append(pts, b.PTS)
		}
		assertMediaEnd(t, c.name, started, stops[c.rec.SessionID], pts, c.maxStep)

		if c.name != "camera" {
			continue
		}

		// The file must hold both switches: from h to q while h was stopped,
		// and from q back to h after it resumed.
		var down, up bool
		prev := ""
		for _, b := range blocks {
			layer, sentAt := "h", camSends[b.Frame].Sub(camH.Start())
			if b.Frame >= qTagOffset {
				layer = "q"
			}

			if layer != prev {
				t.Logf("camera: from %s from frame %d, sent %v after the camera started", layer, b.Frame%qTagOffset, sentAt)
			}

			down = down || (prev == "h" && layer == "q" && sentAt > paused)
			up = up || (down && prev == "q" && layer == "h" && sentAt > resumed)
			prev = layer
		}

		assert.True(t, down, "camera: no switch from h to q while h was stopped")
		assert.True(t, up, "camera: no switch from q back to h")
	}

	if len(medians) == 2 {
		t.Logf("lip sync: the camera lands %.1f ms after the microphone", medians["camera"]-medians["microphone"])
		assert.InDelta(t, medians["microphone"], medians["camera"], lipSyncBound, "lip sync")
	}
}
