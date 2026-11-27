//go:build e2e && linux

package e2e

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBrowserVP8CameraWithH264Backup publishes Chromium's fake camera in VP8
// with BBB's low profile and an H.264 backup codec, which BBB's client does
// not set. LiveKit lists both codecs, and Chromium sends VP8 alone. The
// recorder writes the VP8 track to an .mkv file, which could also hold the
// backup. The start response names VP8 only, so the file's extension must
// come from the codecs LiveKit lists.
func TestBrowserVP8CameraWithH264Backup(t *testing.T) {
	RequireBrowser(t)

	const (
		recordFor      = 5 * time.Second
		stopTailFrames = 1 // as in TestBrowserH264Camera
	)

	mimeVP8 := strings.ToLower(webrtc.MimeTypeVP8)

	s := NewStack(t, StackOptions{})
	room := "e2e-browser-vp8-backup-" + uuid.NewString()[:8]
	peer := s.NewBrowser().JoinAsBBB(room, "e2e-browser")
	cam := peer.PublishBBBCameraCodec("camera", "low", CameraCodec{Video: "vp8", Backup: "h264"})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	listed, err := s.LiveKit.listedTracks(ctx, room, peer.Identity)
	require.NoError(t, err)
	require.Contains(t, listed, cam.SID)
	assert.Equal(t, []string{mimeVP8, mimeH264}, publicationCodecs(listed[cam.SID]), "the codecs LiveKit lists")

	rec := record(ctx, t, s, room, cam.SID)
	assert.Equal(t, []string{mimeVP8}, rec.Codecs, "startRecordingResponse codecs")
	assert.Equal(t, ".mkv", filepath.Ext(rec.Path), "startRecordingResponse fileName")
	time.Sleep(recordFor)
	_, err = s.Driver.StopAndWait(ctx, rec.SessionID)
	require.NoError(t, err)

	stats := peer.SenderStats(cam.SID)
	require.NotEmpty(t, stats, "Chromium's senders of the camera")
	for rid, st := range stats {
		t.Logf("sender %q: %s by %s, %d packets, %d PLIs", rid, st.Codec, st.EncoderImplementation, st.PacketsSent, st.PLICount)
		assert.True(t, strings.EqualFold(mimeVP8, st.Codec), "sender %q codec %s", rid, st.Codec)
	}

	out := ReadOutput(t, rec, cam.SID)
	mkv := out.WebM
	t.Logf("mkv: codec=%s blocks=%d vp8Keyframes=%d pts=[%d,%d] maxGap=%dms; trace: frames=%d plis=%v",
		mkv.Codec, mkv.Blocks, mkv.VP8Keyframes, mkv.FirstPTS, mkv.LastPTS, mkv.MaxGapMs, out.ReadFrames, out.PLIs)

	require.Equal(t, oracle.CodecVP8, mkv.Codec)
	assert.Equal(t, corpus.CodecVP8, out.Trace.Header.Codec, "trace codec")
	assert.Positive(t, mkv.VP8Keyframes, "VP8 key frames")
	assert.NoError(t, mkv.CheckMonotonic())
	assert.NoError(t, mkv.CheckNoDuplicatePTS())
	assert.Empty(t, out.PLIs, "recorder keyframe requests")
	assert.LessOrEqual(t, mkv.Blocks, out.ReadFrames, "frames written but never read")
	assert.LessOrEqual(t, out.ReadFrames-mkv.Blocks, stopTailFrames, "frames read but not written")
}
