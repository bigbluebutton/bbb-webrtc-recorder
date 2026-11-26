package corpus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
)

const (
	h264ClockRate  = 90000
	defaultH264PT  = 102
	h264STAPA      = 24
	h264FUA        = 28
	h264NALTypeSPS = 7
	h264NALTypePPS = 8
	h264NALTypeIDR = 5
	// nal_ref_idc 3 with the IDR, SPS and PPS types; 2 with a P slice.
	h264HeaderIDR   = 0x65
	h264HeaderP     = 0x41
	h264HeaderPPS   = 0x68
	h264HeaderSPS   = 0x67
	h264MinSlicePay = 8 // room for the FU header and the slice header prefix
)

// h264PPS is the PPS libx264 writes for a baseline stream (pps_id 0, sps_id 0).
var h264PPS = []byte{0x68, 0xcb, 0x83, 0xcb, 0x20}

// H264Options configures GenerateH264; zero values select the defaults noted.
type H264Options struct {
	FPS      float64       // frames per second, required
	Frames   int           // frame count; set this or Duration
	Duration time.Duration // stream length; set this or Frames

	KeyInterval int // a key frame every KeyInterval frames; 0 means frame 0 only
	Slices      int // slices per frame (default 1)
	// Packets per slice (default 1): one sends the slice as a single NAL
	// unit, more as FU-A fragments.
	KeySlicePackets   int
	DeltaSlicePackets int
	KeyPLen           int   // payload bytes per key frame slice packet (default 1000)
	DeltaPLen         int   // payload bytes per delta frame slice packet (default 400)
	SpreadUs          int64 // a frame's packets are read evenly over this span
	Width             int   // even; the SPS's cropped size (default 640)
	Height            int   // even (default 360)
	// Mode0 sends each NAL unit in a packet of its own, as packetization
	// mode 0 does: no STAP-A or FU-A.
	Mode0 bool

	StartSeq uint16
	StartTS  uint32
	SSRC     uint32
	PT       uint8 // default 102
}

// GenerateH264 returns a synthetic H.264 sent schedule as libwebrtc
// packetizes one: a key frame's SPS and PPS share a STAP-A ahead of its IDR
// slices. The parameter sets are real, and each slice starts with a real
// header. The rest of each payload is zero.
func GenerateH264(o H264Options) (*Trace, error) {
	frames, err := frameCount(o.Frames, o.Duration, o.FPS)
	if err != nil {
		return nil, err
	}

	if o.Mode0 && (o.KeySlicePackets > 1 || o.DeltaSlicePackets > 1) {
		return nil, errors.New("packetization mode 0 has no FU-A: one packet per slice")
	}

	o.Slices = withDefault(o.Slices, 1)
	o.KeySlicePackets = withDefault(o.KeySlicePackets, 1)
	o.DeltaSlicePackets = withDefault(o.DeltaSlicePackets, 1)
	o.KeyPLen = withDefault(o.KeyPLen, defaultKeyPLen)
	o.DeltaPLen = withDefault(o.DeltaPLen, defaultDelPLen)
	o.Width = withDefault(o.Width, 640)
	o.Height = withDefault(o.Height, 360)
	o.PT = uint8(withDefault(int(o.PT), defaultH264PT))

	switch {
	case o.Slices < 1 || o.KeySlicePackets < 1 || o.DeltaSlicePackets < 1:
		return nil, errors.New("slices and packets per slice must be positive")
	case o.KeyPLen < h264MinSlicePay || o.DeltaPLen < h264MinSlicePay:
		return nil, fmt.Errorf("payload length must be at least %d", h264MinSlicePay)
	case o.KeyInterval < 0 || o.SpreadUs < 0:
		return nil, errors.New("key interval and spread must not be negative")
	case o.Width < 2 || o.Height < 2 || o.Width%2 != 0 || o.Height%2 != 0 || o.Width > 4096 || o.Height > 4096:
		return nil, fmt.Errorf("dimensions %dx%d must be even, 2 to 4096", o.Width, o.Height)
	}

	mbs := ((o.Width + 15) / 16) * ((o.Height + 15) / 16)
	params := [][]byte{H264SPS(o.Width, o.Height), h264PPS}

	var recs []Record
	seq := o.StartSeq
	var prevAt int64
	sinceKey := 0

	for f := range frames {
		key := f == 0 || (o.KeyInterval > 0 && f%o.KeyInterval == 0)
		if key {
			sinceKey = 0
		}

		var pays [][]byte
		var plens []int

		if key && o.Mode0 {
			for _, p := range params {
				pays, plens = append(pays, p), append(plens, len(p))
			}
		} else if key {
			stap := h264STAP(params...)
			pays, plens = append(pays, stap), append(plens, len(stap))
		}

		n, plen, header := o.DeltaSlicePackets, o.DeltaPLen, byte(h264HeaderP)
		if key {
			n, plen, header = o.KeySlicePackets, o.KeyPLen, h264HeaderIDR
		}

		for s := range o.Slices {
			slice := h264SliceHeader(header, s*mbs/o.Slices, key, sinceKey)
			for j := range n {
				pays, plens = append(pays, h264SlicePacket(slice, n, j)), append(plens, plen)
			}
		}

		ts := o.StartTS + uint32(math.Round(float64(f)*h264ClockRate/o.FPS))
		frameAt := int64(math.Round(float64(f) * 1e6 / o.FPS))

		for j, pay := range pays {
			at := frameAt
			if len(pays) > 1 {
				at += int64(j) * o.SpreadUs / int64(len(pays)-1)
			}
			at = max(at, prevAt)
			prevAt = at

			recs = append(recs, Record{
				T: RecordRTP, AtUs: at, Seq: seq, TS: ts, M: j == len(pays)-1,
				PT: o.PT, PLen: max(plens[j], len(pay)), Pay: pay,
			})
			seq++
		}

		sinceKey++
	}

	h := NewSyntheticHeader(KindVideo, o.SSRC, "corpus.GenerateH264")
	h.Codec, h.ClockRate = CodecH264, h264ClockRate

	return &Trace{Header: h, Records: recs}, nil
}

func h264STAP(nals ...[]byte) []byte {
	out := []byte{0x60 | h264STAPA}
	for _, n := range nals {
		out = binary.BigEndian.AppendUint16(out, uint16(len(n)))
		out = append(out, n...)
	}

	return out
}

// h264SlicePacket is packet j of n of slice, which holds a NAL unit header
// and slice header. One packet is the slice itself; more are FU-A fragments.
func h264SlicePacket(slice []byte, n, j int) []byte {
	if n == 1 {
		return slice
	}

	fu := slice[0] & 0x1f
	switch j {
	case 0:
		fu |= 0x80
	case n - 1:
		fu |= 0x40
	}

	out := []byte{slice[0]&0xe0 | h264FUA, fu}
	if j == 0 {
		out = append(out, slice[1:]...)
	}

	return out
}

// h264SliceHeader is a slice's NAL unit header and the start of its slice
// header (H.264 7.3.3).
func h264SliceHeader(header byte, firstMB int, idr bool, frameNum int) []byte {
	var w bitWriter
	w.ue(uint64(firstMB))
	if idr {
		w.ue(7) // I, all slices
	} else {
		w.ue(5) // P, all slices
	}
	w.ue(0)                       // pic_parameter_set_id
	w.u(4, uint64(frameNum)&0x0f) // frame_num, log2_max_frame_num 4
	if idr {
		w.ue(0) // idr_pic_id
	}

	return append([]byte{header}, w.fill()...)
}

// H264SPS returns a constrained baseline SPS NAL unit, header included, for
// a width x height 4:2:0 picture. Its frame_num has 4 bits and its
// pic_order_cnt_type is 2. A size not a multiple of 16 is cropped.
func H264SPS(width, height int) []byte {
	mbW, mbH := (width+15)/16, (height+15)/16

	var w bitWriter
	w.u(8, 66)   // profile_idc: baseline
	w.u(8, 0xc0) // constraint_set0 and set1: constrained baseline
	w.u(8, 31)   // level_idc 3.1
	w.ue(0)      // seq_parameter_set_id
	w.ue(0)      // log2_max_frame_num_minus4
	w.ue(2)      // pic_order_cnt_type
	w.ue(1)      // max_num_ref_frames
	w.u(1, 0)    // gaps_in_frame_num_value_allowed_flag
	w.ue(uint64(mbW - 1))
	w.ue(uint64(mbH - 1))
	w.u(1, 1) // frame_mbs_only_flag
	w.u(1, 1) // direct_8x8_inference_flag

	// 4:2:0 crops in pairs of samples.
	right, bottom := (mbW*16-width)/2, (mbH*16-height)/2
	if right > 0 || bottom > 0 {
		w.u(1, 1)
		w.ue(0)
		w.ue(uint64(right))
		w.ue(0)
		w.ue(uint64(bottom))
	} else {
		w.u(1, 0)
	}
	w.u(1, 0) // vui_parameters_present_flag

	return append([]byte{h264HeaderSPS}, h264Escape(w.trailing())...)
}

// H264KeyFrameStart reports whether payload starts an H.264 key frame as
// LiveKit's SFU detects one. Such a payload carries an SPS on its own, in
// a STAP-A, or as an FU-A start.
func H264KeyFrameStart(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}

	switch payload[0] & 0x1f {
	case h264NALTypeSPS:
		return true
	case h264STAPA:
		for rest := payload[1:]; len(rest) > 2; {
			n := int(binary.BigEndian.Uint16(rest))
			if n == 0 || 2+n > len(rest) {
				return false
			}
			if rest[2]&0x1f == h264NALTypeSPS {
				return true
			}
			rest = rest[2+n:]
		}
	case h264FUA:
		return len(payload) > 1 && payload[1]&0x80 != 0 && payload[1]&0x1f == h264NALTypeSPS
	}

	return false
}

// H264SliceStart reports whether payload starts a slice NAL unit: a single
// NAL unit of type 1 or 5, or the first FU-A fragment of one.
func H264SliceStart(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}

	switch t := payload[0] & 0x1f; t {
	case 1, h264NALTypeIDR:
		return true
	case h264FUA:
		return len(payload) > 1 && payload[1]&0x80 != 0 && h264IsSlice(payload[1])
	}

	return false
}

// H264PictureStart reports whether payload starts the first slice of a
// picture: alone, in an FU-A or in a STAP-A. Its slice header has
// first_mb_in_slice 0, whose exp-Golomb code is a single 1 bit.
func H264PictureStart(payload []byte) bool {
	_, ok := h264PictureSlice(payload)

	return ok
}

// h264PictureSlice returns the part of payload that starts at the NAL unit
// or FU-A header of a picture's first slice.
func h264PictureSlice(payload []byte) ([]byte, bool) {
	if len(payload) > 0 && payload[0]&0x1f == h264STAPA {
		for rest := payload[1:]; len(rest) > 2; {
			n := int(binary.BigEndian.Uint16(rest))
			if n == 0 || 2+n > len(rest) {
				return nil, false
			}
			if nal := rest[2 : 2+n]; h264IsSlice(nal[0]) && len(nal) > 1 && nal[1]&0x80 != 0 {
				return nal, true
			}
			rest = rest[2+n:]
		}

		return nil, false
	}

	if !H264SliceStart(payload) {
		return nil, false
	}

	at := 1
	if payload[0]&0x1f == h264FUA {
		at = 2
	}

	return payload, len(payload) > at && payload[at]&0x80 != 0
}

func h264IsSlice(header byte) bool {
	t := header & 0x1f

	return t == 1 || t == h264NALTypeIDR
}

// bitWriter writes an H.264 RBSP, most significant bit first.
type bitWriter struct {
	b []byte
	n int // bits written
}

func (w *bitWriter) u(bits int, v uint64) {
	for i := bits - 1; i >= 0; i-- {
		if w.n%8 == 0 {
			w.b = append(w.b, 0)
		}
		if v>>uint(i)&1 == 1 {
			w.b[len(w.b)-1] |= 0x80 >> uint(w.n%8)
		}
		w.n++
	}
}

// ue writes an unsigned exp-Golomb code.
func (w *bitWriter) ue(v uint64) {
	bits := 0
	for (v+1)>>uint(bits) > 1 {
		bits++
	}
	w.u(bits, 0)
	w.u(bits+1, v+1)
}

// trailing ends the RBSP with rbsp_trailing_bits.
func (w *bitWriter) trailing() []byte {
	w.u(1, 1)
	for w.n%8 != 0 {
		w.u(1, 0)
	}

	return w.b
}

// fill ends the last byte with one bits. The bytes after it, zero in a
// replayed payload, then do not read as part of the header.
func (w *bitWriter) fill() []byte {
	for w.n%8 != 0 {
		w.u(1, 1)
	}

	return w.b
}

// h264Escape inserts the emulation-prevention bytes an RBSP needs in a NAL
// unit: a 3 after two zeros that a byte of 3 or less follows.
func h264Escape(rbsp []byte) []byte {
	out := make([]byte, 0, len(rbsp)+len(rbsp)/16)
	zeros := 0
	for _, b := range rbsp {
		if zeros >= 2 && b <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}

	return out
}
