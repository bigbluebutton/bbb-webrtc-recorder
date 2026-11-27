package corpus

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nalTypes lists the NAL unit types an H.264 RTP payload carries.
func nalTypes(t *testing.T, p []byte) []byte {
	t.Helper()

	switch p[0] & 0x1f {
	case 24:
		var types []byte
		for rest := p[1:]; len(rest) > 0; {
			require.GreaterOrEqual(t, len(rest), 2)
			n := int(binary.BigEndian.Uint16(rest))
			require.LessOrEqual(t, 2+n, len(rest))
			types = append(types, rest[2]&0x1f)
			rest = rest[2+n:]
		}

		return types
	case 28:
		return []byte{p[1] & 0x1f}
	default:
		return []byte{p[0] & 0x1f}
	}
}

func TestGenerateH264Layout(t *testing.T) {
	tr, err := GenerateH264(H264Options{
		FPS: 30, Frames: 4, KeyInterval: 3, KeySlicePackets: 3, StartSeq: 65534, StartTS: 0xFFFFFF00, SSRC: 7,
	})
	require.NoError(t, err)
	require.NoError(t, tr.Validate())

	assert.Equal(t, KindVideo, tr.Header.Kind)
	assert.Equal(t, CodecH264, tr.Header.Codec)
	assert.Equal(t, uint32(90000), tr.Header.ClockRate)
	assert.Equal(t, uint32(7), tr.Header.SSRC)

	// Key frame: STAP-A of SPS and PPS, then an IDR in three FU-A fragments.
	// Delta frames: one P slice each. Frame 3 is a key frame again.
	require.Len(t, tr.Records, 4+1+1+4)

	var seqs []uint16
	for _, r := range tr.Records {
		seqs = append(seqs, r.Seq)
	}
	assert.Equal(t, []uint16{65534, 65535, 0, 1, 2, 3, 4, 5, 6, 7}, seqs)

	key := tr.Records[:4]
	assert.Equal(t, []byte{7, 8}, nalTypes(t, key[0].Packet().Payload))
	for i, r := range key[1:] {
		p := r.Packet().Payload
		assert.Equal(t, byte(28), p[0]&0x1f)
		assert.Equal(t, byte(5), p[1]&0x1f)
		assert.Equal(t, i == 0, p[1]&0x80 != 0, "start bit")
		assert.Equal(t, i == 2, p[1]&0x40 != 0, "end bit")
	}
	for i, r := range key {
		assert.Equal(t, uint32(0xFFFFFF00), r.TS)
		assert.Equal(t, i == 3, r.M, "marker on the frame's last packet")
	}

	delta := tr.Records[4]
	assert.Equal(t, []byte{1}, nalTypes(t, delta.Packet().Payload))
	assert.True(t, delta.M)
	start := uint32(0xFFFFFF00)
	assert.Equal(t, start+3000, delta.TS, "across the timestamp wrap")

	assert.True(t, H264KeyFrameStart(key[0].Packet().Payload))
	assert.False(t, H264KeyFrameStart(key[1].Packet().Payload))
	assert.False(t, H264KeyFrameStart(delta.Packet().Payload))
	assert.True(t, H264KeyFrameStart(tr.Records[6].Packet().Payload), "frame 3")
}

func TestGenerateH264Slices(t *testing.T) {
	tr, err := GenerateH264(H264Options{FPS: 15, Frames: 2, Slices: 3, DeltaSlicePackets: 2})
	require.NoError(t, err)

	// Key frame: STAP-A, three single-NAL IDR slices. Delta: three P slices
	// in two FU-A fragments each.
	require.Len(t, tr.Records, 1+3+6)
	for _, r := range tr.Records[1:4] {
		assert.Equal(t, []byte{5}, nalTypes(t, r.Packet().Payload))
	}
	for _, r := range tr.Records[4:] {
		assert.Equal(t, byte(28), r.Packet().Payload[0]&0x1f)
	}

	// first_mb_in_slice ue(v) starts each slice header: 0, then later ones.
	assert.Equal(t, byte(0x80), tr.Records[1].Packet().Payload[1]&0x80, "slice 0 starts at macroblock 0")
	assert.Zero(t, tr.Records[2].Packet().Payload[1]&0x80, "slice 1 does not")
}

func TestGenerateH264PacketizationMode0(t *testing.T) {
	tr, err := GenerateH264(H264Options{FPS: 15, Frames: 2, Slices: 2, Mode0: true})
	require.NoError(t, err)

	// No STAP-A and no FU-A: SPS, PPS and each slice in packets of their own.
	var types []byte
	for _, r := range tr.Records {
		types = append(types, nalTypes(t, r.Packet().Payload)...)
		assert.NotEqual(t, byte(24), r.Packet().Payload[0]&0x1f)
		assert.NotEqual(t, byte(28), r.Packet().Payload[0]&0x1f)
	}
	assert.Equal(t, []byte{7, 8, 5, 5, 1, 1}, types)
	assert.True(t, H264KeyFrameStart(tr.Records[0].Packet().Payload))
}

func TestH264SPSSize(t *testing.T) {
	for _, c := range []struct{ w, h, mbW, mbH, cropR, cropB int }{
		{640, 360, 40, 23, 0, 4},
		{1280, 720, 80, 45, 0, 0},
		{320, 180, 20, 12, 0, 6},
		{2, 2, 1, 1, 7, 7},
	} {
		sps := H264SPS(c.w, c.h)
		assert.Equal(t, byte(0x67), sps[0])
		r := spsFields(t, sps)
		assert.Equal(t, []int{c.mbW, c.mbH, c.cropR, c.cropB}, r, "%dx%d", c.w, c.h)
	}
}

// spsFields reads back the macroblock size and the right and bottom crop of
// an SPS H264SPS wrote.
func spsFields(t *testing.T, nal []byte) []int {
	t.Helper()

	var rbsp []byte
	zeros := 0
	for _, b := range nal[1:] {
		if zeros >= 2 && b == 3 {
			zeros = 0
			continue
		}
		rbsp = append(rbsp, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}

	pos := 24 // profile, constraints, level
	bit := func() int {
		v := int(rbsp[pos/8]>>(7-pos%8)) & 1
		pos++
		return v
	}
	ue := func() int {
		n := 0
		for bit() == 0 {
			n++
		}
		v := 1
		for range n {
			v = v<<1 | bit()
		}
		return v - 1
	}

	ue()  // seq_parameter_set_id
	ue()  // log2_max_frame_num_minus4
	ue()  // pic_order_cnt_type 2
	ue()  // max_num_ref_frames
	bit() // gaps_in_frame_num_value_allowed_flag
	w, h := ue()+1, ue()+1
	require.Equal(t, 1, bit(), "frame_mbs_only_flag")
	bit() // direct_8x8_inference_flag
	var right, bottom int
	if bit() == 1 {
		require.Zero(t, ue())
		right = ue()
		require.Zero(t, ue())
		bottom = ue()
	}

	return []int{w, h, right, bottom}
}

func TestGenerateH264Rejects(t *testing.T) {
	for _, o := range []H264Options{
		{FPS: 30},
		{FPS: 30, Frames: 1, Width: 641},
		{FPS: 30, Frames: 1, Slices: -1},
		{FPS: 30, Frames: 1, KeySlicePackets: -1},
		{FPS: 30, Frames: 1, Mode0: true, KeySlicePackets: 2},
	} {
		_, err := GenerateH264(o)
		assert.Error(t, err, "%+v", o)
	}
}

func TestH264HoldsSlice(t *testing.T) {
	tr, err := GenerateH264(H264Options{FPS: 30, Frames: 2, KeySlicePackets: 3})
	require.NoError(t, err)

	var holds []bool
	for _, r := range tr.Records {
		holds = append(holds, H264HoldsSlice(r.Packet().Payload))
	}
	assert.Equal(t, []bool{false, true, true, true, true}, holds, "every packet but the parameter sets")

	assert.True(t, H264HoldsSlice(h264STAP(H264SPS(2, 2), h264PPS, []byte{h264HeaderIDR, 0x88})), "a blank frame's STAP-A")
	assert.False(t, H264HoldsSlice([]byte{h264STAPA, 0x00}), "a truncated STAP-A")
	assert.False(t, H264HoldsSlice([]byte{h264STAPA, 0x00, 0x09, h264HeaderIDR}), "a STAP-A size past the payload")
	assert.False(t, H264HoldsSlice([]byte{h264FUA}), "an FU-A without its header")
	assert.False(t, H264HoldsSlice(nil))
}
