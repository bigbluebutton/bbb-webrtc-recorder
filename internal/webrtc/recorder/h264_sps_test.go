package recorder

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPS and PPS from libx264 (ffmpeg 4.2.7), all made by
//
//	ffmpeg -f lavfi -i testsrc=size=<size>:rate=15 -frames:v 2 -c:v libx264 \
//	  -profile:v <profile> -pix_fmt <pix_fmt> -x264-params keyint=15<params> -f h264 out.h264
//
// with the arguments given on each vector. The sizes were checked with ffprobe.
//
// libx264 writes scaling lists into the PPS only. The "lists in SPS" vectors
// are the same streams, with the PPS lists moved into the SPS and the PPS
// flag cleared. ffmpeg decodes both streams to the same framemd5. Their lists:
//
//	cqm4=10,12,14,16,12,14,16,18,14,16,18,20,16,18,20,22
//	cqm8=row r (0-7) holds r+9 .. r+16
var h264TestSPS = []struct {
	name        string
	nal         string
	profile     uint8
	constraints uint8
	level       uint8
	chroma      int
	depth       int
	width       int
	height      int
	pps         string
}{
	{
		// 640x360, baseline, yuv420p
		name: "baseline 640x360", nal: "6742c016d900a02ff970110000030001000003001e0f162e48",
		profile: 66, constraints: 0xc0, level: 22, chroma: 1, depth: 8, width: 640, height: 360,
		pps: "68cb83cb20",
	},
	{
		// 1920x1080, main, yuv420p
		name: "main 1920x1080", nal: "674d4028eca03c0113f2e0220000030002000003003c1e30632c",
		profile: 77, constraints: 0x40, level: 40, chroma: 1, depth: 8, width: 1920, height: 1080,
		pps: "68ebe3cb20",
	},
	{
		// 480x270, high, yuv420p
		name: "high 480x270", nal: "67640015acd941e08feb0110000003001000000301e0f162d960",
		profile: 100, level: 21, chroma: 1, depth: 8, width: 480, height: 270,
		pps: "68ebe3cb22c0",
	},
	{
		// 320x180, high, yuv420p
		name: "high 320x180", nal: "6764000cacd941419f9f0110000003001000000301e0f1429960",
		profile: 100, level: 12, chroma: 1, depth: 8, width: 320, height: 180,
		pps: "68ebe3cb22c0",
	},
	{
		// 640x360, high, yuv420p, cqm=jvt (the JVT lists are the defaults, so no list is coded)
		name: "high cqm=jvt", nal: "67640016acd940a02ff970110000030001000003001e0f162d96",
		profile: 100, level: 22, chroma: 1, depth: 8, width: 640, height: 360,
		pps: "68ebe3cb3002c0",
	},
	{
		// 640x360, high, yuv420p, cqm4/cqm8 above, lists in SPS (8 lists, 4x4 and 8x8)
		name: "high lists in SPS",
		nal: "67640016ad909264e4c9212124c9c99242956baf5f5fafebf5f5ebad552ad75ebebf5fd7ebebd75a" +
			"ad940a02ff970110000003001000000301e0f162d960",
		profile: 100, level: 22, chroma: 1, depth: 8, width: 640, height: 360,
		pps: "68ebe3cb22c0",
	},
	{
		// 640x360, high444, yuv444p
		name: "high 4:4:4", nal: "67f40016919b281405ff138088000003000800000300f078b16cb0",
		profile: 244, level: 22, chroma: 3, depth: 8, width: 640, height: 360,
		pps: "68ebe3c448",
	},
	{
		// 640x360, high444, yuv444p, 8x8dct=1 and cqm4/cqm8 above, lists in SPS
		// (12 list flags; x264 keeps the 8x8 transform off in 4:4:4, so only 4x4 lists are coded)
		name:    "high 4:4:4 lists in SPS",
		nal:     "67f4001691b2124c9c992424249939324801b281405ff1380880000003008000000f078b16cb",
		profile: 244, level: 22, chroma: 3, depth: 8, width: 640, height: 360,
		pps: "68ebe3c44044",
	},
	{
		// 640x360, high422, yuv422p
		name: "high 4:2:2", nal: "677a0016bcd940a02ff89c0440000003004000000783c58b6580",
		profile: 122, level: 22, chroma: 2, depth: 8, width: 640, height: 360,
		pps: "68ebe3cb22c0",
	},
	{
		// 640x360, high, yuv420p, interlaced=1 (frame_mbs_only_flag 0, coded 640x384)
		name: "high interlaced", nal: "67640016acd940a063f3e0220000030002000003003c3e28532c",
		profile: 100, level: 22, chroma: 1, depth: 8, width: 640, height: 360,
		pps: "68fba3cb22c0",
	},
	{
		// 320x180, high10, yuv420p10le
		name: "high10 320x180", nal: "676e000ca6cd941419f9f0110000030001000003001e0f142996",
		profile: 110, level: 12, chroma: 1, depth: 10, width: 320, height: 180,
		pps: "68ebe3cb22c0",
	},
	{
		// Not from x264: one 4x4 list in the SPS. ffmpeg's trace_headers and
		// GStreamer's h264parse both read it as 640x360.
		name: "one 4x4 list", nal: "6764001fadffff80b405017fca80",
		profile: 100, level: 31, chroma: 1, depth: 8, width: 640, height: 360,
		pps: "68ebe3cb22c0",
	},
	{
		// LiveKit's blank key frame SPS (pkg/sfu/downtrack.go H264KeyFrame2x2SPS).
		name: "livekit blank 2x2", nal: "6742c01f0fd91f888884000003000400000300c83c60c920",
		profile: 66, constraints: 0xc0, level: 31, chroma: 1, depth: 8, width: 2, height: 2,
		pps: "6887cb83cb20",
	},
}

func mustHex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

func TestParseH264SPSRealStreams(t *testing.T) {
	for _, v := range h264TestSPS {
		t.Run(v.name, func(t *testing.T) {
			sps, err := parseH264SPS(mustHex(t, v.nal))
			require.NoError(t, err)
			assert.Equal(t, h264SPS{
				profileIDC:      v.profile,
				constraints:     v.constraints,
				levelIDC:        v.level,
				chromaFormatIDC: v.chroma,
				bitDepthLuma:    v.depth,
				bitDepthChroma:  v.depth,
				width:           v.width,
				height:          v.height,
			}, sps)
		})
	}
}

// h264BitWriter writes the bits of a synthetic RBSP.
type h264BitWriter struct {
	bits []byte
}

func (w *h264BitWriter) u(n int, v uint64) {
	for i := n - 1; i >= 0; i-- {
		w.bits = append(w.bits, byte(v>>uint(i))&1)
	}
}

func (w *h264BitWriter) flag(b bool) {
	if b {
		w.u(1, 1)
	} else {
		w.u(1, 0)
	}
}

func (w *h264BitWriter) ue(v uint64) {
	n := 0
	for (v+1)>>uint(n) > 1 {
		n++
	}
	w.u(n, 0)
	w.u(n+1, v+1)
}

func (w *h264BitWriter) se(v int64) {
	if v > 0 {
		w.ue(uint64(2*v - 1))
	} else {
		w.ue(uint64(-2 * v))
	}
}

// rbsp returns the bits with rbsp_stop_one_bit and the alignment zeros.
func (w *h264BitWriter) rbsp() []byte {
	bits := append(append([]byte{}, w.bits...), 1)
	for len(bits)%8 != 0 {
		bits = append(bits, 0)
	}
	out := make([]byte, len(bits)/8)
	for i, b := range bits {
		out[i/8] |= b << (7 - uint(i%8))
	}
	return out
}

// h264Escape inserts emulation-prevention bytes into an RBSP.
func h264Escape(rbsp []byte) []byte {
	out := make([]byte, 0, len(rbsp)+4)
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

// testSPS describes a synthetic SPS. scaling holds the coded delta_scale
// values of each list, nil when the list is not present.
type testSPS struct {
	profile         uint8
	constraints     uint8
	level           uint8
	chromaFormat    uint64
	separatePlanes  bool
	lumaMinus8      uint64
	chromaMinus8    uint64
	scaling         [][]int64
	pocType         uint64
	pocCycle        []int64
	widthMBsMinus1  uint64
	heightMUsMinus1 uint64
	fieldCoding     bool
	crop            []uint64
	// bitsRead is set by write: the bits parseH264SPS needs.
	bitsRead int
}

func (s *testSPS) write() *h264BitWriter {
	w := &h264BitWriter{}
	w.u(8, uint64(s.profile))
	w.u(8, uint64(s.constraints))
	w.u(8, uint64(s.level))
	w.ue(0)
	if h264HighProfile(s.profile) {
		w.ue(s.chromaFormat)
		if s.chromaFormat == 3 {
			w.flag(s.separatePlanes)
		}
		w.ue(s.lumaMinus8)
		w.ue(s.chromaMinus8)
		w.flag(false)
		w.flag(s.scaling != nil)
		for _, list := range s.scaling {
			w.flag(list != nil)
			for _, d := range list {
				w.se(d)
			}
		}
	}
	w.ue(0)
	w.ue(s.pocType)
	switch s.pocType {
	case 0:
		w.ue(2)
	case 1:
		w.flag(false)
		w.se(-3)
		w.se(5)
		w.ue(uint64(len(s.pocCycle)))
		for _, o := range s.pocCycle {
			w.se(o)
		}
	}
	w.ue(1)
	w.flag(false)
	w.ue(s.widthMBsMinus1)
	w.ue(s.heightMUsMinus1)
	w.flag(!s.fieldCoding)
	if s.fieldCoding {
		w.flag(true)
	}
	w.flag(true)
	w.flag(s.crop != nil)
	for _, c := range s.crop {
		w.ue(c)
	}
	s.bitsRead = len(w.bits)
	w.flag(false) // vui_parameters_present_flag
	return w
}

func (s *testSPS) nal() []byte {
	return append([]byte{0x67}, h264Escape(s.write().rbsp())...)
}

// flatList codes a list of 16 or 64 entries that all equal 8 + delta.
func flatList(size int, delta int64) []int64 {
	list := make([]int64, size)
	list[0] = delta
	return list
}

func TestParseH264SPSSynthetic(t *testing.T) {
	tests := []struct {
		name          string
		sps           testSPS
		chroma        int
		luma, chromaD int
		width, height int
	}{
		{
			name:   "baseline, no crop",
			sps:    testSPS{profile: 66, widthMBsMinus1: 19, heightMUsMinus1: 14},
			chroma: 1, luma: 8, chromaD: 8, width: 320, height: 240,
		},
		{
			name:   "monochrome crops in luma samples",
			sps:    testSPS{profile: 100, chromaFormat: 0, widthMBsMinus1: 39, heightMUsMinus1: 22, crop: []uint64{1, 2, 3, 5}},
			chroma: 0, luma: 8, chromaD: 8, width: 640 - 3, height: 368 - 8,
		},
		{
			name:   "monochrome field coding doubles the vertical crop unit",
			sps:    testSPS{profile: 100, chromaFormat: 0, widthMBsMinus1: 39, heightMUsMinus1: 11, fieldCoding: true, crop: []uint64{0, 0, 0, 12}},
			chroma: 0, luma: 8, chromaD: 8, width: 640, height: 384 - 24,
		},
		{
			name:   "4:2:0 crop unit 2x2",
			sps:    testSPS{profile: 100, chromaFormat: 1, widthMBsMinus1: 39, heightMUsMinus1: 22, crop: []uint64{1, 1, 1, 3}},
			chroma: 1, luma: 8, chromaD: 8, width: 640 - 4, height: 368 - 8,
		},
		{
			name:   "4:2:0 field coding crop unit 2x4",
			sps:    testSPS{profile: 100, chromaFormat: 1, widthMBsMinus1: 39, heightMUsMinus1: 11, fieldCoding: true, crop: []uint64{0, 1, 0, 6}},
			chroma: 1, luma: 8, chromaD: 8, width: 640 - 2, height: 384 - 24,
		},
		{
			name:   "4:2:2 crop unit 2x1",
			sps:    testSPS{profile: 122, chromaFormat: 2, widthMBsMinus1: 39, heightMUsMinus1: 22, crop: []uint64{1, 1, 0, 8}},
			chroma: 2, luma: 8, chromaD: 8, width: 640 - 4, height: 368 - 8,
		},
		{
			name:   "4:4:4 crop unit 1x1",
			sps:    testSPS{profile: 244, chromaFormat: 3, widthMBsMinus1: 39, heightMUsMinus1: 22, crop: []uint64{1, 2, 0, 8}},
			chroma: 3, luma: 8, chromaD: 8, width: 640 - 3, height: 368 - 8,
		},
		{
			name: "separate colour planes crop in luma samples",
			sps: testSPS{
				profile: 244, chromaFormat: 3, separatePlanes: true,
				widthMBsMinus1: 39, heightMUsMinus1: 11, fieldCoding: true, crop: []uint64{1, 2, 0, 12},
			},
			chroma: 3, luma: 8, chromaD: 8, width: 640 - 3, height: 384 - 24,
		},
		{
			name:   "bit depths",
			sps:    testSPS{profile: 110, chromaFormat: 1, lumaMinus8: 2, chromaMinus8: 4, widthMBsMinus1: 0, heightMUsMinus1: 0},
			chroma: 1, luma: 10, chromaD: 12, width: 16, height: 16,
		},
		{
			name:   "pic_order_cnt_type 1 with a cycle",
			sps:    testSPS{profile: 77, pocType: 1, pocCycle: []int64{1, -2, 3}, widthMBsMinus1: 79, heightMUsMinus1: 44},
			chroma: 1, luma: 8, chromaD: 8, width: 1280, height: 720,
		},
		{
			name:   "pic_order_cnt_type 2",
			sps:    testSPS{profile: 66, pocType: 2, widthMBsMinus1: 79, heightMUsMinus1: 44},
			chroma: 1, luma: 8, chromaD: 8, width: 1280, height: 720,
		},
		{
			name: "8 scaling lists, all present",
			sps: testSPS{
				profile: 100, chromaFormat: 1, widthMBsMinus1: 39, heightMUsMinus1: 22, crop: []uint64{0, 0, 0, 4},
				scaling: [][]int64{
					flatList(16, 1), flatList(16, 2), flatList(16, -1), flatList(16, 3), flatList(16, 4), flatList(16, 5),
					flatList(64, 6), flatList(64, -7),
				},
			},
			chroma: 1, luma: 8, chromaD: 8, width: 640, height: 360,
		},
		{
			// delta -8 at j 0 sets nextScale 0 (use the default list): no further deltas follow.
			name: "useDefaultScalingMatrixFlag stops the list",
			sps: testSPS{
				profile: 100, chromaFormat: 1, widthMBsMinus1: 39, heightMUsMinus1: 22, crop: []uint64{0, 0, 0, 4},
				scaling: [][]int64{{-8}, nil, nil, {-8}, nil, nil, {-8}, {-8}},
			},
			chroma: 1, luma: 8, chromaD: 8, width: 640, height: 360,
		},
		{
			// nextScale wraps modulo 256 to 0 (8+120+10+118, and 8-9+1): the rest of each list repeats lastScale.
			name: "lists that end early after a modulo wrap",
			sps: testSPS{
				profile: 100, chromaFormat: 1, widthMBsMinus1: 39, heightMUsMinus1: 22, crop: []uint64{0, 0, 0, 4},
				scaling: [][]int64{{120, 10, 118}, nil, nil, nil, nil, nil, nil, {-9, 1}},
			},
			chroma: 1, luma: 8, chromaD: 8, width: 640, height: 360,
		},
		{
			name: "12 scaling lists in 4:4:4",
			sps: testSPS{
				profile: 244, chromaFormat: 3, widthMBsMinus1: 39, heightMUsMinus1: 22, crop: []uint64{0, 0, 0, 8},
				scaling: [][]int64{
					flatList(16, 1), nil, nil, flatList(16, 3), nil, nil,
					flatList(64, 6), flatList(64, -7), nil, flatList(64, 2), nil, flatList(64, 1),
				},
			},
			chroma: 3, luma: 8, chromaD: 8, width: 640, height: 360,
		},
		{
			name:   "largest exp-Golomb values",
			sps:    testSPS{profile: 66, widthMBsMinus1: 1<<32 - 2, heightMUsMinus1: 1<<32 - 2, crop: []uint64{1<<32 - 2, 0, 0, 0}},
			chroma: 1, luma: 8, chromaD: 8, width: (1<<32-1)*16 - 2*(1<<32-2), height: (1<<32 - 1) * 16,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sps, err := parseH264SPS(tt.sps.nal())
			require.NoError(t, err)
			assert.Equal(t, tt.sps.profile, sps.profileIDC)
			assert.Equal(t, tt.chroma, sps.chromaFormatIDC)
			assert.Equal(t, tt.luma, sps.bitDepthLuma)
			assert.Equal(t, tt.chromaD, sps.bitDepthChroma)
			assert.Equal(t, tt.width, sps.width)
			assert.Equal(t, tt.height, sps.height)
		})
	}
}

func TestParseH264SPSRemovesEmulationPrevention(t *testing.T) {
	// pic_width_in_mbs_minus1 = 2^20-1 codes as 20 zero bits, so the RBSP holds 00 00 0x.
	s := testSPS{profile: 66, widthMBsMinus1: 1<<20 - 1, heightMUsMinus1: 0}
	nal := s.nal()
	require.True(t, bytes.Contains(nal, []byte{0, 0, 3}), "%x", nal)

	sps, err := parseH264SPS(nal)
	require.NoError(t, err)
	assert.Equal(t, 1<<20*16, sps.width)
	assert.Equal(t, 16, sps.height)
}

func TestParseH264SPSFailsOnTruncation(t *testing.T) {
	t.Run("synthetic, exact bit count", func(t *testing.T) {
		for _, s := range []testSPS{
			{profile: 66, level: 30, widthMBsMinus1: 39, heightMUsMinus1: 22, crop: []uint64{0, 0, 0, 4}},
			{profile: 100, level: 30, chromaFormat: 1, widthMBsMinus1: 39, heightMUsMinus1: 22, scaling: [][]int64{flatList(16, 1), nil, nil, nil, nil, nil, flatList(64, 2), nil}},
			{profile: 244, level: 30, chromaFormat: 3, separatePlanes: true, widthMBsMinus1: 39, heightMUsMinus1: 11, fieldCoding: true, crop: []uint64{0, 0, 0, 12}},
			{profile: 77, level: 30, pocType: 1, pocCycle: []int64{1, 2}, widthMBsMinus1: 79, heightMUsMinus1: 44},
		} {
			rbsp := s.write().rbsp()
			require.NotContains(t, string(rbsp), "\x00\x00", "test SPS must not need escaping")
			need := (s.bitsRead + 7) / 8

			full, err := parseH264SPS(append([]byte{0x67}, rbsp...))
			require.NoError(t, err)
			cut, err := parseH264SPS(append([]byte{0x67}, rbsp[:need]...))
			require.NoError(t, err, "the bits read fit in %d bytes", need)
			assert.Equal(t, full, cut)

			_, err = parseH264SPS(append([]byte{0x67}, rbsp[:need-1]...))
			assert.Error(t, err, "SPS %+v cut to %d bytes", s, need-1)
		}
	})

	t.Run("real streams, every prefix", func(t *testing.T) {
		for _, v := range h264TestSPS {
			nal := mustHex(t, v.nal)
			full, err := parseH264SPS(nal)
			require.NoError(t, err)
			ok := false
			for n := 0; n <= len(nal); n++ {
				sps, err := parseH264SPS(nal[:n])
				if err != nil {
					assert.False(t, ok, "%s: prefix %d fails after a shorter one parsed", v.name, n)
					continue
				}
				ok = true
				assert.Equal(t, full, sps, "%s: prefix %d", v.name, n)
			}
		}
	})
}

func TestParseH264SPSRejects(t *testing.T) {
	valid := mustHex(t, h264TestSPS[0].nal)
	withHeader := func(h byte) []byte { return append([]byte{h}, valid[1:]...) }

	// A valid baseline SPS but for a 65-bit seq_parameter_set_id (32 leading zeros).
	tooLong := &h264BitWriter{}
	tooLong.u(24, 0x42c01e)
	tooLong.u(33, 1)
	tooLong.u(32, 0)
	tooLong.ue(0)
	tooLong.ue(2)
	tooLong.ue(1)
	tooLong.flag(false)
	tooLong.ue(19)
	tooLong.ue(14)
	tooLong.flag(true)
	tooLong.flag(true)
	tooLong.flag(false)
	tooLong.flag(false)
	longCode := append([]byte{0x67}, h264Escape(tooLong.rbsp())...)

	badChroma := testSPS{profile: 100, chromaFormat: 4, widthMBsMinus1: 1, heightMUsMinus1: 1}
	badPOC := testSPS{profile: 66, pocType: 3, widthMBsMinus1: 1, heightMUsMinus1: 1}
	cropWidth := testSPS{profile: 66, widthMBsMinus1: 0, heightMUsMinus1: 0, crop: []uint64{5, 4, 0, 0}}
	cropHeight := testSPS{profile: 66, widthMBsMinus1: 0, heightMUsMinus1: 0, crop: []uint64{0, 0, 9, 0}}
	cropAll := testSPS{profile: 66, widthMBsMinus1: 0, heightMUsMinus1: 0, crop: []uint64{8, 0, 0, 0}}

	for name, nal := range map[string][]byte{
		"empty":                    nil,
		"header only":              {0x67},
		"no level":                 {0x67, 0x42, 0xc0},
		"PPS":                      withHeader(0x68),
		"IDR":                      withHeader(0x65),
		"forbidden_zero_bit":       withHeader(0xe7),
		"65-bit exp-Golomb":        longCode,
		"chroma_format_idc 4":      badChroma.nal(),
		"pic_order_cnt_type 3":     badPOC.nal(),
		"crop wider than frame":    cropWidth.nal(),
		"crop taller than frame":   cropHeight.nal(),
		"crop leaves zero columns": cropAll.nal(),
	} {
		_, err := parseH264SPS(nal)
		assert.Error(t, err, name)
	}

	_, err := parseH264SPS(longCode)
	assert.ErrorContains(t, err, "exp-Golomb code with more than 31 leading zeros")
}

func TestH264CodecPrivate(t *testing.T) {
	// Expected bytes: ffmpeg's Matroska muxer and GStreamer's h264parse build the
	// same record for these streams, without the High-profile tail.
	tests := []struct {
		name   string
		sps    string
		pps    string
		ffmpeg string
		tail   string
	}{
		{
			name: "baseline", sps: h264TestSPS[0].nal, pps: h264TestSPS[0].pps,
			ffmpeg: "0142c016ffe100196742c016d900a02ff970110000030001000003001e0f162e4801000568cb83cb20",
		},
		{
			name: "main", sps: h264TestSPS[1].nal, pps: h264TestSPS[1].pps,
			ffmpeg: "014d4028ffe1001a674d4028eca03c0113f2e0220000030002000003003c1e30632c01000568ebe3cb20",
		},
		{
			name: "high", sps: h264TestSPS[2].nal, pps: h264TestSPS[2].pps,
			ffmpeg: "01640015ffe1001a67640015acd941e08feb0110000003001000000301e0f162d96001000668ebe3cb22c0",
			tail:   "fdf8f800",
		},
		{
			name: "high10", sps: h264TestSPS[10].nal, pps: h264TestSPS[10].pps,
			ffmpeg: "016e000cffe1001a676e000ca6cd941419f9f0110000030001000003001e0f14299601000668ebe3cb22c0",
			tail:   "fdfafa00",
		},
		{
			name: "high 4:2:2", sps: h264TestSPS[8].nal, pps: h264TestSPS[8].pps,
			ffmpeg: "017a0016ffe1001a677a0016bcd940a02ff89c0440000003004000000783c58b658001000668ebe3cb22c0",
			tail:   "fef8f800",
		},
		{
			name: "high 4:4:4", sps: h264TestSPS[6].nal, pps: h264TestSPS[6].pps,
			ffmpeg: "01f40016ffe1001b67f40016919b281405ff138088000003000800000300f078b16cb001000568ebe3c448",
			tail:   "fff8f800",
		},
		{
			name: "high 4:4:4 lists in SPS", sps: h264TestSPS[7].nal, pps: h264TestSPS[7].pps,
			ffmpeg: "01f40016ffe1002667f4001691b2124c9c992424249939324801b281405ff1380880000003008000000f078b16cb01000668ebe3c44044",
			tail:   "fff8f800",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp, err := h264CodecPrivate([][]byte{mustHex(t, tt.sps)}, [][]byte{mustHex(t, tt.pps)})
			require.NoError(t, err)
			assert.Equal(t, tt.ffmpeg+tt.tail, hex.EncodeToString(cp))
		})
	}
}

func TestH264CodecPrivateSeveralParameterSets(t *testing.T) {
	sps1, sps2 := mustHex(t, h264TestSPS[0].nal), mustHex(t, h264TestSPS[3].nal)
	pps1, pps2, pps3 := []byte{0x68, 0xce}, []byte{0x68, 0xcb, 0x83}, []byte{0x68, 0x53, 0x9c, 0x80}

	cp, err := h264CodecPrivate([][]byte{sps1, sps2}, [][]byte{pps1, pps2, pps3})
	require.NoError(t, err)

	want := []byte{1, 0x42, 0xc0, 0x16, 0xff, 0xe2}
	for _, s := range [][]byte{sps1, sps2} {
		want = append(want, byte(len(s)>>8), byte(len(s)))
		want = append(want, s...)
	}
	want = append(want, 3)
	for _, p := range [][]byte{pps1, pps2, pps3} {
		want = append(want, byte(len(p)>>8), byte(len(p)))
		want = append(want, p...)
	}
	// The first SPS is baseline, so no High-profile tail follows.
	assert.Equal(t, want, cp)
}

func TestH264CodecPrivateHighProfileTail(t *testing.T) {
	pps := [][]byte{mustHex(t, "68ebe3cb22c0")}
	for profile := 0; profile < 256; profile++ {
		s := testSPS{profile: uint8(profile), chromaFormat: 2, lumaMinus8: 1, chromaMinus8: 3, widthMBsMinus1: 1, heightMUsMinus1: 1}
		cp, err := h264CodecPrivate([][]byte{s.nal()}, pps)
		require.NoError(t, err, "profile %d", profile)

		base := 6 + 2 + len(s.nal()) + 1 + 2 + len(pps[0])
		switch profile {
		case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
			require.Len(t, cp, base+4, "profile %d", profile)
			assert.Equal(t, []byte{0xfe, 0xf9, 0xfb, 0x00}, cp[base:], "profile %d", profile)
		default:
			assert.Len(t, cp, base, "profile %d", profile)
		}
	}
}

func TestH264CodecPrivateRejects(t *testing.T) {
	sps := mustHex(t, h264TestSPS[0].nal)
	high := mustHex(t, h264TestSPS[2].nal)
	pps := mustHex(t, h264TestSPS[0].pps)
	many := func(n int, nal []byte) [][]byte {
		out := make([][]byte, n)
		for i := range out {
			out[i] = nal
		}
		return out
	}
	huge := append([]byte{0x68}, make([]byte, 0x10000)...)

	tests := []struct {
		name     string
		sps, pps [][]byte
	}{
		{"no SPS", nil, [][]byte{pps}},
		{"no PPS", [][]byte{sps}, nil},
		{"empty SPS", [][]byte{{}}, [][]byte{pps}},
		{"short SPS", [][]byte{{0x67, 0x42, 0xc0}}, [][]byte{pps}},
		{"empty PPS", [][]byte{sps}, [][]byte{{}}},
		{"SPS is a PPS", [][]byte{pps}, [][]byte{pps}},
		{"PPS is an SPS", [][]byte{sps}, [][]byte{sps}},
		{"second SPS is a PPS", [][]byte{sps, pps}, [][]byte{pps}},
		{"32 SPS", many(32, sps), [][]byte{pps}},
		{"256 PPS", [][]byte{sps}, many(256, pps)},
		{"PPS over 65535 bytes", [][]byte{sps}, [][]byte{huge}},
		{"high profile SPS that does not parse", [][]byte{high[:6]}, [][]byte{pps}},
	}
	for _, tt := range tests {
		_, err := h264CodecPrivate(tt.sps, tt.pps)
		assert.Error(t, err, tt.name)
	}

	_, err := h264CodecPrivate(many(31, sps), many(255, pps))
	assert.NoError(t, err, "31 SPS and 255 PPS fit")
}

func FuzzParseH264SPS(f *testing.F) {
	for _, v := range h264TestSPS {
		f.Add(mustHex(f, v.nal))
	}
	for _, s := range []testSPS{
		{profile: 244, chromaFormat: 3, separatePlanes: true, widthMBsMinus1: 39, heightMUsMinus1: 11, fieldCoding: true, crop: []uint64{1, 2, 0, 12}},
		{profile: 77, pocType: 1, pocCycle: []int64{1, -2, 3}, widthMBsMinus1: 79, heightMUsMinus1: 44},
		{profile: 100, chromaFormat: 0, widthMBsMinus1: 39, heightMUsMinus1: 22, scaling: [][]int64{{-8}, nil, nil, {120, 10, 118}, nil, nil, nil, flatList(64, 3)}},
	} {
		f.Add(s.nal())
	}
	pps := [][]byte{{0x68, 0xce, 0x3c, 0x80}}
	f.Fuzz(func(t *testing.T, nal []byte) {
		sps, err := parseH264SPS(nal)
		if err == nil {
			assert.Positive(t, sps.width)
			assert.Positive(t, sps.height)
			assert.LessOrEqual(t, sps.chromaFormatIDC, 3)
		}
		cp, err := h264CodecPrivate([][]byte{nal}, pps)
		if err == nil {
			require.GreaterOrEqual(t, len(cp), 7+len(nal))
			assert.Equal(t, nal[1:4], cp[1:4], fmt.Sprintf("%x", nal))
		}
	})
}
