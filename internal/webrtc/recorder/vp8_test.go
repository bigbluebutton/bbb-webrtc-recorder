package recorder

import (
	"path/filepath"
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
)

// VP8 payloads that are only a payload descriptor, with and without the
// start bit, carry no frame header to read.
var descriptorOnlyPayloads = [][]byte{{0x10}, {0x80, 0x00}, {0x90, 0x00}, {0x90, 0x80, 0x81, 0x02}}

func TestVP8HelpersOnDescriptorOnlyPayloads(t *testing.T) {
	for _, payload := range descriptorOnlyPayloads {
		p := &rtp.Packet{Payload: payload}
		assert.False(t, IsVP8KeyFrame(p), "%x", payload)

		width, height := GetVP8KFDimension(p)
		assert.Zero(t, width, "%x", payload)
		assert.Zero(t, height, "%x", payload)
	}
}

func TestWebmRecorderDescriptorOnlyVideoPackets(t *testing.T) {
	for _, custom := range []bool{false, true} {
		rec := NewWebmRecorder(filepath.Join(t.TempDir(), "video.webm"), 0o600, 256, 32, custom, false)
		rec.SetHasVideo(true)

		assert.NotPanics(t, func() {
			for i, payload := range descriptorOnlyPayloads {
				rec.PushVideo(&rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i), Timestamp: 3000}, Payload: payload})
			}
		}, "custom sampler %v", custom)

		rec.Close()
	}
}

// GetVP8KFDimension is handed the packet that completes a key frame, which can
// carry less than a key frame header.
func TestWebmRecorderKeyFrameWithShortLastPacket(t *testing.T) {
	keyFrame := func(seq uint16, ts uint32) []*rtp.Packet {
		return []*rtp.Packet{
			{Header: rtp.Header{SequenceNumber: seq, Timestamp: ts}, Payload: []byte{
				0x10, 0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x80, 0x02, 0xe0, 0x01, 0xaa, 0xaa,
			}},
			{Header: rtp.Header{SequenceNumber: seq + 1, Timestamp: ts, Marker: true}, Payload: []byte{0x00, 0xa0, 0xa1, 0xa2}},
		}
	}

	rec := NewWebmRecorder(filepath.Join(t.TempDir(), "video.webm"), 0o600, 256, 32, false, false)
	rec.SetHasVideo(true)

	assert.NotPanics(t, func() {
		for _, p := range append(keyFrame(1, 3000), keyFrame(3, 6000)...) {
			rec.PushVideo(p)
		}
	})

	rec.Close()
}
