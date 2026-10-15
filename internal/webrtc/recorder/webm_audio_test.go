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

// Frames held in the samplebuilder after a packet loss must be written when
// the recorder closes, not dropped.
func TestWebmRecorderWritesAudioHeldBehindALossOnClose(t *testing.T) {
	file := filepath.Join(t.TempDir(), "audio.webm")
	rec := NewWebmRecorder(file, 0o600, 256, 32, true, false)
	rec.SetHasAudio(true)

	// Five frames, seq 105 lost, then 25 frames: fewer than the queue size.
	var want []int64
	for i := range 31 {
		if i == 5 {
			continue
		}
		rec.PushAudio(&rtp.Packet{
			Header:  rtp.Header{SequenceNumber: uint16(100 + i), Timestamp: 48000 + uint32(i)*960},
			Payload: []byte{0xFC, 0xFF, 0xFE},
		})
		want = append(want, int64(i)*20)
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

	assert.Equal(t, want, pts)
	assert.Equal(t, len(want), rec.GetStats().Audio.WrittenSamples)
}

// LiveKit's blank frames at track close can arrive after a packet loss, so
// the close drain must also skip the one that doesn't advance the timestamp.
func TestWebmRecorderDrainSkipsAudioFrameThatDoesNotAdvance(t *testing.T) {
	file := filepath.Join(t.TempDir(), "audio.webm")
	rec := NewWebmRecorder(file, 0o600, 256, 32, true, false)
	rec.SetHasAudio(true)

	// Three 20 ms frames, seq 103 lost, one frame, then the blank pair.
	for _, p := range []struct {
		seq uint16
		ts  uint32
	}{{100, 0}, {101, 960}, {102, 1920}, {104, 3840}, {105, 3841}, {106, 3841 + 1968}} {
		rec.PushAudio(&rtp.Packet{
			Header:  rtp.Header{SequenceNumber: p.seq, Timestamp: 48000 + p.ts},
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

	assert.Equal(t, []int64{0, 20, 40, 80, 121}, pts)
	assert.Equal(t, 5, rec.GetStats().Audio.WrittenSamples)
}
