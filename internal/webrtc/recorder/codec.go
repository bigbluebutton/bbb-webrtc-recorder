package recorder

import (
	"strings"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// Codec describes a codec the recorder can write.
type Codec struct {
	// MimeType is the lowercase MIME type pion negotiates, such as "video/vp8".
	MimeType  string
	Video     bool
	ClockRate uint32
	// MatroskaID is the track's CodecID in the file.
	MatroskaID string
	// WebM reports whether a WebM file may hold the codec.
	WebM bool
	// NewDepacketizer returns the depacketizer a jitter buffer reads frame
	// boundaries with.
	NewDepacketizer func() rtp.Depacketizer
	// KeyFrameStart reports whether a packet is the first of a key frame.
	KeyFrameStart func(p *rtp.Packet) bool

	// frames splits the packets of a jitter-buffer sample into frames. A
	// malformed frame becomes a lost one in its place, and the error
	// describes it.
	frames func(packets []*rtp.Packet) ([]videoFrame, error)
}

var (
	CodecVP8 = &Codec{
		MimeType:        "video/vp8",
		Video:           true,
		ClockRate:       vp8SampleRate,
		MatroskaID:      "V_VP8",
		WebM:            true,
		NewDepacketizer: func() rtp.Depacketizer { return &codecs.VP8Packet{} },
		KeyFrameStart:   IsVP8KeyFrame,
	}

	CodecOpus = &Codec{
		MimeType:        "audio/opus",
		ClockRate:       opusSampleRate,
		MatroskaID:      "A_OPUS",
		WebM:            true,
		NewDepacketizer: func() rtp.Depacketizer { return &codecs.OpusPacket{} },
		KeyFrameStart:   func(*rtp.Packet) bool { return false },
	}

	CodecH264 = &Codec{
		MimeType:        "video/h264",
		Video:           true,
		ClockRate:       90000,
		MatroskaID:      "V_MPEG4/ISO/AVC",
		NewDepacketizer: func() rtp.Depacketizer { return &codecs.H264Packet{} },
		KeyFrameStart:   isH264KeyFrameStart,
		frames:          h264Frames,
	}

	registry = []*Codec{CodecVP8, CodecOpus, CodecH264}
)

// TakesSamples reports whether the codec's video goes to PushVideoSample,
// whole jitter-buffer samples, instead of one packet at a time.
func (c *Codec) TakesSamples() bool {
	return c.frames != nil
}

// CodecFor returns the codec of mimeType, matched case-insensitively, or
// false when the recorder cannot write it.
func CodecFor(mimeType string) (*Codec, bool) {
	for _, c := range registry {
		if strings.EqualFold(c.MimeType, mimeType) {
			return c, true
		}
	}

	return nil, false
}
