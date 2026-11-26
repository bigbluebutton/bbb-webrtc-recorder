//go:build e2e && linux

package e2e

import (
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// h264CameraLayers are bbbCameraLayers for an H.264 camera. GenerateH264
// takes even sizes only, so q is 450 wide.
var h264CameraLayers = []*livekit.VideoLayer{
	{Quality: livekit.VideoQuality_LOW, Width: 450, Height: 338, Bitrate: 100_000},
	{Quality: livekit.VideoQuality_MEDIUM, Width: 640, Height: 480, Bitrate: 200_000},
}

// TestH264SimulcastMediaTimestampsPlaceEachFrame is
// TestSimulcastMediaTimestampsPlaceEachFrame on an H.264 camera, each layer
// its own H.264 stream. Across both switches, the camera's file must also
// pass the oracle's H.264 checks.
func TestH264SimulcastMediaTimestampsPlaceEachFrame(t *testing.T) {
	r := runSimulcast(t, func(pub *Publisher) []*PubTrack {
		q, err := corpus.GenerateH264(corpus.H264Options{
			FPS: simulcastCameraFPS, Duration: 30 * time.Second, Width: 450, Height: 338,
			KeySlicePackets: 2, DeltaSlicePackets: 1, StartSeq: 31000, StartTS: 0x2a6b_19c4,
		})
		require.NoError(t, err)
		h, err := corpus.GenerateH264(corpus.H264Options{
			FPS: simulcastCameraFPS, Duration: 30 * time.Second, Width: 640, Height: 480,
			KeySlicePackets: 3, DeltaSlicePackets: 2, SpreadUs: 2000, StartSeq: 4000, StartTS: 0x8d10_7e22,
		})
		require.NoError(t, err)

		return pub.PublishH264Simulcast("camera",
			TrackOptions{Layer: h264CameraLayers[0], Schedule: q, KeyframeDelay: simulcastKeyframeDelay, TagFrames: true, TagOffset: simulcastQTagOffset},
			TrackOptions{Layer: h264CameraLayers[1], Schedule: h, KeyframeDelay: simulcastKeyframeDelay, TagFrames: true},
		)
	})

	out := ReadOutput(t, r.Rec, r.Cam.SID)
	mkv := out.WebM
	t.Logf("mkv: blocks=%d h264Keyframes=%d size=%dx%d pts=[%d,%d] maxGap=%dms; trace: frames=%d plis=%v",
		mkv.Blocks, mkv.H264Keyframes, mkv.Width, mkv.Height, mkv.FirstPTS, mkv.LastPTS, mkv.MaxGapMs, out.ReadFrames, out.PLIs)

	require.Equal(t, oracle.CodecH264, mkv.Codec)
	assert.NoError(t, mkv.CheckH264())
	assert.NoError(t, mkv.CheckMonotonic())
}
