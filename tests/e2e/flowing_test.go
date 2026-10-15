//go:build e2e && linux

package e2e

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFlowingFollowsFirstMedia records a camera whose publisher answers the
// recorder's keyframe request late, so LiveKit sends the recorder padding
// before the first key frame. bbb-webrtc-sfu stamps the recording's start on
// the first flowing event, so that event must not come before the first media
// packet the recorder read. Only LiveKit 1.13.5 to 1.13.9 give that padding a
// payload, so only they fail this on a recorder that takes a payload for media.
func TestFlowingFollowsFirstMedia(t *testing.T) {
	const (
		cameraFPS = 15
		// Longer than LiveKit takes to send two padding packets (100 ms
		// after the downtrack binds, then every 100 ms), so its padding
		// reaches the recorder before the first key frame.
		keyframeDelay = 400 * time.Millisecond
		recordFor     = 3 * time.Second
	)

	s := NewStack(t, StackOptions{})
	room := "e2e-flowing-" + uuid.NewString()[:8]
	pub := s.NewPublisher(room, "e2e-publisher")

	video, err := corpus.GenerateVP8(corpus.VP8Options{FPS: cameraFPS, Duration: 30 * time.Second, SpreadUs: 2000})
	require.NoError(t, err)
	cam := pub.PublishVP8(TrackOptions{Name: "camera", Schedule: video, KeyframeDelay: keyframeDelay})

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	require.NoError(t, pub.WaitPublished(ctx, cam))

	rec, err := s.Driver.StartRecording(ctx, room, cam.SID)
	require.NoError(t, err)
	_, err = s.Driver.WaitRtpStatus(ctx, rec.From, rec.SessionID, "flowing")
	require.NoError(t, err)
	time.Sleep(recordFor)
	_, err = s.Driver.StopAndWait(ctx, rec.SessionID)
	require.NoError(t, err)

	var flowingAt time.Time
	for _, m := range s.Driver.Messages()[rec.From:] {
		var e events.RecordingRtpStatusChanged
		if m.ID == events.RecordingRtpStatusChangedKey && m.SessionID == rec.SessionID &&
			json.Unmarshal(m.Raw, &e) == nil && e.Status == "flowing" {
			flowingAt = m.At

			break
		}
	}
	require.False(t, flowingAt.IsZero(), "no flowing event")

	out := ReadOutput(t, rec, cam.SID)
	epoch := time.UnixMicro(out.Trace.Header.EpochUnixUs)
	var firstMedia time.Time
	padBefore := 0
	for _, r := range out.Trace.Records {
		if r.T != corpus.RecordRTP {
			continue
		}
		if r.Pad {
			padBefore++

			continue
		}
		firstMedia = epoch.Add(time.Duration(r.AtUs) * time.Microsecond)

		break
	}
	require.False(t, firstMedia.IsZero(), "no media read")

	t.Logf("padding before the first media read: %d; flowing received %v after it", padBefore, flowingAt.Sub(firstMedia))

	// Without padding first, the scenario cannot tell padding from media.
	require.Positive(t, padBefore, "no padding before the first media read")
	assert.False(t, flowingAt.Before(firstMedia), "flowing %v before the first media read", firstMedia.Sub(flowingAt))
}
