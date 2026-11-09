//go:build e2e && linux

package e2e

import (
	"context"
	"encoding/binary"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBrowserSimulcastMediaTimestamps records Chromium's camera and
// microphone, published as BBB's client publishes them. The room has BBB's
// options, and the camera is VP8 simulcast with the default profile's
// layers.
//
// Midway, the page stops sending h for a while (SetCameraLayer). LiveKit
// then forwards q, until it takes h back. The camera's file must hold both
// switches. Across them, each block must play as late as the first, and
// the file must keep lip sync with the microphone's, as in
// TestBrowserMediaTimestampsKeepLipSync.
//
// bbb-webrtc-sfu starts recording a camera when it is published, or later,
// when recording turns on. If nobody subscribes to a camera, dynacast
// pauses all its layers. In the second case the recorder's subscription
// resumes them, and the recorder then gets the same media as in the first
// case. What the second case adds is a check that LiveKit and Chromium
// resume a paused camera.
func TestBrowserSimulcastMediaTimestamps(t *testing.T) {
	RequireBrowser(t)

	t.Run("recorded at publish", func(t *testing.T) { testBrowserSimulcast(t, false) })
	t.Run("recorded after dynacast paused it", func(t *testing.T) { testBrowserSimulcast(t, true) })
}

// testBrowserSimulcast starts recording once the camera is published, or,
// with paused, once dynacast has paused its layers.
func testBrowserSimulcast(t *testing.T, paused bool) {
	const (
		// Dynacast pauses an unwatched camera 10 s after it is published.
		pauseWait = 20 * time.Second
		// Before h stops, and for how long.
		beforeDrop = 3 * time.Second
		dropFor    = 3 * time.Second
		// As in TestSimulcastMediaTimestampsPlaceEachFrame.
		upSwitchWait = 20 * time.Second
		afterUp      = 2 * time.Second
	)

	s := NewStack(t, StackOptions{RecorderConfig: fullTracePayloads(t)})
	room := "e2e-browser-simulcast-" + uuid.NewString()[:8]
	b := s.NewBrowser()
	peer := b.JoinAsBBB(room, "e2e-browser")
	mic := peer.PublishMic(MicOptions{})
	cam := peer.PublishBBBCamera("camera", bbbDefaultCameraProfile)
	t.Logf("camera: Chromium captures %dx%d at %v fps", cam.Capture.Width, cam.Capture.Height, cam.Capture.FrameRate)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	assert.Equal(t, layerSizes(bbbCameraLayers), layerSizes(listedVP8Layers(ctx, t, s.LiveKit, room, cam.SID)),
		"camera: the layers LiveKit lists")

	// The waits poll here, not in require.Eventually's goroutine: a browser
	// call that fails must stop the test from the test's own goroutine.
	if paused {
		want := map[string]bool{"q": false, "h": false}
		deadline := time.Now().Add(pauseWait)
		for l := peer.CameraLayers(cam.SID); !maps.Equal(l, want); l = peer.CameraLayers(cam.SID) {
			require.True(t, time.Now().Before(deadline), "camera: dynacast did not pause the layers: %v", l)
			time.Sleep(250 * time.Millisecond)
		}
	}

	recCam, err := s.Driver.StartRecording(ctx, room, cam.SID)
	require.NoError(t, err)
	recMic, err := s.Driver.StartRecording(ctx, room, mic.SID)
	require.NoError(t, err)

	for _, rec := range []*Recording{recCam, recMic} {
		_, err = s.Driver.WaitRtpStatus(ctx, rec.From, rec.SessionID, "flowing")
		require.NoError(t, err)
	}

	time.Sleep(beforeDrop)
	peer.SetCameraLayer(cam.SID, "h", false)
	dropped := time.Now()
	time.Sleep(dropFor)
	plis, ok := peer.CameraPLIs(cam.SID)["h"]
	require.True(t, ok, "camera: no outbound-rtp stats for h")
	peer.SetCameraLayer(cam.SID, "h", true)
	restored := time.Now()

	deadline := restored.Add(upSwitchWait)
	for p := peer.CameraPLIs(cam.SID); p["h"] <= plis; p = peer.CameraPLIs(cam.SID) {
		require.True(t, time.Now().Before(deadline), "LiveKit did not take h back: PLIs %v, h had %d before", p, plis)
		time.Sleep(250 * time.Millisecond)
	}
	time.Sleep(afterUp)

	stops := map[string]*events.RecordingStopped{}
	for _, rec := range []*Recording{recCam, recMic} {
		stops[rec.SessionID], err = s.Driver.StopAndWait(ctx, rec.SessionID)
		require.NoError(t, err)
	}

	late := map[string]time.Duration{}
	var camFile capturePlacement
	for _, c := range []struct {
		name    string
		rec     *Recording
		sid     string
		maxStep time.Duration
	}{{"camera", recCam, cam.SID, maxVideoStep}, {"microphone", recMic, mic.SID, maxAudioStep}} {
		p := placeOnCaptureClock(ctx, t, s, c.name, c.rec, c.sid)
		late[c.name] = p.Late
		assert.LessOrEqual(t, p.Worst.Abs(), browserDriftBound, "%s: drift against the capture clock", c.name)
		assertMediaEnd(t, c.name, p.Started, stops[c.rec.SessionID], p.Out.WebM.PTS, c.maxStep)
		if c.name == "camera" {
			camFile = p
		}
	}

	lipSync := late["camera"] - late["microphone"]
	t.Logf("lip sync: the camera plays %v later than the microphone", lipSync)
	assert.LessOrEqual(t, lipSync.Abs(), browserLipSyncBound, "lip sync")

	// Key frames tell the layers apart by size. Chromium can scale q down
	// while it is the only layer sent, so any size but h's is q. Both
	// switches must show in the file, as in
	// TestSimulcastMediaTimestampsPlaceEachFrame.
	h := fmt.Sprintf("%dx%d", bbbCameraLayers[1].Width, bbbCameraLayers[1].Height)
	var down, up bool
	prev := ""
	for _, kf := range writtenKeyFrames(camFile) {
		t.Logf("camera: %s key frame placed %v after h stopped", kf.size, kf.at.Sub(dropped))
		down = down || (prev == h && kf.size != h && kf.at.After(dropped) && kf.at.Before(restored))
		up = up || (down && prev != h && kf.size == h && kf.at.After(restored))
		prev = kf.size
	}

	assert.True(t, down, "camera: no switch from h to q while h was stopped")
	assert.True(t, up, "camera: no switch from q back to h")

	// On a switch up, LiveKit can forward two encodings of one captured frame:
	// q's key frame, then h's. Their RTP timestamps are under a ms apart, and
	// the recorder's PTS is in whole ms, so both can get the same PTS.
	//
	// CheckNoDuplicatePTS would fail this file. Here, two blocks may share a
	// PTS only if they hold the same captured frame. The bound adds a ms for
	// the camera's sender reports, which can disagree by that much.
	byPTS := map[int64]pairedBlock{}
	repeats := 0
	for _, b := range camFile.Blocks {
		other, ok := byPTS[b.PTS]
		if !ok {
			byPTS[b.PTS] = b
			continue
		}

		repeats++
		apart := camFile.Clock.at(b.Frame.TS).Sub(camFile.Clock.at(other.Frame.TS))
		assert.Less(t, apart.Abs(), 2*time.Millisecond, "camera: the block at %d ms repeats another frame's PTS", b.PTS)
	}
	t.Logf("camera: %d blocks repeat an earlier block's PTS", repeats)
}

// layerSizes describes layers as quality, size and bitrate, the fields a
// publisher sets.
func layerSizes(layers []*livekit.VideoLayer) []string {
	out := make([]string, 0, len(layers))
	for _, l := range layers {
		out = append(out, fmt.Sprintf("%s %dx%d %d bps", l.Quality, l.Width, l.Height, l.Bitrate))
	}

	return out
}

type keyFrame struct {
	size string
	at   time.Time
}

// writtenKeyFrames returns each key frame in p's file: its size, from its
// VP8 frame header (RFC 6386 9.1), and where mediaStart places it.
func writtenKeyFrames(p capturePlacement) []keyFrame {
	var out []keyFrame
	for _, b := range p.Blocks {
		d := b.Frame.Data
		if len(d) < 10 || d[0]&1 != 0 {
			continue
		}

		out = append(out, keyFrame{
			size: fmt.Sprintf("%dx%d", binary.LittleEndian.Uint16(d[6:])&0x3FFF, binary.LittleEndian.Uint16(d[8:])&0x3FFF),
			at:   p.placed(b),
		})
	}

	return out
}
