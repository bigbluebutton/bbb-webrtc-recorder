package corpus

import (
	"errors"
	"fmt"
	"math"
	"time"
)

const (
	vp8DescriptorLen = 4  // X|S byte, I extension byte, 15-bit picture ID
	vp8KeyHeaderLen  = 10 // frame tag, start code, width, height (RFC 6386 9.1)
	vp8MinPLen       = vp8DescriptorLen + vp8KeyHeaderLen

	opusTOC         = 0xFC // config 31 (CELT FB, 20 ms), stereo, one frame
	opusFrameUs     = 20000
	opusFrameTicks  = 960
	defaultVP8PT    = 96
	defaultOpusPT   = 111
	defaultKeyPLen  = 1000
	defaultDelPLen  = 400
	defaultOpusPLen = 80
)

// VP8Options configures GenerateVP8; zero values select the defaults noted.
type VP8Options struct {
	FPS      float64       // frames per second, required
	Frames   int           // frame count; set this or Duration
	Duration time.Duration // stream length; set this or Frames

	KeyPackets   int   // packets per key frame (default 4)
	DeltaPackets int   // packets per delta frame (default 2)
	KeyInterval  int   // a key frame every KeyInterval frames; 0 means frame 0 only
	KeyPLen      int   // payload bytes per key frame packet (default 1000, min 14)
	DeltaPLen    int   // payload bytes per delta frame packet (default 400, min 14)
	SpreadUs     int64 // a frame's packets are read evenly over this span
	Width        int   // key frame width (default 640)
	Height       int   // key frame height (default 480)

	StartSeq       uint16
	StartTS        uint32
	StartPictureID uint16
	SSRC           uint32
	PT             uint8 // default 96
}

// OpusOptions configures GenerateOpus; zero values select the defaults noted.
type OpusOptions struct {
	Frames   int           // 20 ms frame count; set this or Duration
	Duration time.Duration // stream length; set this or Frames
	PLen     int           // payload bytes per packet (default 80)

	StartSeq uint16
	StartTS  uint32
	SSRC     uint32
	PT       uint8 // default 111
}

// GenerateVP8 returns a synthetic VP8 sent schedule whose payload prefixes
// carry real descriptors and frame headers, so the WebM writer accepts them.
func GenerateVP8(o VP8Options) (*Trace, error) {
	frames, err := frameCount(o.Frames, o.Duration, o.FPS)
	if err != nil {
		return nil, err
	}

	o.KeyPackets = withDefault(o.KeyPackets, 4)
	o.DeltaPackets = withDefault(o.DeltaPackets, 2)
	o.KeyPLen = withDefault(o.KeyPLen, defaultKeyPLen)
	o.DeltaPLen = withDefault(o.DeltaPLen, defaultDelPLen)
	o.Width = withDefault(o.Width, 640)
	o.Height = withDefault(o.Height, 480)
	o.PT = uint8(withDefault(int(o.PT), defaultVP8PT))

	switch {
	case o.KeyPackets < 1 || o.DeltaPackets < 1:
		return nil, errors.New("packets per frame must be positive")
	case o.KeyPLen < vp8MinPLen || o.DeltaPLen < vp8MinPLen:
		return nil, fmt.Errorf("payload length must be at least %d", vp8MinPLen)
	case o.KeyInterval < 0 || o.SpreadUs < 0:
		return nil, errors.New("key interval and spread must not be negative")
	case o.Width < 1 || o.Width > 0x3FFF || o.Height < 1 || o.Height > 0x3FFF:
		return nil, fmt.Errorf("dimensions %dx%d out of the 14-bit range", o.Width, o.Height)
	}

	recs := make([]Record, 0, frames*o.DeltaPackets+o.KeyPackets)
	seq := o.StartSeq
	var prevAt int64

	for f := range frames {
		key := f == 0 || (o.KeyInterval > 0 && f%o.KeyInterval == 0)
		n, plen := o.DeltaPackets, o.DeltaPLen
		if key {
			n, plen = o.KeyPackets, o.KeyPLen
		}

		ts := o.StartTS + uint32(math.Round(float64(f)*vp8ClockRate/o.FPS))
		frameAt := int64(math.Round(float64(f) * 1e6 / o.FPS))
		pic := (o.StartPictureID + uint16(f)) & 0x7FFF

		for j := range n {
			at := frameAt
			if n > 1 {
				at += int64(j) * o.SpreadUs / int64(n-1)
			}
			// A spread longer than the frame interval delays the next frame, as a FIFO pacer does.
			at = max(at, prevAt)
			prevAt = at

			recs = append(recs, Record{
				T:    RecordRTP,
				AtUs: at,
				Seq:  seq,
				TS:   ts,
				M:    j == n-1,
				PT:   o.PT,
				PLen: plen,
				Pay:  VP8Payload(pic, j == 0, key, o.Width, o.Height),
			})
			seq++
		}
	}

	return &Trace{Header: NewSyntheticHeader(KindVideo, o.SSRC, "corpus.GenerateVP8"), Records: recs}, nil
}

// GenerateOpus returns a synthetic Opus sent schedule of 20 ms frames.
func GenerateOpus(o OpusOptions) (*Trace, error) {
	frames, err := frameCount(o.Frames, o.Duration, 1e6/opusFrameUs)
	if err != nil {
		return nil, err
	}

	o.PLen = withDefault(o.PLen, defaultOpusPLen)
	o.PT = uint8(withDefault(int(o.PT), defaultOpusPT))

	if o.PLen < 1 {
		return nil, errors.New("payload length must be positive")
	}

	recs := make([]Record, frames)
	for f := range frames {
		recs[f] = Record{
			T:    RecordRTP,
			AtUs: int64(f) * opusFrameUs,
			Seq:  o.StartSeq + uint16(f),
			TS:   o.StartTS + uint32(f)*opusFrameTicks,
			M:    f == 0, // the first packet of a talkspurt
			PT:   o.PT,
			PLen: o.PLen,
			Pay:  []byte{opusTOC},
		}
	}

	return &Trace{Header: NewSyntheticHeader(KindAudio, o.SSRC, "corpus.GenerateOpus"), Records: recs}, nil
}

// VP8Payload returns the payload prefix of one packet of picture pic: the
// descriptor, plus the frame header on a frame's first packet.
func VP8Payload(pic uint16, first, key bool, width, height int) []byte {
	desc := byte(0x80) // X: the extension byte follows
	if first {
		desc |= 0x10 // S: start of partition 0
	}

	// I: a 15-bit picture ID (M bit set) follows.
	pay := []byte{desc, 0x80, 0x80 | byte(pic>>8), byte(pic)}

	switch {
	case first && key:
		// Frame tag 0x000210: key (bit0 = 0), version 0, show_frame, first partition 16 bytes.
		pay = append(pay, 0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a,
			byte(width), byte(width>>8), byte(height), byte(height>>8))
	case first:
		pay = append(pay, 0x11, 0x02, 0x00) // the same tag with bit0 = 1: an interframe
	}

	return pay
}

func frameCount(frames int, d time.Duration, fps float64) (int, error) {
	if fps <= 0 || math.IsInf(fps, 0) || math.IsNaN(fps) {
		return 0, fmt.Errorf("invalid frame rate %v", fps)
	}

	switch {
	case frames > 0 && d > 0:
		return 0, errors.New("set Frames or Duration, not both")
	case frames > 0:
		return frames, nil
	case d > 0:
		if n := int(math.Round(d.Seconds() * fps)); n > 0 {
			return n, nil
		}
	}

	return 0, errors.New("the stream has no frames")
}

func withDefault(v, def int) int {
	if v == 0 {
		return def
	}

	return v
}
