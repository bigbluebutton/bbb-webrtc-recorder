package recorder

import (
	"testing"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodecForFindsTheCodecsTheRecorderWrites(t *testing.T) {
	for mime, want := range map[string]*Codec{
		"video/vp8":  CodecVP8,
		"video/VP8":  CodecVP8,
		"audio/opus": CodecOpus,
		"audio/OPUS": CodecOpus,
	} {
		got, ok := CodecFor(mime)
		require.True(t, ok, mime)
		assert.Same(t, want, got, mime)
	}

	for _, mime := range []string{"", "video/h265", "video/av1", "video/vp9", "audio/red", "vp8"} {
		_, ok := CodecFor(mime)
		assert.False(t, ok, mime)
	}
}

func TestCodecsDescribeTheirTracks(t *testing.T) {
	assert.Equal(t, "video/vp8", CodecVP8.MimeType)
	assert.True(t, CodecVP8.Video)
	assert.Equal(t, uint32(90000), CodecVP8.ClockRate)
	assert.Equal(t, "V_VP8", CodecVP8.MatroskaID)
	assert.True(t, CodecVP8.WebM)
	assert.IsType(t, &codecs.VP8Packet{}, CodecVP8.NewDepacketizer())

	assert.Equal(t, "audio/opus", CodecOpus.MimeType)
	assert.False(t, CodecOpus.Video)
	assert.Equal(t, uint32(48000), CodecOpus.ClockRate)
	assert.Equal(t, "A_OPUS", CodecOpus.MatroskaID)
	assert.True(t, CodecOpus.WebM)
	assert.IsType(t, &codecs.OpusPacket{}, CodecOpus.NewDepacketizer())
}

func TestCodecDepacketizersAreNotShared(t *testing.T) {
	assert.NotSame(t, CodecVP8.NewDepacketizer(), CodecVP8.NewDepacketizer())
}

func TestCodecKeyFrameStart(t *testing.T) {
	key := &rtp.Packet{Payload: []byte{0x10, 0x00, 0x00, 0x00, 0x9D, 0x01, 0x2A, 0x80, 0x02, 0xE0, 0x01}}
	delta := &rtp.Packet{Payload: []byte{0x10, 0x01, 0x00, 0x00}}

	assert.True(t, CodecVP8.KeyFrameStart(key))
	assert.False(t, CodecVP8.KeyFrameStart(delta))
	assert.False(t, CodecOpus.KeyFrameStart(key), "audio has no key frames")
}
