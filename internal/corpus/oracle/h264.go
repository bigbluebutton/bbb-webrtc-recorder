package oracle

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
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

	h264NALAUD    = 9
	h264NALFiller = 12
	h264STAPA     = 24
	h264FUA       = 28
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

// CheckH264Blocks compares each tagged block of t, an H.264 track, with the
// NAL units of the frame its tag names. It rebuilds them from the packets
// read in tr, whose records must give each payload as it was read. A block
// of a frame that was not read whole fails too.
func CheckH264Blocks(t *Track, tr *corpus.Trace) error {
	if len(t.H264Data) != t.Blocks || len(t.TagOK) != t.Blocks {
		return fmt.Errorf("%d blocks, %d with data and %d with tags read", t.Blocks, len(t.H264Data), len(t.TagOK))
	}

	frames := h264FramesRead(tr)
	var problems []string
	for k, data := range t.H264Data {
		// VideoPlacement fails a block with no tag, or a tag no frame has.
		f, ok := frames[t.Tags[k]]
		if !t.TagOK[k] || !ok {
			continue
		}

		at := fmt.Sprintf("block %d (PTS %d ms)", k, t.PTS[k])
		switch {
		case f.err != nil:
			problems = append(problems, fmt.Sprintf("%s: frame %d was not read whole: %v", at, t.Tags[k], f.err))
		case !bytes.Equal(data, f.data):
			i := 0
			for i < min(len(data), len(f.data)) && data[i] == f.data[i] {
				i++
			}
			problems = append(problems, fmt.Sprintf("%s: %d bytes, but the NAL units of frame %d are %d, first different at byte %d",
				at, len(data), t.Tags[k], len(f.data), i))
		}
	}

	if len(problems) == 0 {
		return nil
	}

	shown := problems[:min(len(problems), 5)]

	return fmt.Errorf("%s track %d: %d blocks unlike their frames: %s", t.Codec, t.Number, len(problems), strings.Join(shown, "; "))
}

// h264Frame is one RTP timestamp's NAL units, as the recorder writes an
// access unit: AVCC, without AUD and filler. err says why the frame was not
// read whole.
type h264Frame struct {
	data []byte
	err  error
}

// h264Packet is a packet read, with its sequence number unwrapped in read
// order.
type h264Packet struct {
	seq int64
	r   corpus.Record
}

// h264FramesRead depacketizes the frames read in tr, keyed by RTP
// timestamp. A packet read again, such as an RTX copy, counts once.
func h264FramesRead(tr *corpus.Trace) map[uint32]*h264Frame {
	byTS := map[uint32][]h264Packet{}
	seen := map[int64]bool{}
	var (
		seq     int64
		prev    uint16
		started bool
	)
	for _, r := range tr.Records {
		if r.T != corpus.RecordRTP {
			continue
		}

		if started {
			seq += int64(int16(r.Seq - prev))
		} else {
			seq, started = int64(r.Seq), true
		}
		prev = r.Seq

		if r.Pad || max(r.PLen, len(r.Pay)) == 0 || seen[seq] {
			continue
		}
		seen[seq] = true
		byTS[r.TS] = append(byTS[r.TS], h264Packet{seq: seq, r: r})
	}

	frames := make(map[uint32]*h264Frame, len(byTS))
	for ts, packets := range byTS {
		slices.SortFunc(packets, func(a, b h264Packet) int { return cmp.Compare(a.seq, b.seq) })
		f := &h264Frame{}
		f.err = f.depacketize(packets)
		frames[ts] = f
	}

	return frames
}

// depacketize adds the NAL units of one frame's packets, in sequence order,
// as RFC 6184 packs them: single NAL units, STAP-A and FU-A. The frame is
// whole when its sequence numbers follow on, every FU-A ends, and its last
// packet has the marker.
func (f *h264Frame) depacketize(packets []h264Packet) error {
	fua := -1 // the open FU-A's offset in data
	for i, p := range packets {
		if i > 0 && p.seq != packets[i-1].seq+1 {
			return fmt.Errorf("sequence numbers %d-%d not read", uint16(packets[i-1].seq+1), uint16(p.seq-1))
		}

		pay := p.r.Packet().Payload
		switch t := pay[0] & 0x1f; {
		case fua >= 0 && t != h264FUA:
			return fmt.Errorf("sequence number %d: NAL unit type %d inside an FU-A", p.r.Seq, t)
		case t >= 1 && t <= 23:
			f.add(pay)
		case t == h264STAPA:
			for rest := pay[1:]; len(rest) > 0; {
				n := 0
				if len(rest) >= 2 {
					n = int(binary.BigEndian.Uint16(rest))
				}
				if n == 0 || n > len(rest)-2 {
					return fmt.Errorf("sequence number %d: a STAP-A NAL unit size of %d in the %d bytes left", p.r.Seq, n, len(rest))
				}
				f.add(rest[2 : 2+n])
				rest = rest[2+n:]
			}
		case t == h264FUA:
			if len(pay) < 3 {
				return fmt.Errorf("sequence number %d: an FU-A without data", p.r.Seq)
			}

			start, end := pay[1]&0x80 != 0, pay[1]&0x40 != 0
			switch {
			case start && fua >= 0:
				return fmt.Errorf("sequence number %d: an FU-A start inside another FU-A", p.r.Seq)
			case !start && fua < 0:
				return fmt.Errorf("sequence number %d: an FU-A continuation without a start", p.r.Seq)
			case start:
				fua = len(f.data)
				f.data = append(f.data, 0, 0, 0, 0, pay[0]&0xe0|pay[1]&0x1f)
			}
			f.data = append(f.data, pay[2:]...)
			if end {
				binary.BigEndian.PutUint32(f.data[fua:], uint32(len(f.data)-fua-4))
				if t := f.data[fua+4] & 0x1f; t == h264NALAUD || t == h264NALFiller {
					f.data = f.data[:fua]
				}
				fua = -1
			}
		default:
			return fmt.Errorf("sequence number %d: packetization type %d", p.r.Seq, t)
		}
	}

	switch {
	case fua >= 0:
		return errors.New("an FU-A not ended")
	case !packets[len(packets)-1].r.M:
		return errors.New("no marker")
	}

	return nil
}

func (f *h264Frame) add(nal []byte) {
	if t := nal[0] & 0x1f; t == h264NALAUD || t == h264NALFiller {
		return
	}

	f.data = binary.BigEndian.AppendUint32(f.data, uint32(len(nal)))
	f.data = append(f.data, nal...)
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
