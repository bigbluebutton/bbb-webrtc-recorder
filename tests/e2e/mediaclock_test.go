//go:build e2e && linux

package e2e

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/appstats"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSlowMicClockKeepsWallClock publishes a microphone whose media clock
// runs slow and checks that the recorder writes each frame where it arrived,
// so that the audio keeps the timeline its video follows. After a mute,
// LiveKit moves the downtrack's timestamps to the wall clock itself, and the
// recorder must give back the gaps that the jump covers.
func TestSlowMicClockKeepsWallClock(t *testing.T) {
	for _, tc := range []struct {
		name    string
		muteAt  time.Duration // 0: no mute
		muteFor time.Duration
	}{
		{name: "steady"},
		{name: "mute and unmute", muteAt: 15 * time.Second, muteFor: 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) { testSlowMicClock(t, tc.muteAt, tc.muteFor) })
	}
}

// Copies of mediaClockArrivalWindow, mediaClockTrigger and mediaClockGap
// (internal/webrtc/utils), for the microphone scenarios' lag bounds.
const (
	clockWindow  = time.Second
	clockTrigger = 40 * time.Millisecond
	clockGap     = 120 * time.Millisecond
	// Read times carry the publisher's pacing and loopback jitter.
	micReadJitter = 60 * time.Millisecond
)

// endLagBound is how far a corrected microphone can end from where it was
// read. unseenMs is the loss the arrival window does not show yet. The
// microphone can end early by up to unseenMs plus the trigger, or late by
// up to a gap less the trigger.
func endLagBound(unseenMs float64) float64 {
	return max(unseenMs+ms(clockTrigger), ms(clockGap-clockTrigger)) + ms(micReadJitter)
}

func testSlowMicClock(t *testing.T, muteAt, muteFor time.Duration) {
	t.Helper()

	const (
		// The worst rate a production sender showed over 8 minutes.
		clockRate   = 0.955
		recordFor   = 30 * time.Second
		scheduleLen = 60 * time.Second
		// LiveKit sends a sender report every 3 s, and the reports need to
		// span 5 s to show the rate, so the lag can grow for 8 s before it
		// is corrected.
		onsetLag = 8 * time.Second
	)

	s := NewStack(t, StackOptions{RecorderConfig: func(_ *Stack, cfg map[string]any) {
		lookup[map[string]any](t, cfg, "livekit")["audioClockSync"] = true
	}})
	room := "e2e-media-clock-" + uuid.NewString()[:8]
	pub := s.NewPublisher(room, "e2e-publisher")

	audio, err := corpus.GenerateOpus(corpus.OpusOptions{Duration: scheduleLen})
	require.NoError(t, err)
	audio.Records, err = corpus.Apply(audio.Records, corpus.MediaClock(0, scheduleLen.Microseconds(), clockRate))
	require.NoError(t, err)

	mic := pub.PublishOpus(TrackOptions{Name: "microphone", Schedule: audio})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	require.NoError(t, pub.WaitPublished(ctx, mic))

	base, err := ScrapeMetrics(ctx, s.Recorder.MetricsURL)
	require.NoError(t, err)

	rec, err := s.Driver.StartRecording(ctx, room, mic.SID)
	require.NoError(t, err)
	_, err = s.Driver.WaitRtpStatus(ctx, rec.From, rec.SessionID, "flowing")
	require.NoError(t, err)
	if muteAt > 0 {
		time.Sleep(muteAt)
		mic.Pub.SetMuted(true)
		time.Sleep(muteFor)
		mic.Pub.SetMuted(false)
		time.Sleep(recordFor - muteAt - muteFor)
	} else {
		time.Sleep(recordFor)
	}
	_, err = s.Driver.StopAndWait(ctx, rec.SessionID)
	require.NoError(t, err)

	after, err := ScrapeMetrics(ctx, s.Recorder.MetricsURL)
	require.NoError(t, err)
	delta := after.Delta(base)

	out := ReadOutput(t, rec, mic.SID)
	require.Equal(t, out.ReadFrames, out.WebM.Blocks, "every frame the recorder read is written")

	lag, err := oracle.TimelineLag(out.WebM, out.Trace)
	require.NoError(t, err)

	deficit := func(d time.Duration) float64 {
		return float64(d.Milliseconds()) * (1 - clockRate)
	}
	jitter := ms(micReadJitter)
	maxLag := deficit(onsetLag) + jitter
	// The sender loses media at a constant rate, so the arrival window has
	// yet to show one second of deficit.
	finalLag := endLagBound(deficit(clockWindow))

	t.Logf("webm: blocks=%d pts=[%d,%d] maxGap=%dms; lag: max=%.1fms final=%.1fms (bounds %.0f, %.0f); uncorrected it would end %.0f ms early",
		out.WebM.Blocks, out.WebM.FirstPTS, out.WebM.LastPTS, out.WebM.MaxGapMs, lag.MaxMs, lag.FinalMs,
		maxLag, finalLag, deficit(recordFor))

	assert.NoError(t, out.WebM.CheckMonotonic())
	assert.NoError(t, out.WebM.CheckNoDuplicatePTS())
	assert.LessOrEqual(t, math.Abs(lag.MaxMs), maxLag, "largest lag behind the read times")
	assert.LessOrEqual(t, math.Abs(lag.FinalMs), finalLag, "lag at the end")

	stats := readStatsFile(t, rec.Path)
	track := stats.CaptureStats.Tracks["MICROPHONE"]
	require.NotNil(t, track, "microphone stats")
	require.NotNil(t, track.MediaClock, "microphone media clock stats")
	mc := track.MediaClock
	lost := float64(mc.ArrivalDurationMs) * (1 - clockRate)
	t.Logf("stats file mediaClock: %+v; lost %.0f ms", *mc, lost)

	assert.Positive(t, mc.SenderReports, "LiveKit forwards the publisher's sender reports")
	covered := mc.RTPDurationMs + mc.InsertedGapMs - mc.GivenBackMs
	assert.InDelta(t, mc.ArrivalDurationMs, covered, finalLag, "media time plus gaps against arrival time")
	// The reports leave LiveKit's realignment out. The newest can be 4 s
	// old: pion's 1 s interval and LiveKit's 3 s.
	assert.InDelta(t, -lost, float64(mc.ReportedDriftMs), deficit(4*time.Second)+jitter, "drift by the reports")
	if muteAt > 0 {
		assert.Positive(t, mc.Realignments, "LiveKit moved the timestamps to the wall clock")
		assert.Positive(t, mc.GivenBackMs, "the gaps the jump covers given back")
	} else {
		// A mute fills its time with LiveKit's blank frames, at the wall
		// clock's rate, so only a steady track shows the sender's rate.
		assert.InDelta(t, clockRate, mc.RateRatio, 0.003, "media time over arrival time")
		assert.InDelta(t, lost, float64(mc.InsertedGapMs), finalLag, "gaps inserted against the media time lost")
	}

	labels := map[string]string{"source": "MICROPHONE", "kind": string(corpus.KindAudio)}
	assert.InDelta(t, float64(mc.InsertedGaps), delta.Sum("recorder_media_clock_inserted_gaps_total", labels), 0)
	assert.InDelta(t, float64(mc.InsertedGapMs)/1000, delta.Sum("recorder_media_clock_inserted_gap_seconds_total", labels), 0.001)
	assert.InDelta(t, 1, delta.Sum("recorder_media_clock_rate_ratio_count", labels), 0, "one track's rate ratio observed")
}

func readStatsFile(t *testing.T, webmPath string) *appstats.StatsFileOutput {
	t.Helper()

	raw, err := os.ReadFile(strings.TrimSuffix(webmPath, ".webm") + "-stats.json")
	require.NoError(t, err)

	out := &appstats.StatsFileOutput{}
	require.NoError(t, json.Unmarshal(raw, out))

	return out
}
