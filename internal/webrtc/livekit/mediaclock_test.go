package livekit

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/pion/rtcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSenderReportsReachTheirTrackClock(t *testing.T) {
	h := newLoopHarness(newPushRecorder(), "mic", "cam")
	mic := trackInfo{id: "mic", kind: TrackKindAudio, mime: MimeTypeOpus, source: "MICROPHONE", ssrc: 111}
	cam := trackInfo{id: "cam", kind: TrackKindVideo, mime: MimeTypeVP8, source: "CAMERA", ssrc: 222}
	share := trackInfo{id: "share", kind: TrackKindAudio, mime: MimeTypeOpus, source: "SCREEN_SHARE_AUDIO", ssrc: 444}
	h.w.prepareTrack(mic, 48000)
	h.w.prepareTrack(cam, 90000)
	h.w.prepareTrack(share, 48000)

	require.Nil(t, h.w.mediaClock("cam"), "video keeps its timestamps")
	require.Nil(t, h.w.mediaClock("share"), "screen-share audio keeps its timestamps")
	clock := h.w.mediaClock("mic")
	require.NotNil(t, clock)
	clock.Timestamp(5000, 1, time.Now())

	h.w.onRTCP(&rtcp.ReceiverReport{SSRC: 111})
	h.w.onRTCP(&rtcp.SenderReport{SSRC: 222, NTPTime: 1 << 32, RTPTime: 5000})
	h.w.onRTCP(&rtcp.SenderReport{SSRC: 111, NTPTime: 1 << 32, RTPTime: 5000})
	assert.Equal(t, 1, clock.Stats().SenderReports, "only the microphone's own report")

	// Subscribed again under a new SSRC: the old one reaches nothing.
	mic.ssrc = 333
	h.w.prepareTrack(mic, 48000)
	again := h.w.mediaClock("mic")
	again.Timestamp(9000, 1, time.Now())
	h.w.onRTCP(&rtcp.SenderReport{SSRC: 111, NTPTime: 2 << 32, RTPTime: 9000})
	h.w.onRTCP(&rtcp.SenderReport{SSRC: 333, NTPTime: 2 << 32, RTPTime: 9000})
	assert.Equal(t, 1, again.Stats().SenderReports)
	assert.Equal(t, 1, clock.Stats().SenderReports)
}

// TestAudioClockSync replays a microphone whose media clock runs slow with
// livekit.audioClockSync on and off: off, the clock only measures.
func TestAudioClockSync(t *testing.T) {
	traces := loadSeed(t, "c18-slow-mic-clock")

	for _, on := range []bool{true, false} {
		cfg := (&config.Config{}).GetDefaults().LiveKit
		cfg.AudioClockSync = on

		var r replayResult
		synctest.Test(t, func(t *testing.T) {
			r = replayTracesWith(t, cfg, traces)
		})
		require.NotNil(t, r.audioClock)
		require.NoError(t, r.audioLagErr)
		st := *r.audioClock
		t.Logf("on=%v: %s %+v", on, r, st)

		// 60 s of media read over 61.28 s, which the reports show.
		assert.InDelta(t, 0.979, st.RateRatio, 0.001)
		assert.InDelta(t, -1280, st.ReportedDriftMs, 20)
		assert.Equal(t, 12, st.SenderReports)

		if on {
			// Within a 120 ms gap less the 40 ms trigger, either way.
			assert.InDelta(t, 1280, st.InsertedGapMs, 80)
			assert.InDelta(t, 0, st.LagMs, 80)
			assert.InDelta(t, 0, r.audioLag.FinalMs, 80)
		} else {
			assert.Zero(t, st.InsertedGaps)
			assert.InDelta(t, 1280, st.LagMs, 20, "the lag it would have corrected")
			assert.InDelta(t, -1284, r.audioLag.FinalMs, 1, "the last frame plays early")
		}
	}
}
