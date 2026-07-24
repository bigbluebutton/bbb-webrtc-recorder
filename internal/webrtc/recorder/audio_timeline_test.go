package recorder

import (
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
)

// makeOpusPacket creates a minimal Opus RTP packet for testing.
func makeOpusPacket(seq uint16, ts uint32) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    111,
			SequenceNumber: seq,
			Timestamp:      ts,
			Marker:         true,
			SSRC:           12345,
		},
		Payload: []byte{0x00, 0x00},
	}
}

// feedOpusStreamWithLoss pushes an Opus RTP stream into the recorder with a
// configurable loss pattern. Each frame is 20ms (960 samples at 48kHz).
// Frames marked as lost are skipped (sequence number advances, no packet sent).
func feedOpusStreamWithLoss(t *testing.T, r *WebmRecorder, totalFrames int, lossInterval int) {
	t.Helper()
	seq := uint16(0)
	for i := 0; i < totalFrames; i++ {
		if lossInterval > 0 && i > 0 && i%lossInterval == 0 {
			seq++
			continue
		}
		pkt := makeOpusPacket(seq, uint32(i*960))
		r.PushAudio(pkt)
		seq++
	}
}

// TestAudioTimelinePreservedOnClose verifies that after Close() flushes the
// samplebuilder, the audio timestamp preserves the wall-clock span of the
// source even under packet loss.
//
// Root cause: pushOpus calls Pop() (non-force), which blocks on the
// samplebuilder's sequence-gap gate when loss occurs. Valid samples remain
// buffered indefinitely. Without a flush on close, those samples — and the
// timeline span they carry — are silently lost, shortening the recording by
// an amount proportional to the loss rate.
//
// Fix: close() now calls flushBuilders(), which drains remaining samples via
// ForcePopWithTimestamp before closing the writers. Additionally, pushOpus now
// uses PopWithTimestamp and derives duration from the RTP timestamp delta
// (mirroring pushVP8Custom), eliminating the first-frame duration=0 edge case.
func TestAudioTimelinePreservedOnClose(t *testing.T) {
	tests := []struct {
		name         string
		totalFrames  int
		lossInterval int // 0 = no loss
	}{
		{"no_loss", 100, 0},
		{"3pct_loss", 200, 33},
		{"5pct_loss", 1000, 20},
		{"10pct_loss", 500, 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewWebmRecorder("/dev/null", 0600, 256, 32, true, false)
			r.SetHasAudio(true)
			r.SetHasVideo(false)

			feedOpusStreamWithLoss(t, r, tt.totalFrames, tt.lossInterval)
			r.Close()

			expected := time.Duration(tt.totalFrames) * 20 * time.Millisecond
			actual := r.AudioTimestamp()
			deficitMs := expected.Milliseconds() - actual.Milliseconds()
			deficitPercent := float64(deficitMs) / float64(expected.Milliseconds()) * 100

			t.Logf("%s: audioTS=%v expected=%v deficit=%dms (%.2f%%)",
				tt.name, actual, expected, deficitMs, deficitPercent)

			// After the fix, the deficit should be exactly one frame (20ms)
			// from the first sample's zero-duration. This is a CONSTANT offset,
			// not proportional to the loss rate or recording length.
			assert.Equal(t, int64(20), deficitMs,
				"deficit should be exactly one frame (20ms constant), got %dms (%.2f%%)",
				deficitMs, deficitPercent)
		})
	}
}

// TestAudioTimelineNoLossConsistency verifies the no-loss baseline.
func TestAudioTimelineNoLossConsistency(t *testing.T) {
	r := NewWebmRecorder("/dev/null", 0600, 256, 32, true, false)
	r.SetHasAudio(true)
	r.SetHasVideo(false)

	feedOpusStreamWithLoss(t, r, 200, 0)
	r.Close()

	expected := time.Duration(200) * 20 * time.Millisecond
	actual := r.AudioTimestamp()
	assert.Equal(t, int64(20), expected.Milliseconds()-actual.Milliseconds(),
		"no-loss deficit should be exactly one frame")
}
