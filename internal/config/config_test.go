package config

import (
	"testing"

	"github.com/crazy-max/gonfig/file"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTraceCaptureDefaults(t *testing.T) {
	cfg := (&Config{}).GetDefaults()

	assert.Equal(t, TraceCapture{Enabled: false, Payload: TraceCapturePayloadHeader, MaxBytesPerTrack: 16 << 20},
		cfg.LiveKit.TraceCapture)
}

func TestTraceCaptureFromFile(t *testing.T) {
	cfg := (&Config{}).GetDefaults()
	yml := "livekit:\n  traceCapture:\n    enabled: true\n    payload: full\n    maxBytesPerTrack: 1048576\n"

	require.NoError(t, file.DecodeContent(yml, ".yml", cfg))

	assert.Equal(t, TraceCapture{Enabled: true, Payload: TraceCapturePayloadFull, MaxBytesPerTrack: 1 << 20},
		cfg.LiveKit.TraceCapture)
	assert.Equal(t, "ws://localhost:7880", cfg.LiveKit.Host, "the other defaults stay")
}

func TestTraceCaptureSampleConfig(t *testing.T) {
	cfg := (&Config{}).GetDefaults()

	require.NoError(t, file.Decode("../../config/bbb-webrtc-recorder.yml", cfg))

	assert.Equal(t, (&Config{}).GetDefaults().LiveKit.TraceCapture, cfg.LiveKit.TraceCapture, "the sample documents the defaults")
}

// The variables the sample config names, through the loader the app runs.
func TestTraceCaptureFromLoader(t *testing.T) {
	t.Setenv("BBBRECORDER_LIVEKIT_TRACECAPTURE_ENABLED", "true")
	t.Setenv("BBBRECORDER_LIVEKIT_TRACECAPTURE_PAYLOAD", "full")
	t.Setenv("BBBRECORDER_LIVEKIT_TRACECAPTURE_MAXBYTESPERTRACK", "4096")
	cfg := (&Config{}).GetDefaults()

	cfg.Load(App{Name: "bbb-webrtc-recorder"}, "../../config/bbb-webrtc-recorder.yml")

	assert.Equal(t, TraceCapture{Enabled: true, Payload: TraceCapturePayloadFull, MaxBytesPerTrack: 4096},
		cfg.LiveKit.TraceCapture)
}

func TestAudioClockSync(t *testing.T) {
	cfg := (&Config{}).GetDefaults()
	require.NoError(t, file.Decode("../../config/bbb-webrtc-recorder.yml", cfg))
	assert.False(t, cfg.LiveKit.AudioClockSync, "off by default, in the sample too")

	t.Setenv("BBBRECORDER_LIVEKIT_AUDIOCLOCKSYNC", "true")
	cfg = (&Config{}).GetDefaults()
	cfg.Load(App{Name: "bbb-webrtc-recorder"}, "../../config/bbb-webrtc-recorder.yml")
	assert.True(t, cfg.LiveKit.AudioClockSync, "the variable the sample config names turns it on")
}
