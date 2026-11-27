package corpus

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// h264NAL rebuilds the NAL unit a single NAL or FU-A start payload begins,
// as a depacketizer writes it into a block.
func h264NAL(p []byte) []byte {
	if p[0]&0x1f != h264FUA {
		return p
	}

	return append([]byte{p[0]&0xe0 | p[1]&0x1f}, p[2:]...)
}

func TestTagH264FrameRoundTrip(t *testing.T) {
	tr, err := GenerateH264(H264Options{FPS: 30, Frames: 2, KeySlicePackets: 3, DeltaPLen: 40})
	require.NoError(t, err)

	for _, c := range []struct {
		name   string
		record int
	}{{"FU-A start", 1}, {"single NAL unit", 4}} {
		t.Run(c.name, func(t *testing.T) {
			r := tr.Records[c.record]
			payload := r.Packet().Payload
			want := bytes.Clone(payload)
			require.True(t, H264SliceStart(payload))

			require.True(t, TagH264Frame(payload, 0xDEADBEEF))

			nal := h264NAL(payload)
			assert.Equal(t, []byte("BBBF"), nal[9:13], "8 bytes past the NAL unit header")
			assert.Equal(t, h264NAL(want)[:9], nal[:9], "the NAL unit header and slice header prefix are unchanged")
			assert.Equal(t, len(want), len(payload))

			v, ok := TaggedFrame(nal)
			require.True(t, ok)
			assert.Equal(t, uint32(0xDEADBEEF), v)
		})
	}
}

func TestTagH264FrameSkipsPacketsThatStartNoSlice(t *testing.T) {
	tr, err := GenerateH264(H264Options{FPS: 30, Frames: 1, KeySlicePackets: 3, KeyPLen: 40})
	require.NoError(t, err)

	stap := tr.Records[0].Packet().Payload
	middle := tr.Records[2].Packet().Payload
	sps := append(H264SPS(640, 360), make([]byte, 20)...)
	blank := h264STAP(H264SPS(2, 2), h264PPS, append([]byte{h264HeaderIDR}, make([]byte, 20)...))

	for name, p := range map[string][]byte{
		"STAP-A of parameter sets": stap,
		"FU-A continuation":        middle,
		"SPS":                      sps,
		"STAP-A holding an IDR":    blank,
		"empty":                    nil,
	} {
		want := bytes.Clone(p)
		assert.False(t, H264SliceStart(p), name)
		assert.False(t, TagH264Frame(p, 1), name)
		assert.Equal(t, want, p, name)
	}
}

// Only the slice that starts the picture is tagged, so a frame written
// without it names no frame.
func TestTagH264FrameSkipsLaterSlices(t *testing.T) {
	tr, err := GenerateH264(H264Options{FPS: 30, Frames: 2, Slices: 3, KeySlicePackets: 2, KeyPLen: 40, DeltaPLen: 40})
	require.NoError(t, err)

	var tagged []bool
	for _, r := range tr.Records {
		tagged = append(tagged, TagH264Frame(r.Packet().Payload, r.TS))
	}
	// Key frame: STAP-A, then 3 slices of 2 FU-A fragments. Delta: 3 slices.
	assert.Equal(t, []bool{false, true, false, false, false, false, false, true, false, false}, tagged)
	assert.True(t, H264SliceStart(tr.Records[3].Packet().Payload), "a later slice starts a slice")
	assert.False(t, H264PictureStart(tr.Records[3].Packet().Payload), "but not the picture")
}

func TestTagH264FrameTooShort(t *testing.T) {
	// The NAL unit or FU-A header, the 8 bytes the tag skips, then 7 bytes:
	// one short of the tag. Each slice header starts at macroblock 0.
	single := append([]byte{h264HeaderP, 0x80}, make([]byte, 14)...)
	fu := append([]byte{0x7c, 0x85, 0x80}, make([]byte, 14)...)

	for name, p := range map[string][]byte{"single NAL unit": single, "FU-A start": fu} {
		want := bytes.Clone(p)
		require.True(t, H264SliceStart(p), name)
		assert.False(t, TagH264Frame(p, 1), name)
		assert.Equal(t, want, p, name)

		assert.True(t, TagH264Frame(append(p, 0), 1), "%s: one byte longer", name)
	}
}

// The tag never covers the slice header prefix GenerateH264 writes: 8
// bytes hold it at the largest size and slice count.
func TestGenerateH264SliceHeaderFitsBeforeTheTag(t *testing.T) {
	tr, err := GenerateH264(H264Options{FPS: 30, Frames: 2, Slices: 8, Width: 4096, Height: 4096, KeySlicePackets: 2})
	require.NoError(t, err)

	for i, r := range tr.Records {
		p := r.Packet().Payload
		if !H264SliceStart(p) {
			continue
		}
		assert.LessOrEqual(t, len(h264NAL(r.Pay)), 1+h264TagSkip, "record %d", i)
	}
}

func TestTagH264FrameInAStapA(t *testing.T) {
	idr := append([]byte{h264HeaderIDR, 0x88}, make([]byte, 30)...)
	p := h264STAP(H264SPS(640, 360), h264PPS, idr)
	want := bytes.Clone(p)

	require.True(t, H264PictureStart(p))
	require.True(t, TagH264Frame(p, 0xDEADBEEF))

	assert.Len(t, p, len(want), "the STAP-A sizes still match")
	nal := p[len(p)-len(idr):]
	assert.Equal(t, want[:len(want)-len(idr)+9], p[:len(p)-len(idr)+9], "the parameter sets and the slice header prefix are unchanged")
	v, ok := TaggedFrame(nal)
	require.True(t, ok)
	assert.Equal(t, uint32(0xDEADBEEF), v)
}

func TestTagH264FrameSkipsAStapAItCannotHoldTheTagIn(t *testing.T) {
	blank := h264STAP(H264SPS(2, 2), h264PPS, []byte{h264HeaderIDR, 0x88})
	assert.True(t, H264PictureStart(blank))
	assert.False(t, TagH264Frame(blank, 1), "a blank frame's IDR is too short")

	later := h264STAP(append([]byte{0x41, 0x40}, make([]byte, 30)...))
	assert.False(t, H264PictureStart(later), "a later slice")
	assert.False(t, TagH264Frame(later, 1))

	assert.False(t, H264PictureStart(h264STAP(H264SPS(640, 360), h264PPS)), "no slice")
}
