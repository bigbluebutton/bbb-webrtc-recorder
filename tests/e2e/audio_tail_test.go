//go:build e2e && linux

package e2e

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAudioAfterTrailingLossIsWritten drops one microphone packet shortly
// before the recording stops. The recorder and the LiveKit jitter buffer both
// hold the frames that follow a loss, so stopping must still write them.
func TestAudioAfterTrailingLossIsWritten(t *testing.T) {
	for _, tc := range []struct {
		name string
		// Below the recorder's audioPacketQueueSize (32), so the frames are
		// still held when the recording stops.
		framesAfterLoss int
		// If set, the source pauses this long before the stop, longer than the
		// 200 ms audio jitter latency. Otherwise the stop arrives while the
		// source is still sending.
		quietFor time.Duration
	}{
		{name: "source quiet", framesAfterLoss: 10, quietFor: 300 * time.Millisecond},
		{name: "still talking", framesAfterLoss: 3},
	} {
		t.Run(tc.name, func(t *testing.T) { testAudioAfterTrailingLoss(t, tc.framesAfterLoss, tc.quietFor) })
	}
}

func testAudioAfterTrailingLoss(t *testing.T, framesAfterLoss int, quietFor time.Duration) {
	t.Helper()

	s := NewStack(t, StackOptions{})
	room := "e2e-audio-tail-" + uuid.NewString()[:8]
	pub := s.NewPublisher(room, "e2e-publisher")

	audio, err := corpus.GenerateOpus(corpus.OpusOptions{Duration: 30 * time.Second})
	require.NoError(t, err)

	var lossFrame atomic.Int64
	lossFrame.Store(-1)
	mic := pub.PublishOpus(TrackOptions{Name: "microphone", Schedule: audio, Drop: func(p SentPacket) bool {
		return int64(p.Frame) == lossFrame.Load()
	}})

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	require.NoError(t, pub.WaitPublished(ctx, mic))

	base, err := ScrapeMetrics(ctx, s.Recorder.MetricsURL)
	require.NoError(t, err)

	rec, err := s.Driver.StartRecording(ctx, room, mic.SID)
	require.NoError(t, err)
	_, err = s.Driver.WaitRtpStatus(ctx, rec.From, rec.SessionID, "flowing")
	require.NoError(t, err)
	time.Sleep(time.Second)

	lastFrame := func() int {
		sent := mic.Sent()

		return sent[len(sent)-1].Frame
	}
	loss := lastFrame() + 5
	lossFrame.Store(int64(loss))
	require.Eventually(t, func() bool { return lastFrame() >= loss+framesAfterLoss }, 5*time.Second, time.Millisecond)
	if quietFor > 0 {
		mic.Pause()
		time.Sleep(quietFor)
	}

	_, err = s.Driver.StopAndWait(ctx, rec.SessionID)
	require.NoError(t, err)
	after, err := ScrapeMetrics(ctx, s.Recorder.MetricsURL)
	require.NoError(t, err)
	labels := map[string]string{"source": "MICROPHONE", "kind": string(corpus.KindAudio)}
	written := int(after.Delta(base).Sum(metricWrittenSamples, labels))

	var lostSeq uint16
	afterLoss := 0
	for _, p := range mic.Sent() {
		switch {
		case p.Dropped:
			lostSeq = p.Seq
		case p.Frame > loss:
			afterLoss++
		}
	}

	out := ReadOutput(t, rec, mic.SID)
	readAfterLoss := 0
	for _, r := range out.Trace.Records {
		require.False(t, r.T == corpus.RecordRTP && r.Seq == lostSeq, "the lost packet was read")
		if r.T == corpus.RecordRTP && !r.Pad && int16(r.Seq-lostSeq) > 0 {
			readAfterLoss++
		}
	}

	t.Logf("lost seq %d, then %d frames sent and %d read; read %d frames, wrote %d (metric %d)",
		lostSeq, afterLoss, readAfterLoss, out.ReadFrames, out.WebM.Blocks, written)

	// With no frames after the loss there is nothing to check; with 32 or
	// more, the recorder would have written them before the stop anyway.
	require.Positive(t, readAfterLoss, "no frame read after the loss")
	require.Less(t, readAfterLoss, 32, "the recorder moved past the loss before the stop")
	assert.Equal(t, out.ReadFrames, out.WebM.Blocks, "every frame the recorder read is written")
	assert.Equal(t, out.WebM.Blocks, written, "%s %v", metricWrittenSamples, labels)
}
