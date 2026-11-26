package recorder

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	h264NALTypeMask = 0x1f
	h264NALSPS      = 7
	h264NALPPS      = 8
)

var errH264ShortRead = errors.New("read past the end")

// h264SPS holds the SPS fields the recorder needs. width and height are the
// cropped display size.
type h264SPS struct {
	profileIDC, constraints, levelIDC             uint8
	chromaFormatIDC, bitDepthLuma, bitDepthChroma int
	width, height                                 int
}

// h264HighProfile reports whether an SPS with this profile_idc codes
// chroma_format_idc and bit depths (H.264 7.3.2.1.1). avcC adds the same
// fields for these profiles.
func h264HighProfile(profileIDC uint8) bool {
	switch profileIDC {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		return true
	}
	return false
}

// h264BitReader reads an RBSP. The first failed read sets err; later reads
// return 0, so callers check err after a group of reads and in every loop.
type h264BitReader struct {
	b   []byte
	pos int
	err error
}

func (r *h264BitReader) bit() uint32 {
	if r.err != nil {
		return 0
	}
	if r.pos >= len(r.b)*8 {
		r.err = errH264ShortRead
		return 0
	}
	v := uint32(r.b[r.pos/8]>>(7-uint(r.pos%8))) & 1
	r.pos++
	return v
}

func (r *h264BitReader) bits(n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		v = v<<1 | r.bit()
	}
	return v
}

// ue reads an unsigned exp-Golomb code. The syntax elements that use it fit
// in 32 bits, so a longer code is an error.
func (r *h264BitReader) ue() uint32 {
	zeros := 0
	for r.bit() == 0 {
		if r.err != nil {
			return 0
		}
		zeros++
		if zeros > 31 {
			r.err = errors.New("exp-Golomb code with more than 31 leading zeros")
			return 0
		}
	}
	return uint32(1)<<uint(zeros) - 1 + r.bits(zeros)
}

func (r *h264BitReader) se() int64 {
	k := int64(r.ue())
	if k&1 == 1 {
		return (k + 1) / 2
	}
	return -k / 2
}

// skipScalingList reads scaling_list() (H.264 7.3.2.1.1.1). A nextScale of 0
// ends the coded deltas. Go's % keeps the sign of a malformed large negative
// delta, but the result is still 0 exactly when the spec's is.
func (r *h264BitReader) skipScalingList(size int) {
	last, next := int64(8), int64(8)
	for j := 0; j < size && r.err == nil; j++ {
		if next != 0 {
			next = (last + r.se() + 256) % 256
		}
		if next != 0 {
			last = next
		}
	}
}

// h264RBSP removes the emulation-prevention bytes (00 00 03) from a NAL unit
// payload.
func h264RBSP(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		out = append(out, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// parseH264SPS parses an SPS NAL unit, header byte included, up to the frame
// cropping fields.
func parseH264SPS(nal []byte) (h264SPS, error) {
	if len(nal) < 4 {
		return h264SPS{}, fmt.Errorf("SPS of %d bytes is too short", len(nal))
	}
	if nal[0]&0x80 != 0 {
		return h264SPS{}, errors.New("SPS has forbidden_zero_bit set")
	}
	if t := nal[0] & h264NALTypeMask; t != h264NALSPS {
		return h264SPS{}, fmt.Errorf("NAL unit type %d is not an SPS", t)
	}

	r := &h264BitReader{b: h264RBSP(nal[1:])}
	s := h264SPS{
		profileIDC:      uint8(r.bits(8)),
		constraints:     uint8(r.bits(8)),
		levelIDC:        uint8(r.bits(8)),
		chromaFormatIDC: 1,
		bitDepthLuma:    8,
		bitDepthChroma:  8,
	}
	r.ue() // seq_parameter_set_id

	separateColourPlanes := false
	if h264HighProfile(s.profileIDC) {
		chroma := r.ue()
		if r.err == nil && chroma > 3 {
			return h264SPS{}, fmt.Errorf("SPS chroma_format_idc %d", chroma)
		}
		s.chromaFormatIDC = int(chroma)
		if chroma == 3 {
			separateColourPlanes = r.bit() == 1
		}
		s.bitDepthLuma = 8 + int(r.ue())
		s.bitDepthChroma = 8 + int(r.ue())
		r.bit() // qpprime_y_zero_transform_bypass_flag
		if r.bit() == 1 {
			lists := 8
			if chroma == 3 {
				lists = 12
			}
			for i := 0; i < lists && r.err == nil; i++ {
				if r.bit() == 0 {
					continue
				}
				if i < 6 {
					r.skipScalingList(16)
				} else {
					r.skipScalingList(64)
				}
			}
		}
	}

	r.ue() // log2_max_frame_num_minus4
	switch pocType := r.ue(); pocType {
	case 0:
		r.ue() // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		r.bit() // delta_pic_order_always_zero_flag
		r.se()  // offset_for_non_ref_pic
		r.se()  // offset_for_top_to_bottom_field
		cycle := r.ue()
		for i := uint32(0); i < cycle && r.err == nil; i++ {
			r.se()
		}
	case 2:
	default:
		if r.err == nil {
			return h264SPS{}, fmt.Errorf("SPS pic_order_cnt_type %d", pocType)
		}
	}
	r.ue()  // max_num_ref_frames
	r.bit() // gaps_in_frame_num_value_allowed_flag
	widthMBs := int64(r.ue()) + 1
	heightMapUnits := int64(r.ue()) + 1
	frameMBsOnly := int64(r.bit())
	if frameMBsOnly == 0 {
		r.bit() // mb_adaptive_frame_field_flag
	}
	r.bit() // direct_8x8_inference_flag
	var cropLeft, cropRight, cropTop, cropBottom int64
	if r.bit() == 1 {
		cropLeft = int64(r.ue())
		cropRight = int64(r.ue())
		cropTop = int64(r.ue())
		cropBottom = int64(r.ue())
	}
	if r.err != nil {
		return h264SPS{}, fmt.Errorf("SPS: %w", r.err)
	}

	// Crop offsets count chroma samples, and field pairs when frames are coded
	// as fields (H.264 7.4.2.1.1, frame_crop_left_offset).
	cropUnitX, cropUnitY := int64(1), 2-frameMBsOnly
	if !separateColourPlanes && s.chromaFormatIDC != 0 {
		subWidth, subHeight := int64(2), int64(2)
		switch s.chromaFormatIDC {
		case 2:
			subHeight = 1
		case 3:
			subWidth, subHeight = 1, 1
		}
		cropUnitX = subWidth
		cropUnitY = subHeight * (2 - frameMBsOnly)
	}
	width := widthMBs*16 - cropUnitX*(cropLeft+cropRight)
	height := (2-frameMBsOnly)*heightMapUnits*16 - cropUnitY*(cropTop+cropBottom)
	if width <= 0 || height <= 0 {
		return h264SPS{}, fmt.Errorf("SPS cropping leaves %dx%d", width, height)
	}
	if int64(int(width)) != width || int64(int(height)) != height {
		return h264SPS{}, fmt.Errorf("SPS size %dx%d does not fit int", width, height)
	}
	s.width, s.height = int(width), int(height)
	return s, nil
}

// h264CodecPrivate builds the avcC record (AVCDecoderConfigurationRecord,
// ISO/IEC 14496-15 5.3.3.1) that Matroska's V_MPEG4/ISO/AVC CodecPrivate holds.
// Blocks then carry 4-byte NAL unit lengths.
func h264CodecPrivate(sps, pps [][]byte) ([]byte, error) {
	if len(sps) == 0 || len(pps) == 0 {
		return nil, fmt.Errorf("avcC needs an SPS and a PPS, got %d and %d", len(sps), len(pps))
	}
	// numOfSequenceParameterSets has 5 bits.
	if len(sps) > 31 || len(pps) > 255 {
		return nil, fmt.Errorf("avcC holds at most 31 SPS and 255 PPS, got %d and %d", len(sps), len(pps))
	}
	if len(sps[0]) < 4 {
		return nil, fmt.Errorf("SPS of %d bytes is too short", len(sps[0]))
	}

	size := 7
	for _, list := range []struct {
		nals [][]byte
		typ  byte
	}{{sps, h264NALSPS}, {pps, h264NALPPS}} {
		for _, nal := range list.nals {
			if len(nal) == 0 || len(nal) > 0xffff {
				return nil, fmt.Errorf("parameter set of %d bytes does not fit avcC", len(nal))
			}
			if t := nal[0] & h264NALTypeMask; t != list.typ {
				return nil, fmt.Errorf("NAL unit type %d in the type %d list", t, list.typ)
			}
			size += 2 + len(nal)
		}
	}

	var tail []byte
	if h264HighProfile(sps[0][1]) {
		s, err := parseH264SPS(sps[0])
		if err != nil {
			return nil, err
		}
		if s.bitDepthLuma-8 > 7 || s.bitDepthChroma-8 > 7 {
			return nil, fmt.Errorf("SPS bit depths %d/%d do not fit avcC", s.bitDepthLuma, s.bitDepthChroma)
		}
		// chroma_format, bit_depth_luma_minus8, bit_depth_chroma_minus8 with
		// their reserved 1 bits, then no SPS extensions.
		tail = []byte{
			0xfc | byte(s.chromaFormatIDC),
			0xf8 | byte(s.bitDepthLuma-8),
			0xf8 | byte(s.bitDepthChroma-8),
			0,
		}
	}

	cp := make([]byte, 0, size+len(tail))
	// 0xff: reserved bits and lengthSizeMinusOne 3. 0xe0: reserved bits.
	cp = append(cp, 1, sps[0][1], sps[0][2], sps[0][3], 0xff, 0xe0|byte(len(sps)))
	for _, nal := range sps {
		cp = binary.BigEndian.AppendUint16(cp, uint16(len(nal)))
		cp = append(cp, nal...)
	}
	cp = append(cp, byte(len(pps)))
	for _, nal := range pps {
		cp = binary.BigEndian.AppendUint16(cp, uint16(len(nal)))
		cp = append(cp, nal...)
	}
	return append(cp, tail...), nil
}
