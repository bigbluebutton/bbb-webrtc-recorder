//go:build e2e && linux

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBrowserH264CameraDecodesAcrossALayerSwitch publishes Chromium's
// camera in H.264 with BBB's default profile, so LiveKit forwards one of
// its two layers. Midway, the page stops sending h for a while, as in
// TestBrowserSimulcastMediaTimestamps. LiveKit then forwards q, until it
// takes h back.
//
// The file must hold h, then q, then h, and pass the oracle's H.264 checks.
// Two blocks may share a PTS only if they hold one captured frame. ffmpeg
// must decode the file across both switches, as in TestH264ClipDecodes.
func TestBrowserH264CameraDecodesAcrossALayerSwitch(t *testing.T) {
	RequireBrowser(t)

	const (
		dropFor = 3 * time.Second
		// As in TestBrowserSimulcastMediaTimestamps, for each time LiveKit
		// takes h.
		upSwitchWait = 20 * time.Second
		// The media recorded on h after each switch up.
		onH = 2 * time.Second
	)

	s := NewStack(t, StackOptions{RecorderConfig: fullTracePayloads(t)})
	room := "e2e-browser-h264-switch-" + uuid.NewString()[:8]
	peer := s.NewBrowser().JoinAsBBB(room, "e2e-browser")
	cam := peer.PublishBBBCameraCodec("camera", bbbDefaultCameraProfile, CameraCodec{Video: "h264"})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	assert.Equal(t, layerSizes(bbbCameraLayers), layerSizes(listedLayers(ctx, t, s.LiveKit, room, cam.SID, webrtc.MimeTypeH264)),
		"camera: the H.264 layers LiveKit lists")

	// LiveKit asks h for a key frame each time it takes h.
	hPLIs := func() int {
		st, ok := peer.SenderStats(cam.SID)["h"]
		require.True(t, ok, "camera: no outbound-rtp stats for h")
		return st.PLICount
	}
	// The waits poll on the test's goroutine, as testBrowserSimulcast's do.
	waitForH := func(after int, what string) {
		deadline := time.Now().Add(upSwitchWait)
		for hPLIs() <= after {
			require.True(t, time.Now().Before(deadline), "LiveKit did not take h %s", what)
			time.Sleep(250 * time.Millisecond)
		}
		time.Sleep(onH)
	}

	before := hPLIs()
	rec := record(ctx, t, s, room, cam.SID)
	waitForH(before, "at the start")

	peer.SetCameraLayer(cam.SID, "h", false)
	dropped := time.Now()
	time.Sleep(dropFor)
	before = hPLIs()
	peer.SetCameraLayer(cam.SID, "h", true)
	restored := time.Now()
	waitForH(before, "back")

	_, err := s.Driver.StopAndWait(ctx, rec.SessionID)
	require.NoError(t, err)

	for rid, st := range peer.SenderStats(cam.SID) {
		t.Logf("sender %q: %s by %s, %d packets, %d PLIs", rid, st.Codec, st.EncoderImplementation, st.PacketsSent, st.PLICount)
	}

	p := placeOnCaptureClock(ctx, t, s, "camera", rec, cam.SID)
	mkv := p.Out.WebM
	t.Logf("mkv: blocks=%d h264Keyframes=%d size=%dx%d pts=[%d,%d] maxGap=%dms",
		mkv.Blocks, mkv.H264Keyframes, mkv.Width, mkv.Height, mkv.FirstPTS, mkv.LastPTS, mkv.MaxGapMs)
	require.Equal(t, oracle.CodecH264, mkv.Codec)
	assert.NoError(t, mkv.CheckH264())
	assert.NoError(t, mkv.CheckMonotonic())
	t.Logf("camera: %d blocks repeat an earlier block's PTS", assertRepeatsHoldOneFrame(t, "camera", p))

	require.GreaterOrEqual(t, len(mkv.PTS), 2, "blocks")
	last := time.Duration(mkv.PTS[len(mkv.PTS)-1]-mkv.PTS[len(mkv.PTS)-2]) * time.Millisecond
	span := time.Duration(mkv.LastPTS-mkv.FirstPTS)*time.Millisecond + last
	t.Run("ffmpeg", func(t *testing.T) {
		checkDecodes(t, rec.Path, mkv.Blocks, span, time.Duration(mkv.MaxGapMs)*time.Millisecond)

		// The size of each picture, one per block: baseline H.264 decodes
		// in the order it is sent.
		out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0",
			"-show_entries", "frame=width,height", "-of", "csv=p=0:s=x", rec.Path).Output()
		require.NoError(t, err, "ffprobe frame sizes")
		sizes := strings.Fields(string(out))
		require.Len(t, sizes, len(p.Blocks), "ffprobe: pictures")

		for i := 1; i < len(p.Blocks); i++ {
			if p.Blocks[i].PTS == p.Blocks[i-1].PTS {
				t.Logf("camera: the blocks at %d ms are %s, then %s", p.Blocks[i].PTS, sizes[i-1], sizes[i])
			}
		}

		// Any size but h's is q, as in testBrowserSimulcast.
		h := fmt.Sprintf("%dx%d", bbbCameraLayers[1].Width, bbbCameraLayers[1].Height)
		var down, up bool
		prev := ""
		for i, size := range sizes {
			if size == prev {
				continue
			}

			at := p.placed(p.Blocks[i])
			t.Logf("camera: %s from the block placed %v after h stopped", size, at.Sub(dropped))
			down = down || (prev == h && at.After(dropped) && at.Before(restored))
			up = up || (down && size == h && at.After(restored))
			prev = size
		}

		assert.True(t, down, "camera: no switch from h to q while h was stopped")
		assert.True(t, up, "camera: no switch from q back to h")
	})
}
