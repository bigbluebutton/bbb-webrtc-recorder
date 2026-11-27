//go:build e2e && linux

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBrowserH264Camera publishes Chromium's fake camera in H.264 with BBB's
// low profile, as BBB's client would with videoCodec 'h264' in its camera
// publish options. Chromium encodes it with OpenH264. Each case records the
// camera for 10 s. The file must be .mkv, hold H.264 and pass the oracle's
// checks, and decode with ffmpeg as in TestH264ClipDecodes.
//
// The first case turns livekit-client's backup codec off, so LiveKit lists
// H.264 alone. The second keeps livekit-client's default, a VP8 backup. The
// recorder must still get H.264.
func TestBrowserH264Camera(t *testing.T) {
	RequireBrowser(t)

	for _, c := range []struct {
		name   string
		codec  CameraCodec
		listed []string
	}{
		{"no-backup", CameraCodec{Video: "h264", NoBackup: true}, []string{mimeH264}},
		{"vp8-backup", CameraCodec{Video: "h264"}, []string{mimeH264, "video/vp8"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			const (
				recordFor = 10 * time.Second
				// Frames read but not written at stop, as in the baseline.
				stopTailFrames = 1
			)

			s := NewStack(t, StackOptions{})
			room := "e2e-browser-h264-" + uuid.NewString()[:8]
			peer := s.NewBrowser().JoinAsBBB(room, "e2e-browser")
			cam := peer.PublishBBBCameraCodec("camera", "low", c.codec)

			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()

			listed, err := s.LiveKit.listedTracks(ctx, room, peer.Identity)
			require.NoError(t, err)
			require.Contains(t, listed, cam.SID)
			assert.Equal(t, c.listed, publicationCodecs(listed[cam.SID]), "the codecs LiveKit lists")

			rec := record(ctx, t, s, room, cam.SID)
			assert.Equal(t, []string{mimeH264}, rec.Codecs, "startRecordingResponse codecs")
			time.Sleep(recordFor)
			_, err = s.Driver.StopAndWait(ctx, rec.SessionID)
			require.NoError(t, err)

			stats := peer.SenderStats(cam.SID)
			require.NotEmpty(t, stats, "Chromium's senders of the camera")
			for rid, st := range stats {
				t.Logf("sender %q: %s by %s, %d packets, %d PLIs", rid, st.Codec, st.EncoderImplementation, st.PacketsSent, st.PLICount)
				assert.True(t, strings.EqualFold(mimeH264, st.Codec), "sender %q codec %s", rid, st.Codec)
			}

			out := ReadOutput(t, rec, cam.SID)
			mkv := out.WebM
			t.Logf("mkv: blocks=%d h264Keyframes=%d size=%dx%d pts=[%d,%d] maxGap=%dms; trace: frames=%d plis=%v",
				mkv.Blocks, mkv.H264Keyframes, mkv.Width, mkv.Height, mkv.FirstPTS, mkv.LastPTS, mkv.MaxGapMs,
				out.ReadFrames, out.PLIs)

			require.Equal(t, oracle.CodecH264, mkv.Codec)
			assert.Equal(t, corpus.CodecH264, out.Trace.Header.Codec, "trace codec")
			assert.NoError(t, mkv.CheckH264())
			assert.NoError(t, mkv.CheckMonotonic())
			assert.NoError(t, mkv.CheckNoDuplicatePTS())
			assert.Empty(t, out.PLIs, "recorder keyframe requests")
			assert.LessOrEqual(t, mkv.Blocks, out.ReadFrames, "frames written but never read")
			assert.LessOrEqual(t, out.ReadFrames-mkv.Blocks, stopTailFrames, "frames read but not written")

			require.GreaterOrEqual(t, len(mkv.PTS), 2, "blocks")
			last := time.Duration(mkv.PTS[len(mkv.PTS)-1]-mkv.PTS[len(mkv.PTS)-2]) * time.Millisecond
			span := time.Duration(mkv.LastPTS-mkv.FirstPTS)*time.Millisecond + last
			t.Run("ffmpeg", func(t *testing.T) {
				checkDecodes(t, rec.Path, mkv.Blocks, span, time.Duration(mkv.MaxGapMs)*time.Millisecond)
			})
		})
	}
}
