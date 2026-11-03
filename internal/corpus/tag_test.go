package corpus

import (
	"testing"

	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTagFrameRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		name string
		key  bool
		at   int // the tag's offset in the de-packetized frame
	}{
		{"key", true, 10},
		{"delta", false, 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			payload := make([]byte, 400)
			pay := VP8Payload(0x1234, true, tt.key, 640, 480)
			copy(payload, pay)

			require.True(t, TagFrame(payload, KindVideo, tt.key, 0xDEADBEEF))

			v, ok := TaggedFrame(payload)
			require.True(t, ok)
			assert.Equal(t, uint32(0xDEADBEEF), v)

			var p codecs.VP8Packet
			_, err := p.Unmarshal(payload)
			require.NoError(t, err)
			assert.Equal(t, pay[VP8DescriptorLen(pay):], p.Payload[:len(pay)-VP8DescriptorLen(pay)], "the frame header is unchanged")
			assert.Equal(t, []byte("BBBF"), p.Payload[tt.at:tt.at+4])

			v, ok = TaggedFrame(p.Payload)
			require.True(t, ok)
			assert.Equal(t, uint32(0xDEADBEEF), v)
		})
	}
}

func TestTagFrameOpus(t *testing.T) {
	payload := []byte{0xFC, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	require.True(t, TagFrame(payload, KindAudio, false, 7))

	assert.Equal(t, byte(0xFC), payload[0])
	v, ok := TaggedFrame(payload)
	require.True(t, ok)
	assert.Equal(t, uint32(7), v)
}

func TestTagFrameTooShort(t *testing.T) {
	payload := make([]byte, vp8MinPLen)
	copy(payload, VP8Payload(1, true, true, 640, 480))
	want := append([]byte(nil), payload...)

	assert.False(t, TagFrame(payload, KindVideo, true, 1))
	assert.Equal(t, want, payload)

	_, ok := TaggedFrame(payload)
	assert.False(t, ok)

	delta := make([]byte, vp8MinPLen)
	copy(delta, VP8Payload(1, true, false, 640, 480))
	assert.False(t, TagFrame(delta, KindVideo, false, 1), "GenerateVP8 allows a delta frame one byte short of the tag")
}

func TestVP8DescriptorLen(t *testing.T) {
	assert.Equal(t, 4, VP8DescriptorLen(VP8Payload(1, true, false, 640, 480)))
	assert.Equal(t, 1, VP8DescriptorLen([]byte{0x10}), "no extension byte")
	assert.Equal(t, 3, VP8DescriptorLen([]byte{0x90, 0x80, 0x05}), "7-bit picture ID")
	assert.Equal(t, 6, VP8DescriptorLen([]byte{0x90, 0xF0, 0x80, 0x01, 0x02, 0x03}), "picture ID, TL0PICIDX and TID")
	assert.Equal(t, 0, VP8DescriptorLen([]byte{0x90}), "too short for the extension byte")
	assert.Equal(t, 0, VP8DescriptorLen(nil))
}

func TestVP8PictureID(t *testing.T) {
	long := VP8Payload(0x1234, true, false, 640, 480)
	id, bits := VP8PictureID(long)
	assert.Equal(t, uint16(0x1234), id)
	assert.Equal(t, 15, bits)

	SetVP8PictureID(long, 0x8001)
	id, _ = VP8PictureID(long)
	assert.Equal(t, uint16(0x0001), id, "15 bits kept")
	assert.Equal(t, byte(0x80), long[2]&0x80, "M bit kept")

	short := []byte{0x90, 0x80, 0x05}
	id, bits = VP8PictureID(short)
	assert.Equal(t, uint16(5), id)
	assert.Equal(t, 7, bits)

	SetVP8PictureID(short, 0x1FF)
	id, bits = VP8PictureID(short)
	assert.Equal(t, uint16(0x7F), id, "7 bits kept")
	assert.Equal(t, 7, bits)

	_, bits = VP8PictureID([]byte{0x10, 0, 0})
	assert.Zero(t, bits, "no extension byte")
	_, bits = VP8PictureID([]byte{0x90, 0x80, 0x80})
	assert.Zero(t, bits, "too short for a 15-bit picture ID")
}
