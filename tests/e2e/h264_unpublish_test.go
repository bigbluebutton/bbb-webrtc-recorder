//go:build e2e && linux

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestH264UnpublishWritesNoBlankFrames records the H.264 clip, then the
// publisher unpublishes the camera, as a browser does when its user stops
// sharing. LiveKit closes the recorder's downtrack, and sends it a few blank
// 2x2 key frames first. The driver then stops the session, as the SFU does
// on track_unpublished. The blank frames must stay out of the file, which
// must hold only the clip's pictures.
func TestH264UnpublishWritesNoBlankFrames(t *testing.T) {
	const (
		recordFor = 3 * time.Second
		// The SFU's stop follows LiveKit's track_unpublished webhook.
		stopAfterUnpublish = 500 * time.Millisecond
		frameInterval      = time.Second / h264ClipFPS
	)

	s := NewStack(t, StackOptions{})
	room := "e2e-h264-unpublish-" + uuid.NewString()[:8]
	pub := s.NewPublisher(room, "e2e-publisher")
	cam := pub.PublishH264(TrackOptions{Name: "camera", Schedule: h264ClipSchedule(t), KeyframeFromSchedule: true})

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	require.NoError(t, pub.WaitPublished(ctx, cam))

	rec := record(ctx, t, s, room, cam.SID)
	time.Sleep(recordFor)

	require.NoError(t, pub.Room.LocalParticipant.UnpublishTrack(cam.SID))
	require.NoError(t, waitTracksUnlisted(ctx, s.LiveKit, room, pub.Identity, cam.SID))
	time.Sleep(stopAfterUnpublish)
	unrecord(ctx, t, s, "camera", rec)

	out := ReadOutput(t, rec, cam.SID)
	mkv := out.WebM

	// pion's payloader puts only an SPS and a PPS in a STAP-A, ahead of the
	// IDR's fragments. So a STAP-A that ends a frame is a blank frame.
	blank := 0
	for _, r := range out.Trace.Records {
		if r.T == corpus.RecordRTP && !r.Pad && r.M && len(r.Pay) > 0 && r.Pay[0]&0x1f == 24 {
			blank++
		}
	}

	t.Logf("mkv: blocks=%d h264Keyframes=%d size=%dx%d pts=[%d,%d] maxGap=%dms; trace: frames=%d blank=%d plis=%v",
		mkv.Blocks, mkv.H264Keyframes, mkv.Width, mkv.Height, mkv.FirstPTS, mkv.LastPTS, mkv.MaxGapMs,
		out.ReadFrames, blank, out.PLIs)

	assert.Positive(t, blank, "LiveKit's blank frames read")
	require.Equal(t, oracle.CodecH264, mkv.Codec)
	assert.NoError(t, mkv.CheckH264())
	assert.Equal(t, [2]int{640, 360}, [2]int{mkv.Width, mkv.Height}, "the SPS's size in the track entry")
	assert.NoError(t, mkv.CheckMonotonic())
	assert.NoError(t, mkv.CheckNoDuplicatePTS())
	assert.LessOrEqual(t, mkv.MaxGapMs, int64(2*1000/h264ClipFPS), "PTS gap")

	span := time.Duration(mkv.LastPTS-mkv.FirstPTS)*time.Millisecond + frameInterval
	t.Run("ffmpeg", func(t *testing.T) { checkDecodes(t, rec.Path, mkv.Blocks, span, frameInterval) })
	t.Run("pictures", func(t *testing.T) { checkClipPictures(t, rec.Path, h264ClipPath, mkv.Blocks) })
}
