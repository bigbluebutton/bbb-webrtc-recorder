package oracle

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
)

const (
	h264NALSlice = 1
	h264NALIDR   = 5
	h264NALSPS   = 7
	h264NALPPS   = 8
	// The only NAL units of one byte: they have no payload.
	h264NALEndOfSeq    = 10
	h264NALEndOfStream = 11
)

// h264Block is what the H.264 checks read from a block.
type h264Block struct {
	idr bool
	// sps and pps are the block's first SPS and PPS, nil without one.
	sps, pps []byte
	// firstSlice is the block's first slice NAL unit, nil without one.
	firstSlice []byte
}

// parseH264Block reads the NAL units of an AVCC block. The block must not
// be empty, and 4-byte lengths, none zero, must cover it exactly. Each NAL
// unit needs a valid header and, unless it ends a sequence or stream, a
// payload. The units before an error are still read.
//
// An Annex-B start code fails too. Read as a length, 00 00 00 01 gives a
// NAL unit of one byte, with no payload. Read as a NAL unit header, a
// start code's zero byte gives type 0.
func parseH264Block(data []byte) (h264Block, error) {
	var b h264Block
	if len(data) == 0 {
		return b, errors.New("an empty block")
	}

	for i, rest := 0, data; len(rest) > 0; i++ {
		if len(rest) < 4 {
			return b, fmt.Errorf("NAL unit %d: %d bytes left, too few for a length", i, len(rest))
		}

		n := binary.BigEndian.Uint32(rest)
		rest = rest[4:]
		switch {
		case n == 0:
			return b, fmt.Errorf("NAL unit %d: a zero length", i)
		case uint64(n) > uint64(len(rest)):
			return b, fmt.Errorf("NAL unit %d: a length of %d overruns the %d bytes left", i, n, len(rest))
		}

		nal := rest[:n]
		rest = rest[n:]
		if err := checkH264NAL(nal); err != nil {
			return b, fmt.Errorf("NAL unit %d: %w", i, err)
		}

		switch nal[0] & 0x1f {
		case h264NALIDR:
			b.idr = true
			fallthrough
		case h264NALSlice:
			if b.firstSlice == nil {
				b.firstSlice = nal
			}
		case h264NALSPS:
			if b.sps == nil {
				b.sps = nal
			}
		case h264NALPPS:
			if b.pps == nil {
				b.pps = nal
			}
		}
	}

	return b, nil
}

func checkH264NAL(nal []byte) error {
	t := nal[0] & 0x1f
	switch {
	case nal[0]&0x80 != 0:
		return errors.New("forbidden_zero_bit set")
	case t == 0 || t > 23:
		return fmt.Errorf("NAL unit type %d", t)
	case len(nal) < 2 && t != h264NALEndOfSeq && t != h264NALEndOfStream:
		return fmt.Errorf("a NAL unit of type %d and one byte", t)
	case t == h264NALSPS && len(nal) < 4:
		return errors.New("an SPS without its profile and level")
	}

	return nil
}

// h264Config is a track's avcC record: its first SPS and PPS.
type h264Config struct {
	sps, pps []byte
	// keyed is set once the first key block was checked against them.
	keyed bool
}

// checkH264Block adds block k's problems to t and counts its IDR. The key
// flag must mark an IDR, and a key block must carry an SPS and a PPS. The
// first block must be a key block. The first key block must carry the SPS
// and PPS of c, which come from CodecPrivate.
func (t *Track) checkH264Block(k int, fr Block, b h264Block, err error, c *h264Config) {
	fail := func(format string, args ...any) {
		t.H264Errors = append(t.H264Errors, fmt.Sprintf("block %d (PTS %d ms): ", k, fr.PTS)+fmt.Sprintf(format, args...))
	}

	if err != nil {
		fail("%v", err)
	}

	if b.idr {
		t.H264Keyframes++
	}

	switch {
	case k == 0 && !fr.Key:
		fail("the first block is not a key block")
	case fr.Key && !b.idr:
		fail("key flag set, but no IDR slice")
	case !fr.Key && b.idr:
		fail("an IDR slice, but no key flag")
	}

	if fr.Key && b.sps == nil {
		fail("a key block without an SPS")
	}
	if fr.Key && b.pps == nil {
		fail("a key block without a PPS")
	}

	if fr.Key && c != nil && !c.keyed {
		c.keyed = true
		if b.sps != nil && !bytes.Equal(b.sps, c.sps) {
			fail("the first key block's SPS is not CodecPrivate's")
		}
		if b.pps != nil && !bytes.Equal(b.pps, c.pps) {
			fail("the first key block's PPS is not CodecPrivate's")
		}
	}
}

// checkAVCC checks an avcC record (ISO/IEC 14496-15 5.3.3.1) and returns
// its first SPS and PPS. It needs version 1, 4-byte NAL lengths, an SPS and
// a PPS. The record's profile, compatibility and level bytes must match
// the first SPS's.
func checkAVCC(cp []byte) (*h264Config, error) {
	if len(cp) < 7 {
		return nil, fmt.Errorf("%d bytes, too short for an avcC record", len(cp))
	}
	if cp[0] != 1 {
		return nil, fmt.Errorf("avcC version %d, want 1", cp[0])
	}
	if n := cp[4] & 0x03; n != 3 {
		return nil, fmt.Errorf("lengthSizeMinusOne %d, want 3", n)
	}

	rest := cp[6:]
	sets := func(name string, count int, nalType byte) ([][]byte, error) {
		var nals [][]byte
		for i := range count {
			if len(rest) < 2 {
				return nil, fmt.Errorf("%s %d: no length", name, i)
			}

			n := int(binary.BigEndian.Uint16(rest))
			rest = rest[2:]
			switch {
			case n == 0:
				return nil, fmt.Errorf("%s %d: a zero length", name, i)
			case n > len(rest):
				return nil, fmt.Errorf("%s %d of %d bytes overruns the %d left", name, i, n, len(rest))
			case rest[0]&0x1f != nalType:
				return nil, fmt.Errorf("%s %d: NAL unit type %d", name, i, rest[0]&0x1f)
			}

			nals = append(nals, rest[:n])
			rest = rest[n:]
		}

		return nals, nil
	}

	count := int(cp[5] & 0x1f)
	if count == 0 {
		return nil, errors.New("no SPS")
	}

	sps, err := sets("SPS", count, h264NALSPS)
	if err != nil {
		return nil, err
	}
	if len(sps[0]) < 4 {
		return nil, errors.New("SPS 0: no profile and level")
	}
	if !bytes.Equal(cp[1:4], sps[0][1:4]) {
		return nil, fmt.Errorf("profile, compatibility and level %x, the first SPS has %x", cp[1:4], sps[0][1:4])
	}

	if len(rest) == 0 {
		return nil, errors.New("no PPS count")
	}

	count = int(rest[0])
	rest = rest[1:]
	if count == 0 {
		return nil, errors.New("no PPS")
	}

	pps, err := sets("PPS", count, h264NALPPS)
	if err != nil {
		return nil, err
	}

	return &h264Config{sps: sps[0], pps: pps[0]}, nil
}

// CheckH264 returns an error naming the first H.264 problems, if any.
func (t *Track) CheckH264() error {
	if len(t.H264Errors) == 0 {
		return nil
	}

	shown := t.H264Errors[:min(len(t.H264Errors), 5)]

	return fmt.Errorf("%s track %d: %d H.264 problems: %s", t.Codec, t.Number, len(t.H264Errors), strings.Join(shown, "; "))
}

// h264FrameTag reads the corpus.TagH264Frame tag in a block's first slice.
// The in-band parameter sets before it can push it past the 64 bytes
// TaggedFrame searches.
func h264FrameTag(b h264Block) (uint32, bool) {
	if b.firstSlice == nil {
		return 0, false
	}

	return corpus.TaggedFrame(b.firstSlice)
}
