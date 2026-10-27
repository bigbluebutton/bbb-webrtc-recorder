//go:build e2e && linux

package e2e

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBrowserMicCaptureFreeze publishes Chromium's microphone, healthy or
// under a CaptureFreeze. With livekit.audioClockSync off, the default, the
// recording ends early by the time lost, and the stats, endWarning and
// Chromium's sender reports show it. With it on, each frame plays near where
// it arrived, and a healthy microphone gets no gap.
func TestBrowserMicCaptureFreeze(t *testing.T) {
	RequireBrowser(t)

	for _, tc := range []struct {
		name      string
		freeze    bool
		clockSync bool
	}{
		{name: "healthy, clock sync on", clockSync: true},
		{name: "capture freeze, clock sync off", freeze: true},
		{name: "capture freeze, clock sync on", freeze: true, clockSync: true},
	} {
		t.Run(tc.name, func(t *testing.T) { testBrowserMic(t, tc.freeze, tc.clockSync) })
	}
}

func testBrowserMic(t *testing.T, freeze, clockSync bool) {
	t.Helper()

	const (
		recordFor = 45 * time.Second
		// Chromium sends a sender report every 2.5 to 7.5 s, and LiveKit
		// forwards the latest every 3 s. The reports can take two of
		// Chromium's intervals and a forwarding interval to show the rate,
		// and the lag grows until they do.
		onsetLag = 18 * time.Second
		// The newest report can be one of Chromium's intervals and a
		// forwarding interval old.
		reportAge = 10*time.Second + 500*time.Millisecond
	)

	// 4.5 % of the time, the worst rate a production sender showed.
	freezeFor := CaptureFreeze{Burst: 90 * time.Millisecond, Every: 2 * time.Second}

	s := NewStack(t, StackOptions{RecorderConfig: func(_ *Stack, cfg map[string]any) {
		lookup[map[string]any](t, cfg, "livekit")["audioClockSync"] = clockSync
	}})
	b := s.NewBrowser()
	room := "e2e-browser-mic-" + uuid.NewString()[:8]
	mic := b.Join(room, "e2e-browser").PublishMic(MicOptions{})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	if freeze {
		b.FreezeCapture(freezeFor)
	}

	rec, err := s.Driver.StartRecording(ctx, room, mic.SID)
	require.NoError(t, err)
	_, err = s.Driver.WaitRtpStatus(ctx, rec.From, rec.SessionID, "flowing")
	require.NoError(t, err)
	time.Sleep(recordFor)
	_, err = s.Driver.StopAndWait(ctx, rec.SessionID)
	require.NoError(t, err)

	// A burst is the most media lost at once.
	var burst float64
	if freeze {
		fs := b.ThawCapture()
		burst = max(ms(freezeFor.Burst), fs.MaxBurstMs)
		t.Logf("capture freeze: %d stops, %.0f ms, at most %.0f ms at once", fs.Stops, fs.FrozenMs, fs.MaxBurstMs)
	}

	out := ReadOutput(t, rec, mic.SID)
	require.Equal(t, out.ReadFrames, out.WebM.Blocks, "every frame the recorder read is written")
	assert.NoError(t, out.WebM.CheckMonotonic())
	assert.NoError(t, out.WebM.CheckNoDuplicatePTS())

	lag, err := oracle.TimelineLag(out.WebM, out.Trace)
	require.NoError(t, err)

	mc := readStatsFile(t, rec.Path).CaptureStats.Tracks["MICROPHONE"].MediaClock
	require.NotNil(t, mc, "microphone media clock stats")

	// lost is the media time Chromium lost. RateRatio counts the sender's
	// timestamps without the gaps inserted, so it measures the loss with
	// clock sync on or off. ArrivalDurationMs starts at the first read, which
	// a backlog at the start delays by lag.StartMs.
	arrival := float64(mc.ArrivalDurationMs) + lag.StartMs
	lost := float64(mc.ArrivalDurationMs)*(1-mc.RateRatio) + lag.StartMs
	lossRate := max(lost, 0) / arrival
	deficit := func(d time.Duration) float64 { return ms(d) * lossRate }
	jitter := ms(micReadJitter)
	// A burst lost in the last second, or the deficit over that second, does
	// not show in the arrival window yet.
	finalLag := endLagBound(max(burst, deficit(clockWindow)))
	// Until the reports show the rate, the lag is the loss so far: the mean
	// rate's deficit over onsetLag, plus up to one burst.
	maxLag := max(deficit(onsetLag)+burst+jitter, finalLag)

	t.Logf("webm: blocks=%d pts=[%d,%d] maxGap=%dms; lag: max=%.1fms final=%.1fms start=%.1fms (bounds %.0f, %.0f); lost %.0f ms",
		out.WebM.Blocks, out.WebM.FirstPTS, out.WebM.LastPTS, out.WebM.MaxGapMs, lag.MaxMs, lag.FinalMs, lag.StartMs,
		maxLag, finalLag, lost)
	t.Logf("stats file mediaClock: %+v", *mc)

	if freeze {
		assert.Less(t, mc.RateRatio, 1-freezeFor.Burst.Seconds()/freezeFor.Every.Seconds()/2,
			"the freeze slowed Chromium's media clock")
	}

	if !freeze {
		require.Less(t, math.Abs(lost), ms(clockTrigger), "Chromium's capture was healthy")
	}

	assert.Positive(t, mc.SenderReports, "LiveKit forwards Chromium's sender reports")
	assert.InDelta(t, -lost, float64(mc.ReportedDriftMs), deficit(reportAge)+burst+jitter, "drift by Chromium's sender reports")

	if !clockSync {
		// Chromium's timestamps stay contiguous: one 20 ms frame apart.
		assert.LessOrEqual(t, out.WebM.MaxGapMs, int64(20), "no step between frames")
		assert.Zero(t, mc.InsertedGaps)
		assert.InDelta(t, -lost, lag.FinalMs, jitter, "the recording ends early by the time lost")
		assert.Contains(t, readFile(t, s.Recorder.Proc.LogPath), endWarning)

		return
	}

	assert.LessOrEqual(t, math.Abs(lag.MaxMs), maxLag, "largest lag behind the read times")
	assert.LessOrEqual(t, math.Abs(lag.FinalMs), finalLag, "lag at the end")
	covered := mc.RTPDurationMs + mc.InsertedGapMs - mc.GivenBackMs
	assert.InDelta(t, arrival, covered, finalLag, "media time plus gaps against arrival time")
	if !freeze {
		assert.Zero(t, mc.InsertedGaps, "no gap for a healthy microphone")
	}
	assert.Zero(t, mc.Realignments, "a capture freeze is not a LiveKit realignment")
	assert.Zero(t, mc.GivenBackMs)
	assert.NotContains(t, readFile(t, s.Recorder.Proc.LogPath), endWarning, "no warning once corrected")
}

// endWarning starts the warning the recorder logs when a microphone ends
// mediaClockLagWarning (internal/webrtc/livekit) or more behind the wall
// clock.
const endWarning = "Microphone ended behind the wall clock"

func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(data)
}
