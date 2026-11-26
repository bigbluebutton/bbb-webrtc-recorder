//go:build e2e && linux

package e2e

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const mimeH264 = "video/h264"

// h264Camera is the H.264 schedule the synthetic scenarios publish. Each
// frame has two slices, sent as FU-A fragments, so a frame spans packets as
// a real one does.
func h264Camera(t *testing.T, fps float64, d time.Duration) *corpus.Trace {
	t.Helper()

	sched, err := corpus.GenerateH264(corpus.H264Options{
		FPS: fps, Duration: d, Slices: 2, KeySlicePackets: 3, DeltaSlicePackets: 2, SpreadUs: 2000,
	})
	require.NoError(t, err)

	return sched
}

// TestH264BaselineRecording records an H.264 camera for about 8 s on a clean
// loopback path, one session, as TestBaselineRecording does for VP8. The
// file must be Matroska and hold each frame sent while it ran. Every block
// must pass the oracle's H.264 checks and land where its frame was sent.
func TestH264BaselineRecording(t *testing.T) {
	const (
		cameraFPS = 15
		recordFor = 8 * time.Second
		// Long enough that the schedule does not loop during the run.
		scheduleLen = 30 * time.Second
		// As in the baseline: the subscription, LiveKit's PLI and the
		// publisher's next frame come before the first frame.
		startupAllowance = time.Second
		stopTailFrames   = 1
	)

	s := NewStack(t, StackOptions{})
	room := "e2e-h264-" + uuid.NewString()[:8]
	pub := s.NewPublisher(room, "e2e-publisher")
	cam := pub.PublishH264(TrackOptions{Name: "camera", Schedule: h264Camera(t, cameraFPS, scheduleLen), TagFrames: true})

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	require.NoError(t, pub.WaitPublished(ctx, cam))

	listed, err := s.LiveKit.listedTracks(ctx, room, pub.Identity)
	require.NoError(t, err)
	require.Contains(t, listed, cam.SID)
	assert.True(t, strings.EqualFold(webrtc.MimeTypeH264, listed[cam.SID].MimeType), "LiveKit lists %s", listed[cam.SID].MimeType)

	base, err := ScrapeMetrics(ctx, s.Recorder.MetricsURL)
	require.NoError(t, err)

	startSent := time.Now()
	rec, err := s.Driver.StartRecording(ctx, room, cam.SID)
	require.NoError(t, err)
	started := time.Now()
	assert.Equal(t, []string{mimeH264}, rec.Codecs, "startRecordingResponse codecs")

	_, err = s.Driver.WaitRtpStatus(ctx, rec.From, rec.SessionID, "flowing")
	require.NoError(t, err)

	time.Sleep(time.Until(started.Add(recordFor)))

	stopSent := time.Now()
	stop, err := s.Driver.StopAndWait(ctx, rec.SessionID)
	require.NoError(t, err)
	stoppedAt := time.Now()
	assert.Equal(t, events.StopReasonNormal, stop.Reason)

	after, err := ScrapeMetrics(ctx, s.Recorder.MetricsURL)
	require.NoError(t, err)
	delta := after.Delta(base)

	out := ReadOutput(t, rec, cam.SID)
	mkv := out.WebM
	// The metrics name the codec as the publication does.
	labels := map[string]string{"source": "CAMERA", "kind": string(corpus.KindVideo), "mime": webrtc.MimeTypeH264}
	written := int(delta.Sum(metricWrittenSamples, labels))
	sent, _ := cam.SentBetween(startSent.Sub(cam.Start()), stoppedAt.Sub(cam.Start()))

	t.Logf("mkv: blocks=%d keyBlocks=%d h264Keyframes=%d size=%dx%d pts=[%d,%d] maxGap=%dms; "+
		"trace: frames=%d packets=%d pad=%d rtx=%d plis=%v; metrics: written=%d; publisher: sent=%d plis=%v",
		mkv.Blocks, mkv.KeyBlocks, mkv.H264Keyframes, mkv.Width, mkv.Height, mkv.FirstPTS, mkv.LastPTS, mkv.MaxGapMs,
		out.ReadFrames, out.ReadPackets, out.PadPackets, out.RTXPackets, out.PLIs, written, sent, cam.PLIs())

	require.Equal(t, oracle.CodecH264, mkv.Codec)
	assert.Equal(t, ".mkv", filepath.Ext(rec.Path))
	assert.NoError(t, mkv.CheckH264())
	assert.Equal(t, [2]int{640, 360}, [2]int{mkv.Width, mkv.Height}, "the SPS's size in the track entry")
	assert.GreaterOrEqual(t, mkv.H264Keyframes, 1, "H.264 key frames")

	minFrames := int(cameraFPS * (stopSent.Sub(started) - startupAllowance).Seconds())
	assert.GreaterOrEqual(t, mkv.Blocks, minFrames, "frames written")
	assert.LessOrEqual(t, mkv.Blocks, sent, "more frames written than sent")
	assert.NoError(t, mkv.CheckMonotonic())
	assert.NoError(t, mkv.CheckNoDuplicatePTS())
	assert.LessOrEqual(t, mkv.MaxGapMs, int64(2*1000/cameraFPS), "PTS gap")

	// LiveKit starts the downtrack at a key frame it asks for, and nothing
	// is lost after it.
	assert.NotEmpty(t, cam.PLIs(), "publisher PLIs")
	assert.Empty(t, out.PLIs, "recorder keyframe requests")

	h := out.Trace.Header
	assert.Equal(t, corpus.CodecH264, h.Codec, "trace codec")
	assert.Equal(t, cam.SID, h.TrackID)
	assertTraceEnded(t, "camera", out)
	assert.LessOrEqual(t, mkv.Blocks, out.ReadFrames, "frames written but never read")
	assert.LessOrEqual(t, out.ReadFrames-mkv.Blocks, stopTailFrames, "frames read but not written")
	assert.Equal(t, mkv.Blocks, written, "%s %v", metricWrittenSamples, labels)

	p, _ := checkPlacement(ctx, t, s.Driver, "camera", rec, frameSends(cam), stop, recorder.MaxVideoStep)

	// With nothing lost, each block holds the frame after the one before
	// it. The PTS gap bound alone passes a file that misses one frame.
	var breaks []string
	for i := 1; i < len(p.Blocks); i++ {
		if prev, cur := p.Blocks[i-1].Frame, p.Blocks[i].Frame; cur != prev+1 {
			breaks = append(breaks, fmt.Sprintf("frame %d after frame %d", cur, prev))
		}
	}
	assert.Empty(t, breaks, "blocks whose frame does not follow the block before")
}
