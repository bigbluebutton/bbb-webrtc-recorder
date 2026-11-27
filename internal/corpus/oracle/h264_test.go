package oracle_test

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/at-wat/ebml-go/webm"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	h264SPS = corpus.H264SPS(640, 360)
	h264PPS = []byte{0x68, 0xcb, 0x83, 0xcb, 0x20}
	h264IDR = []byte{0x65, 0x88, 0x84, 0x00}
	h264P   = []byte{0x41, 0x9a, 0x00}
)

// avcC is a version 1 avcC record with 4-byte NAL lengths.
func avcC(sps, pps []byte) []byte {
	out := []byte{1, sps[1], sps[2], sps[3], 0xfc | 3, 0xe0 | 1}
	out = binary.BigEndian.AppendUint16(out, uint16(len(sps)))
	out = append(out, sps...)
	out = append(out, 1)
	out = binary.BigEndian.AppendUint16(out, uint16(len(pps)))

	return append(out, pps...)
}

// avcc is a block of NAL units with 4-byte lengths.
func avcc(nals ...[]byte) []byte {
	var out []byte
	for _, n := range nals {
		out = binary.BigEndian.AppendUint32(out, uint32(len(n)))
		out = append(out, n...)
	}

	return out
}

type h264Block struct {
	key  bool
	data []byte
}

func writeH264(t *testing.T, codecPrivate []byte, blocks ...h264Block) string {
	return writeRaw(t, []webm.TrackEntry{{
		Name: "Video", TrackNumber: 1, TrackUID: 1, CodecID: oracle.CodecH264, CodecPrivate: codecPrivate, TrackType: 1,
		Video: &webm.Video{PixelWidth: 640, PixelHeight: 360},
	}}, func(ws []webm.BlockWriteCloser) {
		for i, b := range blocks {
			_, err := ws[0].Write(b.key, int64(i*40), b.data)
			require.NoError(t, err)
		}
	})
}

func TestSummarizeWebMH264Recorded(t *testing.T) {
	tr, err := corpus.GenerateH264(corpus.H264Options{
		FPS: 25, Frames: 10, KeyInterval: 5, Slices: 2, KeySlicePackets: 3, StartTS: 90000, SSRC: 1,
	})
	require.NoError(t, err)

	file := filepath.Join(t.TempDir(), "out.mkv")
	rec := recorder.NewWebmRecorder(file, 0o600, 256, 32, true, false)
	rec.SetHasVideo(true)
	require.NoError(t, rec.SetVideoCodec(recorder.CodecH264))

	var sample recorder.VideoSample
	tagged := map[uint32]bool{}
	for i, r := range tr.Records {
		p := r.Packet()
		if !tagged[p.Timestamp] {
			tagged[p.Timestamp] = corpus.TagH264Frame(p.Payload, p.Timestamp)
		}
		sample.Packets = append(sample.Packets, recorder.ArrivedPacket{Packet: p, At: time.Now()})
		if i == len(tr.Records)-1 || tr.Records[i+1].TS != r.TS {
			rec.PushVideoSample(sample)
			sample = recorder.VideoSample{Follows: true}
		}
	}
	rec.Close()

	s, err := oracle.SummarizeWebM(rec.GetFilePath())
	require.NoError(t, err)
	assert.Equal(t, "matroska", s.DocType)
	require.Len(t, s.Tracks, 1)

	vt := s.Track(oracle.CodecH264)
	require.NotNil(t, vt)
	assert.Equal(t, [2]int{640, 360}, [2]int{vt.Width, vt.Height})
	assert.Equal(t, 10, vt.Blocks)
	assert.Equal(t, 2, vt.KeyBlocks)
	assert.Equal(t, 2, vt.H264Keyframes)
	assert.Zero(t, vt.VP8Keyframes)
	assert.Empty(t, vt.H264Errors)
	require.NoError(t, vt.CheckH264())
	assert.Equal(t, []int64{0, 40, 80, 120, 160, 200, 240, 280, 320, 360}, vt.PTS)

	var want []uint32
	for f := range 10 {
		want = append(want, 90000+uint32(f)*3600)
	}
	assert.Equal(t, want, vt.Tags)
	assert.Equal(t, []bool{true, true, true, true, true, true, true, true, true, true}, vt.TagOK)

	p, err := oracle.VideoPlacement(vt, tr)
	require.NoError(t, err)
	assert.Equal(t, 10, p.Placed)
	assert.Zero(t, p.Dropped)
}

// h264AsRead is an H.264 trace whose payloads are whole and tagged, as replay
// reads them.
func h264AsRead(t *testing.T) *corpus.Trace {
	tr, err := corpus.GenerateH264(corpus.H264Options{
		FPS: 25, Frames: 10, KeyInterval: 5, Slices: 2, KeySlicePackets: 3, StartTS: 90000, SSRC: 1,
	})
	require.NoError(t, err)

	for i, r := range tr.Records {
		p := r.Packet().Payload
		for k := len(r.Pay); k < len(p); k++ {
			p[k] = 0x80 | byte(i+k)
		}
		if corpus.H264PictureStart(p) {
			require.True(t, corpus.TagH264Frame(p, r.TS))
		}
		tr.Records[i].Pay, tr.Records[i].PLen = p, len(p)
	}

	return tr
}

func TestCheckH264BlocksAgainstTheFramesRead(t *testing.T) {
	tr := h264AsRead(t)
	file := filepath.Join(t.TempDir(), "out.mkv")
	rec := recorder.NewWebmRecorder(file, 0o600, 256, 32, true, false)
	rec.SetHasVideo(true)
	require.NoError(t, rec.SetVideoCodec(recorder.CodecH264))
	sample := recorder.VideoSample{}
	for i, r := range tr.Records {
		sample.Packets = append(sample.Packets, recorder.ArrivedPacket{Packet: r.Packet(), At: time.Now()})
		if i == len(tr.Records)-1 || tr.Records[i+1].TS != r.TS {
			rec.PushVideoSample(sample)
			sample = recorder.VideoSample{Follows: true}
		}
	}
	rec.Close()

	s, err := oracle.SummarizeWebM(rec.GetFilePath())
	require.NoError(t, err)
	vt := s.Track(oracle.CodecH264)
	require.NotNil(t, vt)
	require.Equal(t, 10, vt.Blocks)
	require.NoError(t, oracle.CheckH264Blocks(vt, tr))

	entries, blocks, err := oracle.ReadBlocks(rec.GetFilePath())
	require.NoError(t, err)
	for _, c := range []struct {
		name   string
		damage func([]byte) []byte
	}{
		{"its second slice dropped", func(d []byte) []byte { return d[:4+binary.BigEndian.Uint32(d)] }},
		{"its slices written twice", func(d []byte) []byte { return append(d, d...) }},
		{"its last byte zeroed", func(d []byte) []byte { d[len(d)-1] = 0; return d }},
	} {
		var damaged []h264Block
		for k, b := range blocks {
			data := slices.Clone(b.Data)
			if k == 3 {
				data = c.damage(data)
			}
			damaged = append(damaged, h264Block{b.Key, data})
		}

		s, err := oracle.SummarizeWebM(writeH264(t, entries[0].CodecPrivate, damaged...))
		require.NoError(t, err)
		err = oracle.CheckH264Blocks(s.Track(oracle.CodecH264), tr)
		assert.ErrorContains(t, err, "block 3 (PTS 120 ms): ", c.name)
		assert.ErrorContains(t, err, "the NAL units of frame 100800", c.name)
	}

	// A fragment in the middle of key frame 5's first IDR slice is lost.
	i := slices.IndexFunc(tr.Records, func(r corpus.Record) bool { return r.TS == 90000+5*3600 }) + 2
	lost := &corpus.Trace{Header: tr.Header, Records: slices.Delete(slices.Clone(tr.Records), i, i+1)}
	err = oracle.CheckH264Blocks(vt, lost)
	assert.ErrorContains(t, err, fmt.Sprintf("block 5 (PTS 200 ms): frame 108000 was not read whole: sequence numbers %d-%d not read",
		tr.Records[i].Seq, tr.Records[i].Seq))
}

func TestSummarizeWebMH264Checks(t *testing.T) {
	key := h264Block{true, avcc(h264SPS, h264PPS, h264IDR)}
	delta := h264Block{false, avcc(h264P)}
	good := avcC(h264SPS, h264PPS)

	tests := []struct {
		name         string
		codecPrivate []byte
		blocks       []h264Block
		want         []string // one per problem
	}{
		{name: "valid", codecPrivate: good, blocks: []h264Block{key, delta, key, delta}},
		{
			name: "Annex-B start code", codecPrivate: good,
			blocks: []h264Block{key, {false, append([]byte{0, 0, 0, 1}, h264P...)}},
			want:   []string{"block 1 (PTS 40 ms): NAL unit 0: a NAL unit of type 1 and one byte"},
		},
		{
			name: "start code inside a length", codecPrivate: good,
			blocks: []h264Block{key, {false, avcc(append([]byte{0, 0, 1}, h264P...))}},
			want:   []string{"block 1 (PTS 40 ms): NAL unit 0: NAL unit type 0"},
		},
		{
			name: "zero length", codecPrivate: good,
			blocks: []h264Block{key, {false, append(avcc(h264P), 0, 0, 0, 0)}},
			want:   []string{"block 1 (PTS 40 ms): NAL unit 1: a zero length"},
		},
		{
			name: "length past the block", codecPrivate: good,
			blocks: []h264Block{key, {false, avcc(h264P)[:6]}},
			want:   []string{"block 1 (PTS 40 ms): NAL unit 0: a length of 3 overruns the 2 bytes left"},
		},
		{
			name: "truncated length", codecPrivate: good,
			blocks: []h264Block{key, {false, append(avcc(h264P), 0, 0)}},
			want:   []string{"block 1 (PTS 40 ms): NAL unit 1: 2 bytes left, too few for a length"},
		},
		{
			name: "forbidden bit", codecPrivate: good,
			blocks: []h264Block{key, {false, avcc([]byte{0xc1, 0x9a})}},
			want:   []string{"block 1 (PTS 40 ms): NAL unit 0: forbidden_zero_bit set"},
		},
		{
			name: "key flag without an IDR", codecPrivate: good,
			blocks: []h264Block{key, {true, avcc(h264SPS, h264PPS, h264P)}},
			want:   []string{"block 1 (PTS 40 ms): key flag set, but no IDR slice"},
		},
		{
			name: "IDR without the key flag", codecPrivate: good,
			blocks: []h264Block{key, {false, key.data}},
			want:   []string{"block 1 (PTS 40 ms): an IDR slice, but no key flag"},
		},
		{
			name: "first block not a key block", codecPrivate: good,
			blocks: []h264Block{delta, key},
			want:   []string{"block 0 (PTS 0 ms): the first block is not a key block"},
		},
		{
			name: "key block without parameter sets", codecPrivate: good,
			blocks: []h264Block{key, {true, avcc(h264PPS, h264IDR)}, {true, avcc(h264IDR)}},
			want: []string{
				"block 1 (PTS 40 ms): a key block without an SPS",
				"block 2 (PTS 80 ms): a key block without an SPS",
				"block 2 (PTS 80 ms): a key block without a PPS",
			},
		},
		{
			name: "CodecPrivate from another SPS", codecPrivate: avcC(corpus.H264SPS(320, 180), h264PPS),
			blocks: []h264Block{key, delta, key},
			want:   []string{"block 0 (PTS 0 ms): the first key block's SPS is not CodecPrivate's"},
		},
		{
			name: "CodecPrivate from another PPS", codecPrivate: avcC(h264SPS, []byte{0x68, 0xce, 0x3c, 0x80}),
			blocks: []h264Block{key},
			want:   []string{"block 0 (PTS 0 ms): the first key block's PPS is not CodecPrivate's"},
		},
		{
			name: "a later key block with other parameter sets", codecPrivate: good,
			blocks: []h264Block{key, delta, {true, avcc(corpus.H264SPS(320, 180), h264PPS, h264IDR)}},
		},
		{
			name: "no CodecPrivate", blocks: []h264Block{key},
			want: []string{"CodecPrivate: 0 bytes, too short for an avcC record"},
		},
		{
			name: "avcC version", codecPrivate: append([]byte{0}, good[1:]...), blocks: []h264Block{key},
			want: []string{"CodecPrivate: avcC version 0, want 1"},
		},
		{
			name: "avcC NAL length size", codecPrivate: append(append([]byte(nil), good[:4]...), append([]byte{0xfd}, good[5:]...)...),
			blocks: []h264Block{key},
			want:   []string{"CodecPrivate: lengthSizeMinusOne 1, want 3"},
		},
		{
			name: "avcC without an SPS", codecPrivate: []byte{1, 0x42, 0xc0, 0x1f, 0xff, 0xe0, 1, 0, 5, 0x68, 0xcb, 0x83, 0xcb, 0x20},
			blocks: []h264Block{key},
			want:   []string{"CodecPrivate: no SPS"},
		},
		{
			name: "avcC without a PPS", codecPrivate: good[:len(good)-len(h264PPS)-3], blocks: []h264Block{key},
			want: []string{"CodecPrivate: no PPS count"},
		},
		{
			name: "avcC PPS overruns", codecPrivate: good[:len(good)-1], blocks: []h264Block{key},
			want: []string{"CodecPrivate: PPS 0 of 5 bytes overruns the 4 left"},
		},
		{
			name: "avcC profile differs from the SPS", codecPrivate: append([]byte{1, 0x64}, good[2:]...),
			blocks: []h264Block{key},
			want:   []string{"CodecPrivate: profile, compatibility and level 64c01f, the first SPS has 42c01f"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := oracle.SummarizeWebM(writeH264(t, tt.codecPrivate, tt.blocks...))
			require.NoError(t, err)

			vt := s.Track(oracle.CodecH264)
			require.NotNil(t, vt)
			assert.Equal(t, tt.want, vt.H264Errors)
			if tt.want == nil {
				assert.NoError(t, vt.CheckH264())
			} else {
				assert.ErrorContains(t, vt.CheckH264(), tt.want[0])
			}
		})
	}
}

func TestSummarizeWebMH264Keyframes(t *testing.T) {
	// An IDR counts with or without the key flag and its parameter sets.
	s, err := oracle.SummarizeWebM(writeH264(t, avcC(h264SPS, h264PPS),
		h264Block{true, avcc(h264SPS, h264PPS, h264IDR)},
		h264Block{false, avcc(h264P)},
		h264Block{false, avcc(h264IDR)},
		h264Block{true, avcc(h264SPS, h264PPS, h264IDR, h264IDR)},
	))
	require.NoError(t, err)

	vt := s.Track(oracle.CodecH264)
	require.NotNil(t, vt)
	assert.Equal(t, 3, vt.H264Keyframes)
	assert.Equal(t, 2, vt.KeyBlocks)
}

func TestSummarizeWebMH264FrameTags(t *testing.T) {
	// The tag sits in the first slice, past the in-band parameter sets and
	// an SEI, over 64 bytes into the block.
	slice := func(header byte, tag uint32) []byte {
		p := append([]byte{header, 0x88, 0x84, 0, 0, 0, 0, 0, 0}, make([]byte, 16)...)
		require.True(t, corpus.TagH264Frame(p, tag))

		return p
	}
	sps := corpus.H264SPS(1920, 1080)
	sei := append([]byte{0x06}, make([]byte, 39)...)

	s, err := oracle.SummarizeWebM(writeH264(t, avcC(sps, h264PPS),
		h264Block{true, avcc(sps, h264PPS, sei, slice(0x65, 90000), slice(0x65, 1))},
		h264Block{false, avcc(slice(0x41, 93600))},
		h264Block{false, avcc(h264P)},
	))
	require.NoError(t, err)

	vt := s.Track(oracle.CodecH264)
	require.NotNil(t, vt)
	assert.Equal(t, []uint32{90000, 93600, 0}, vt.Tags)
	assert.Equal(t, []bool{true, true, false}, vt.TagOK)
}

func TestVideoPlacementH264FramesHoldASlice(t *testing.T) {
	// At a simulcast start, LiveKit forwards the low layer's SPS and PPS
	// alone. The high layer's key frame follows one tick later. Only that
	// key frame and the delta after it are frames.
	stap := func(nals ...[]byte) []byte {
		out := []byte{0x78}
		for _, n := range nals {
			out = binary.BigEndian.AppendUint16(out, uint16(len(n)))
			out = append(out, n...)
		}

		return out
	}
	read := func(seq uint16, ts uint32, atUs int64, pay []byte) corpus.Record {
		return corpus.Record{T: corpus.RecordRTP, AtUs: atUs, Seq: seq, TS: ts, PT: 102, PLen: len(pay) + 100, Pay: pay}
	}
	h := corpus.NewSyntheticHeader(corpus.KindVideo, 1, "test")
	h.Codec = corpus.CodecH264
	tr := &corpus.Trace{Header: h, Records: []corpus.Record{
		read(1, 0, 0, stap(corpus.H264SPS(320, 180), h264PPS)),
		read(2, 1, 1000, stap(h264SPS, h264PPS)),
		read(3, 1, 1500, []byte{0x7c, 0x85}),
		read(4, 1, 2000, []byte{0x7c, 0x45}),
		read(5, 3601, 41000, h264P),
		{T: corpus.RecordEOF, AtUs: 50000},
	}}

	vt := &oracle.Track{Codec: oracle.CodecH264, Blocks: 2, PTS: []int64{0, 40}, Tags: []uint32{1, 3601}, TagOK: []bool{true, true},
		Keyframes: []bool{true, false}}
	p, err := oracle.VideoPlacement(vt, tr)
	require.NoError(t, err)
	assert.Equal(t, 2, p.Frames)
	assert.Equal(t, 0, p.Dropped)
	assert.InDelta(t, 1000, p.IdealStartUs, 1e-6, "the key frame's first read, its parameter sets")
}
