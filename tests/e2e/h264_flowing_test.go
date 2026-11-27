//go:build e2e && linux

package e2e

import (
	"slices"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestH264FlowingFollowsFirstMedia is TestFlowingFollowsFirstMedia on an
// H.264 camera whose frames are tagged. LiveKit 1.13.7, which CI pins, gives
// the padding a 255-byte payload. It comes only before the first key frame,
// where the jitter buffer drops it, so the H.264 code that skips padding is
// not reached.
//
// Flowing must wait for the first media read. The file must begin with the
// key frame sent for the PLI, and the recorder must ask for no other. Each
// block must play where its frame was sent.
func TestH264FlowingFollowsFirstMedia(t *testing.T) {
	r := runFlowing(t, func(pub *Publisher) *PubTrack {
		return pub.PublishH264(TrackOptions{
			Name: "camera", Schedule: h264Camera(t, flowingCameraFPS, 30*time.Second),
			KeyframeDelay: flowingKeyframeDelay, TagFrames: true,
		})
	})

	padded := 0
	for _, rec := range r.Out.Trace.Records {
		if rec.T == corpus.RecordRTP && rec.Pad && rec.PLen > 0 {
			padded++
		}
	}

	mkv := r.Out.WebM
	t.Logf("padding with a payload: %d of %d; blocks=%d plis=%v", padded, r.Out.PadPackets, mkv.Blocks, r.Out.PLIs)

	require.Equal(t, oracle.CodecH264, mkv.Codec)
	assert.NoError(t, mkv.CheckH264())
	assert.Empty(t, r.Out.PLIs, "recorder keyframe requests")

	p, ok := checkPlacement(t.Context(), t, r.Stack.Driver, "camera", r.Rec, frameSends(r.Cam), r.Stopped, recorder.MaxVideoStep)
	sent := r.Cam.Sent()
	if i := slices.IndexFunc(sent, func(s SentPacket) bool { return s.PLIKey }); ok && assert.GreaterOrEqual(t, i, 0, "no key frame sent for a PLI") {
		assert.Equal(t, sent[i].Frame, p.Blocks[0].Frame, "the file's first frame")
	}
}
