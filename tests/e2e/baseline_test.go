//go:build e2e && linux

package e2e

import (
	"context"
	"slices"
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
	metricWrittenSamples = "recorder_written_samples_total"
	metricPLIRequests    = "recorder_pli_requests_total"
)

// TestBaselineRecording records one camera and one microphone, one session per
// track as the SFU does, for about 10 s on a clean loopback path.
func TestBaselineRecording(t *testing.T) {
	const (
		cameraFPS = 15
		opusFPS   = 50 // 20 ms frames
		recordFor = 10 * time.Second
		// Long enough that the schedule does not loop during the run.
		scheduleLen = 30 * time.Second

		// A session's first frame waits for the recorder's subscription
		// (ICE/DTLS on loopback, 0.1-0.3 s measured), and on video also for
		// LiveKit's PLI and the publisher's next frame. 1 s leaves room for a
		// slow runner.
		startupAllowance = time.Second
		// Frames read but not written at stop: at most the one that was only
		// partly received when the room closed.
		stopTailFrames = 1
	)

	s := NewStack(t, StackOptions{})
	room := "e2e-baseline-" + uuid.NewString()[:8]
	pub := s.NewPublisher(room, "e2e-publisher")

	video, err := corpus.GenerateVP8(corpus.VP8Options{FPS: cameraFPS, Duration: scheduleLen, SpreadUs: 2000})
	require.NoError(t, err)
	audio, err := corpus.GenerateOpus(corpus.OpusOptions{Duration: scheduleLen})
	require.NoError(t, err)

	cam := pub.PublishVP8(TrackOptions{Name: "camera", Schedule: video})
	mic := pub.PublishOpus(TrackOptions{Name: "microphone", Schedule: audio})

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	require.NoError(t, pub.WaitPublished(ctx, cam, mic))
	t.Logf("tracks published %s after the camera started", time.Since(cam.Start()))

	base, err := ScrapeMetrics(ctx, s.Recorder.MetricsURL)
	require.NoError(t, err)

	startSent := time.Now()
	camRec, err := s.Driver.StartRecording(ctx, room, cam.SID)
	require.NoError(t, err)
	micRec, err := s.Driver.StartRecording(ctx, room, mic.SID)
	require.NoError(t, err)
	started := time.Now()
	t.Logf("both sessions started in %s", started.Sub(startSent))

	for _, rec := range []*Recording{camRec, micRec} {
		_, err := s.Driver.WaitRtpStatus(ctx, rec.From, rec.SessionID, "flowing")
		require.NoError(t, err)
	}
	t.Logf("both flowing %s after start", time.Since(started))

	time.Sleep(time.Until(started.Add(recordFor)))

	stopSent := time.Now()
	stopFrom := len(s.Driver.Messages())
	stopped := map[string]*events.RecordingStopped{}
	for _, rec := range []*Recording{camRec, micRec} {
		require.NoError(t, s.Driver.Stop(rec.SessionID))
	}
	for _, rec := range []*Recording{camRec, micRec} {
		st, err := s.Driver.WaitStopped(ctx, stopFrom, rec.SessionID)
		require.NoError(t, err)
		stopped[rec.SessionID] = st
	}
	stoppedAt := time.Now()
	t.Logf("both stopped %s after stopRecording", stoppedAt.Sub(stopSent))

	// The recorder updates its capture metrics as a session stops, before
	// it sends recordingStopped.
	after, err := ScrapeMetrics(ctx, s.Recorder.MetricsURL)
	require.NoError(t, err)
	delta := after.Delta(base)

	for _, c := range []struct {
		name   string
		rec    *Recording
		track  *PubTrack
		codec  string
		kind   corpus.Kind
		source string
		fps    float64
	}{
		{"camera", camRec, cam, oracle.CodecVP8, corpus.KindVideo, "CAMERA", cameraFPS},
		{"microphone", micRec, mic, oracle.CodecOpus, corpus.KindAudio, "MICROPHONE", opusFPS},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := ReadOutput(t, c.rec, c.track.SID)
			webm := out.WebM

			labels := map[string]string{"source": c.source, "kind": string(c.kind)}
			written := int(delta.Sum(metricWrittenSamples, labels))
			plis := int(delta.Sum(metricPLIRequests, map[string]string{"source": c.source}))
			// What the publisher sent while the session was open.
			sent, _ := c.track.SentBetween(startSent.Sub(c.track.Start()), stoppedAt.Sub(c.track.Start()))

			t.Logf("webm: blocks=%d keyBlocks=%d vp8Keyframes=%d pts=[%d,%d] maxGap=%dms; "+
				"trace: frames=%d packets=%d pad=%d rtx=%d plis=%v; metrics: written=%d plis=%d; "+
				"publisher: sent=%d plis=%v",
				webm.Blocks, webm.KeyBlocks, webm.VP8Keyframes, webm.FirstPTS, webm.LastPTS, webm.MaxGapMs,
				out.ReadFrames, out.ReadPackets, out.PadPackets, out.RTXPackets, out.PLIs, written, plis,
				sent, c.track.PLIs())

			assert.Equal(t, events.StopReasonNormal, stopped[c.rec.SessionID].Reason)

			// WebM: the codec, the frame count, and well-formed timestamps.
			require.Equal(t, c.codec, webm.Codec)
			minFrames := int(c.fps * (stopSent.Sub(started) - startupAllowance).Seconds())
			assert.GreaterOrEqual(t, webm.Blocks, minFrames, "frames written")
			assert.LessOrEqual(t, webm.Blocks, sent, "more frames written than sent")
			assert.NoError(t, webm.CheckMonotonic())
			assert.NoError(t, webm.CheckNoDuplicatePTS())
			// PTS come from RTP timestamps: a step over two frame intervals
			// is a frame lost on a path that loses nothing.
			assert.LessOrEqual(t, webm.MaxGapMs, int64(2*1000/c.fps), "PTS gap")

			if c.kind == corpus.KindVideo {
				// The WebM keyframe flag is not checked: WebmRecorder does not set it for VP8.
				assert.GreaterOrEqual(t, webm.VP8Keyframes, 1, "VP8 key frames")
				// LiveKit starts the recorder's downtrack at a key frame it asks
				// for; the schedule's own key frame went out before anyone listened.
				assert.NotEmpty(t, c.track.PLIs(), "publisher PLIs")
				assert.True(t, slices.ContainsFunc(c.track.Sent(), func(s SentPacket) bool { return s.PLIKey }),
					"no key frame sent for a PLI")
				// Nothing is lost, so the recorder has no reason to ask for one.
				assert.Empty(t, out.PLIs, "recorder keyframe requests")
			}

			// Trace: a valid capture of this track, ended cleanly, whose reads
			// account for what was written.
			h := out.Trace.Header
			assert.Equal(t, corpus.ProvenanceCapture, h.Provenance)
			assert.Equal(t, c.track.SID, h.TrackID)
			assert.Equal(t, c.kind, h.Kind)
			if assert.NotNil(t, out.EOF, "trace eof") {
				assert.Empty(t, out.EOF.Reason, "trace truncated or dropped records")
			}
			assert.LessOrEqual(t, webm.Blocks, out.ReadFrames, "frames written but never read")
			assert.LessOrEqual(t, out.ReadFrames-webm.Blocks, stopTailFrames, "frames read but not written")

			// Metrics are taken after the recorder closes its file, so they
			// must match it.
			assert.Equal(t, webm.Blocks, written, "%s %v", metricWrittenSamples, labels)
			assert.Equal(t, out.PLIs["packet_drop"]+out.PLIs["direct_request"], plis, metricPLIRequests)
		})
	}
}
