package recorder

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/at-wat/ebml-go/mkvcore"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebmRecorderSkipsAudioFrameThatDoesNotAdvance(t *testing.T) {
	file := filepath.Join(t.TempDir(), "audio.webm")
	rec := NewWebmRecorder(file, 0o600, 256, 32, true, false)
	rec.SetHasAudio(true)

	// Three 20 ms frames, then the blank pair LiveKit sends at downtrack
	// close: the first one tick after the last frame, the next 41 ms later.
	for i, ts := range []uint32{0, 960, 1920, 1921, 1921 + 1968} {
		rec.PushAudio(&rtp.Packet{
			Header:  rtp.Header{SequenceNumber: uint16(100 + i), Timestamp: 48000 + ts},
			Payload: []byte{0xFC, 0xFF, 0xFE},
		})
	}

	rec.Close()

	f, err := os.Open(file)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	readers, err := mkvcore.NewSimpleBlockReader(f)
	require.NoError(t, err)
	require.Len(t, readers, 1)

	var pts []int64
	for {
		_, _, ts, err := readers[0].Read()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		pts = append(pts, ts)
	}

	assert.Equal(t, []int64{0, 20, 40, 81}, pts)
	assert.Equal(t, 4, rec.GetStats().Audio.WrittenSamples)
}
