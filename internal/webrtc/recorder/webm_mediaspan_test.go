package recorder

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vp8Frame is picture pic at ts in pkts packets, the last one marked.
func vp8Frame(seq *uint16, pic uint16, ts uint32, key bool, pkts int) []*rtp.Packet {
	frame := make([]*rtp.Packet, pkts)
	for i := range frame {
		payload := append(corpus.VP8Payload(pic, i == 0, key, 640, 480), make([]byte, 64)...)
		frame[i] = &rtp.Packet{
			Header:  rtp.Header{SequenceNumber: *seq, Timestamp: ts, Marker: i == pkts-1},
			Payload: payload,
		}
		*seq++
	}

	return frame
}

func TestWebmRecorderMediaSpanFollowsAudioArrivals(t *testing.T) {
	rec := NewWebmRecorder(filepath.Join(t.TempDir(), "audio.webm"), 0o600, 256, 32, true, false)
	rec.SetHasAudio(true)

	_, ok := rec.MediaSpan()
	assert.False(t, ok)

	t0 := time.Now()
	for k := range 110 {
		rec.PushAudioAt(&rtp.Packet{
			Header:  rtp.Header{SequenceNumber: uint16(100 + k), Timestamp: uint32(48000 + k*960)},
			Payload: []byte{0xFC, 0xFF, 0xFE},
		}, t0.Add(time.Duration(k)*20*time.Millisecond))
	}

	span, ok := rec.MediaSpan()
	require.True(t, ok)
	assert.Equal(t, t0, span.Start)
	assert.True(t, span.Settled)

	rec.Close()

	span, _ = rec.MediaSpan()
	assert.Equal(t, t0.Add(2200*time.Millisecond), span.End)
}

// A camera's file starts at its first key frame, which can arrive long
// after the camera's first packets.
func TestWebmRecorderMediaSpanStartsAtTheFirstKeyFrame(t *testing.T) {
	for _, custom := range []bool{true, false} {
		rec := NewWebmRecorder(filepath.Join(t.TempDir(), "video.webm"), 0o600, 256, 32, custom, false)
		rec.SetHasVideo(true)

		t0 := time.Now()
		var seq, pic uint16 = 1000, 1
		push := func(ts uint32, key bool, at time.Duration) {
			for _, p := range vp8Frame(&seq, pic, ts, key, 3) {
				rec.PushVideoAt(p, t0.Add(at))
			}
			pic++
		}

		// 30 fps: 3000 ticks of 90 kHz apart.
		for k := range 18 {
			push(uint32(k*3000), false, time.Duration(k)*time.Second/30)
		}

		for k := 18; k < 40; k++ {
			push(uint32(k*3000), k == 18, time.Duration(k)*time.Second/30)
		}

		rec.Close()

		span, ok := rec.MediaSpan()
		require.True(t, ok, "custom sampler %v", custom)
		assert.Equal(t, t0.Add(18*time.Second/30), span.Start, "custom sampler %v", custom)
	}
}

// A 5 fps camera whose first frames are late: under either sampler, the
// frames that caught up set the start.
func TestWebmRecorderMediaSpanTakesOffALowFrameRateCameraBacklog(t *testing.T) {
	late := []time.Duration{321, 222, 61}
	for _, custom := range []bool{true, false} {
		rec := NewWebmRecorder(filepath.Join(t.TempDir(), "video.webm"), 0o600, 256, 32, custom, false)
		rec.SetHasVideo(true)

		t0 := time.Now()
		var seq uint16 = 1000
		for k := range 20 {
			at := time.Duration(k)*200*time.Millisecond + 2*time.Millisecond
			if k < len(late) {
				at = time.Duration(k)*200*time.Millisecond + late[k]*time.Millisecond
			}

			for _, p := range vp8Frame(&seq, uint16(k+1), uint32(k*18000), k == 0, 3) {
				rec.PushVideoAt(p, t0.Add(at))
			}
		}

		rec.Close()

		span, ok := rec.MediaSpan()
		require.True(t, ok, "custom sampler %v", custom)
		assert.Equal(t, t0.Add(2*time.Millisecond), span.Start, "custom sampler %v", custom)
		assert.True(t, span.Settled, "custom sampler %v", custom)
	}
}

// An Opus timestamp that steps back 100 ms is skipped, and the frames after
// it repeat timestamps already written. Their arrivals are their own.
func TestWebmRecorderMediaSpanIgnoresAnOpusStepBack(t *testing.T) {
	rec := NewWebmRecorder(filepath.Join(t.TempDir(), "audio.webm"), 0o600, 256, 32, true, false)
	rec.SetHasAudio(true)

	t0 := time.Now()
	for k := range 30 {
		ts := 48000 + k*960
		if k > 10 {
			ts -= 5 * 960
		}

		rec.PushAudioAt(&rtp.Packet{
			Header:  rtp.Header{SequenceNumber: uint16(100 + k), Timestamp: uint32(ts)},
			Payload: []byte{0xFC, 0xFF, 0xFE},
		}, t0.Add(time.Duration(k)*20*time.Millisecond))
	}

	rec.Close()

	span, ok := rec.MediaSpan()
	require.True(t, ok)
	assert.Equal(t, t0, span.Start)
}
