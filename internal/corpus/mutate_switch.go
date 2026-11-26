package corpus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
)

// The 2x2 key frame LiveKit's downtrack sends as an H.264 blank frame
// (livekit-server pkg/sfu/downtrack.go, H264KeyFrame2x2).
var (
	h264BlankSPS = []byte{
		0x67, 0x42, 0xc0, 0x1f, 0x0f, 0xd9, 0x1f, 0x88, 0x88, 0x84, 0x00, 0x00,
		0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xc8, 0x3c, 0x60, 0xc9, 0x20,
	}
	h264BlankPPS = []byte{0x68, 0x87, 0xcb, 0x83, 0xcb, 0x20}
	h264BlankIDR = []byte{0x65, 0x88, 0x84, 0x0a, 0xf2, 0x62, 0x80, 0x00, 0xa7, 0xbe}
)

func firstOriginal(recs []Record, seq uint16) (int, error) {
	i := slices.IndexFunc(recs, func(r Record) bool { return isOriginal(r) && r.Seq == seq })
	if i < 0 {
		return 0, fmt.Errorf("no packet %d", seq)
	}

	return i, nil
}

// Switch models a LiveKit simulcast layer switch right after packet seq:
// the SFU forwards nothing read after it, and next's packets follow it.
// Their sequence numbers start at seq + 1, their timestamps one tick after
// seq's, and their read times at seq's read. Each keeps its offset within
// next.
//
// The SFU switches only at a key frame, so next should start with one.
// The eofs are dropped: apply EOFAt after.
func Switch(seq uint16, next []Record) Op {
	return func(recs []Record) ([]Record, error) {
		i, err := firstOriginal(recs, seq)
		if err != nil {
			return nil, err
		}

		first := slices.IndexFunc(next, func(r Record) bool { return r.T == RecordRTP })
		if first < 0 {
			return nil, errors.New("switch to a layer with no packet")
		}

		base, from := recs[i], next[first]
		out := slices.Clone(recs[:i+1])
		for _, r := range next {
			if r.T != RecordRTP {
				continue
			}

			r.Seq = base.Seq + 1 + (r.Seq - from.Seq)
			r.TS = base.TS + 1 + (r.TS - from.TS)
			r.AtUs = base.AtUs + (r.AtUs - from.AtUs)
			out = append(out, r)
		}

		return out, nil
	}
}

// Padding inserts count padding packets of plen bytes after the original
// packet seq, as LiveKit sends its probes at a frame boundary. They are read
// with seq, take its timestamp and the next sequence numbers, and every
// packet after seq moves up by count.
func Padding(seq uint16, count, plen int) Op {
	return func(recs []Record) ([]Record, error) {
		if count < 1 || plen < 0 {
			return nil, fmt.Errorf("invalid padding: %d packets of %d bytes", count, plen)
		}

		i, err := firstOriginal(recs, seq)
		if err != nil {
			return nil, err
		}

		out := slices.Clone(recs)
		for k, r := range out {
			if r.T == RecordRTP && int16(r.Seq-seq) > 0 {
				out[k].Seq += uint16(count)
			}
		}

		at := recs[i]
		pads := make([]Record, 0, count)
		for k := range count {
			pads = append(pads, Record{T: RecordRTP, AtUs: at.AtUs, Seq: seq + 1 + uint16(k), TS: at.TS, PT: at.PT, Pad: true, PLen: plen})
		}

		return slices.Insert(out, i+1, pads...), nil
	}
}

// H264BlankFrame is the payload of LiveKit's H.264 blank frame: one STAP-A
// of the SPS, PPS and IDR of a 2x2 picture.
func H264BlankFrame() []byte {
	out := []byte{0x18}
	for _, nal := range [][]byte{h264BlankSPS, h264BlankPPS, h264BlankIDR} {
		out = binary.BigEndian.AppendUint16(out, uint16(len(nal)))
		out = append(out, nal...)
	}

	return out
}

// H264BlankClose appends the blank frames LiveKit sends as an H.264
// downtrack closes, 6 over its 200 ms. Each frame is one H264BlankFrame
// packet with the marker, at 30 fps after the last original packet, with the
// next sequence numbers. The eofs are dropped: apply EOFAt after.
func H264BlankClose(frames int) Op {
	return func(recs []Record) ([]Record, error) {
		if frames < 1 {
			return nil, fmt.Errorf("invalid blank frame count %d", frames)
		}

		last := -1
		for i, r := range recs {
			if isOriginal(r) {
				last = i
			}
		}
		if last < 0 {
			return nil, errors.New("no packet to close after")
		}

		out := slices.DeleteFunc(slices.Clone(recs), func(r Record) bool { return r.T == RecordEOF })
		at := recs[last]
		pay := H264BlankFrame()
		for k := 1; k <= frames; k++ {
			out = append(out, Record{
				T: RecordRTP, AtUs: at.AtUs + int64(math.Round(float64(k)*1e6/30)), Seq: at.Seq + uint16(k),
				TS: at.TS + uint32(k)*h264ClockRate/30, M: true, PT: at.PT, PLen: len(pay), Pay: slices.Clone(pay),
			})
		}

		return out, nil
	}
}

// PauseH264 is Pause for an H.264 track, which has no picture ID.
func PauseH264(fromUs, toUs int64) Op { return pause(fromUs, toUs, false) }
