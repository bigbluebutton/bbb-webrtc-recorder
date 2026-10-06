package corpus_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The generated payloads must pass the writer's own VP8 and Opus checks.
func TestGeneratedTracesAreRecorded(t *testing.T) {
	video, err := corpus.GenerateVP8(corpus.VP8Options{FPS: 30, Frames: 30, KeyInterval: 10, SSRC: 1})
	require.NoError(t, err)
	audio, err := corpus.GenerateOpus(corpus.OpusOptions{Frames: 50, SSRC: 2})
	require.NoError(t, err)

	rec := recorder.NewWebmRecorder(filepath.Join(t.TempDir(), "out.webm"), 0o600, 256, 32, true, false)
	//nolint:staticcheck // SA1029: the key the recorder reads
	rec.WithContext(context.WithValue(context.Background(), "session", "corpus-test"))
	rec.SetHasVideo(true)
	rec.SetHasAudio(true)

	keys := 0
	for _, r := range video.Records {
		p := r.Packet()
		p.SSRC = video.Header.SSRC

		if recorder.IsVP8KeyFrame(p) {
			keys++
		}

		rec.PushVideo(p)
	}

	for _, r := range audio.Records {
		p := r.Packet()
		p.SSRC = audio.Header.SSRC
		rec.PushAudio(p)
	}

	stats := rec.GetStats()
	rec.Close()

	assert.Equal(t, 3, keys)
	require.NotNil(t, stats.Video)
	assert.Equal(t, 30, stats.Video.WrittenSamples)
	assert.Zero(t, stats.Video.CorruptedFrames)
	require.NotNil(t, stats.Audio)
	assert.Positive(t, stats.Audio.WrittenSamples)
}
