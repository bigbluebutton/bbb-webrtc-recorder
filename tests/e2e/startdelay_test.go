//go:build e2e && linux

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestMediaTimestampsTakeOffAStartDelay records a microphone and a camera
// while netem delays everything the test sends the servers by 300 ms. The
// delay then comes off 10 ms at a time. It lasts past the microphone's first
// second of media and is gone within each track's first 2 s. Both starts
// must drop it: every block lands where its frame was sent, and the two
// files land together.
//
// It uses the NACK scenario's namespaces, with no loss, so it needs root and
// E2E_NETEM=1 too.
func TestMediaTimestampsTakeOffAStartDelay(t *testing.T) {
	requireNetem(t)

	const (
		cameraFPS = 15
		delay     = 300 * time.Millisecond
		// From the microphone's flowing event, which comes at least delay
		// after its first frame was sent.
		hold      = 800 * time.Millisecond
		drain     = 400 * time.Millisecond
		recordFor = 6 * time.Second
	)

	plan := newNetemPlan(os.Getpid(), 0)
	setupNetem(t, plan)

	// Everything the host sends the servers: the publisher's media and the
	// test's signalling and Redis commands.
	netem := func(verb string, d time.Duration) {
		out, err := exec.Command("tc", "qdisc", verb, "dev", plan.HostSrvIf, "root", "netem",
			"delay", fmt.Sprintf("%dms", d.Milliseconds())).CombinedOutput()
		require.NoError(t, err, "tc qdisc %s: %s", verb, strings.TrimSpace(string(out)))
	}
	netem("add", delay)

	s := newNetemStack(t, plan, StackOptions{})

	room := "e2e-startdelay-" + uuid.NewString()[:8]
	pub := s.NewPublisher(room, "e2e-publisher")

	video, err := corpus.GenerateVP8(corpus.VP8Options{FPS: cameraFPS, Duration: 30 * time.Second})
	require.NoError(t, err)
	audio, err := corpus.GenerateOpus(corpus.OpusOptions{Duration: 30 * time.Second})
	require.NoError(t, err)

	cam := pub.PublishVP8(TrackOptions{Name: "camera", Schedule: video, KeyframeDelay: 100 * time.Millisecond, TagFrames: true})
	mic := pub.PublishOpus(TrackOptions{Name: "microphone", Schedule: audio, TagFrames: true})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	require.NoError(t, pub.WaitPublished(ctx, cam, mic))

	// Both start at once, as when a moderator starts recording. Each start
	// joins the room on its own, and its Redis command meets the delay.
	// Started one after the other, the camera would start after the delay is
	// off.
	var recCam *Recording
	var camErr error
	camStarted := make(chan struct{})
	go func() {
		defer close(camStarted)
		recCam, camErr = s.Driver.StartRecording(ctx, room, cam.SID)
	}()

	recMic, err := s.Driver.StartRecording(ctx, room, mic.SID)
	require.NoError(t, err)
	_, err = s.Driver.WaitRtpStatus(ctx, recMic.From, recMic.SessionID, "flowing")
	require.NoError(t, err)
	micFlowing := firstFlowing(s.Driver, recMic.From, recMic.SessionID)
	require.False(t, micFlowing.IsZero())

	<-camStarted
	require.NoError(t, camErr)

	drainFrom := micFlowing.Add(hold)
	require.True(t, time.Now().Before(drainFrom), "the camera's recording started after the delay was to come off")

	// One 10 ms step every 13 ms keeps whole frames in order. A packet sent
	// just after a step can still overtake one sent just before it; the
	// jitter buffer puts them back in order.
	const step = 10 * time.Millisecond
	steps := int(delay / step)
	for i := 1; i <= steps; i++ {
		time.Sleep(time.Until(drainFrom.Add(drain * time.Duration(i) / time.Duration(steps))))
		netem("change", delay-step*time.Duration(i))
	}
	drainEnd := time.Now()

	_, err = s.Driver.WaitRtpStatus(ctx, recCam.From, recCam.SessionID, "flowing")
	require.NoError(t, err)

	time.Sleep(recordFor)

	stops := map[string]*events.RecordingStopped{}
	for _, rec := range []*Recording{recMic, recCam} {
		stops[rec.SessionID], err = s.Driver.StopAndWait(ctx, rec.SessionID)
		require.NoError(t, err)
	}

	places := map[string]placement{}
	for _, c := range []struct {
		name    string
		rec     *Recording
		track   *PubTrack
		maxStep time.Duration
	}{{"camera", recCam, cam, maxVideoStep}, {"microphone", recMic, mic, maxAudioStep}} {
		sends := frameSends(c.track)
		p, _ := checkPlacement(ctx, t, s.Driver, c.name, c.rec, sends, stops[c.rec.SessionID], c.maxStep)
		places[c.name] = p

		// The scenario's premises. Without them the test could pass with no
		// delay to take off.
		firstSent := sends[p.Blocks[0].Frame]
		flowingAt := firstFlowing(s.Driver, c.rec.From, c.rec.SessionID)
		t.Logf("%s: the flowing event came %v after the first frame written was sent, and the delay began to come off %v after it",
			c.name, flowingAt.Sub(firstSent), drainFrom.Sub(firstSent))
		require.True(t, firstSent.Before(drainFrom), "%s: the file started after the delay", c.name)
		require.Less(t, drainEnd.Sub(firstSent), recorder.MediaSpanWindow-200*time.Millisecond,
			"%s: the delay was still on near the end of the recorder's window", c.name)
		if c.name == "microphone" {
			require.GreaterOrEqual(t, flowingAt.Sub(firstSent), delay, "the microphone's first frame was not delayed")
			require.Greater(t, drainFrom.Sub(firstSent), time.Second,
				"the delay did not last past the microphone's first second of media")
		}
	}

	assertLipSync(t, "lip sync", places["camera"], places["microphone"])
}
