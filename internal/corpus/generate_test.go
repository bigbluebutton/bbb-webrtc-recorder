package corpus

import (
	"bytes"
	"testing"
	"time"

	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateVP8Deterministic(t *testing.T) {
	o := VP8Options{FPS: 15, Duration: 2 * time.Second, KeyInterval: 7, SpreadUs: 5000, SSRC: 77}

	a, err := GenerateVP8(o)
	require.NoError(t, err)
	b, err := GenerateVP8(o)
	require.NoError(t, err)
	assert.Equal(t, a, b)

	var wa, wb bytes.Buffer
	require.NoError(t, a.Write(&wa))
	require.NoError(t, b.Write(&wb))
	assert.Equal(t, wa.Bytes(), wb.Bytes())
}

func TestGenerateVP8Shape(t *testing.T) {
	tr, err := GenerateVP8(VP8Options{
		FPS: 30, Frames: 25, KeyPackets: 3, DeltaPackets: 2, KeyInterval: 10,
		KeyPLen: 900, DeltaPLen: 300, SpreadUs: 10000, Width: 320, Height: 180,
		StartSeq: 65530, StartTS: 4294967295 - 6000, StartPictureID: 0x7FFE, SSRC: 5,
	})
	require.NoError(t, err)
	require.NoError(t, tr.Validate())

	assert.Equal(t, Header{V: 1, Kind: KindVideo, Codec: CodecVP8, ClockRate: 90000, SSRC: 5,
		Provenance: ProvenanceSynthetic, Generator: "corpus.GenerateVP8"}, tr.Header)
	require.Len(t, tr.Records, 3*3+22*2)

	seq := uint16(65530)
	frame, j := 0, 0

	for i, r := range tr.Records {
		key := frame%10 == 0
		n, plen := 2, 300
		if key {
			n, plen = 3, 900
		}

		assert.Equal(t, seq, r.Seq, "seq wraps through 0 at record %d", i)
		assert.Equal(t, uint32(4294967295-6000)+uint32(frame*3000), r.TS)
		assert.Equal(t, (int64(frame)*1000000+15)/30+int64(j)*10000/int64(n-1), r.AtUs, "record %d", i)
		assert.Equal(t, j == n-1, r.M)
		assert.Equal(t, uint8(96), r.PT)
		assert.Equal(t, plen, r.PLen)

		var vp8 codecs.VP8Packet
		_, err := vp8.Unmarshal(r.Packet().Payload)
		require.NoError(t, err)
		assert.Equal(t, uint16((0x7FFE+frame)&0x7FFF), vp8.PictureID, "picture ID wraps at 15 bits")
		assert.Zero(t, vp8.PID)
		assert.Equal(t, plen-4, len(vp8.Payload))

		if j == 0 {
			assert.Equal(t, uint8(1), vp8.S)
			assert.Equal(t, !key, vp8.Payload[0]&0x01 == 1, "the frame tag's bit0 is 0 only on key frames")

			if key {
				assert.Equal(t, []byte{0x9d, 0x01, 0x2a}, vp8.Payload[3:6], "key frame start code")
				assert.Equal(t, 320, int(vp8.Payload[6])|int(vp8.Payload[7])<<8)
				assert.Equal(t, 180, int(vp8.Payload[8])|int(vp8.Payload[9])<<8)
			}
		} else {
			assert.Zero(t, vp8.S)
			assert.Len(t, r.Pay, 4, "a continuation packet carries only the descriptor")
		}

		seq++
		if j++; j == n {
			frame, j = frame+1, 0
		}
	}
}

func TestGenerateVP8SpreadQueuesNextFrame(t *testing.T) {
	// A 6-packet key frame spread over 200 ms pushes the 33 ms-later delta frame behind it.
	tr, err := GenerateVP8(VP8Options{FPS: 30, Frames: 2, KeyPackets: 6, DeltaPackets: 1, SpreadUs: 200000})
	require.NoError(t, err)

	at := make([]int64, len(tr.Records))
	for i, r := range tr.Records {
		at[i] = r.AtUs
	}

	assert.Equal(t, []int64{0, 40000, 80000, 120000, 160000, 200000, 200000}, at)
}

func TestGenerateVP8Options(t *testing.T) {
	_, err := GenerateVP8(VP8Options{FPS: 30, Frames: 1})
	require.NoError(t, err)

	tr, err := GenerateVP8(VP8Options{FPS: 3.4, Duration: 10 * time.Second})
	require.NoError(t, err)
	assert.Len(t, tr.Records, 4+33*2, "34 frames at 3.4 fps")
	assert.Equal(t, uint32(26471), tr.Records[4].TS, "ts rounds per frame, without drift")

	for _, o := range []VP8Options{
		{Frames: 1},
		{FPS: 30},
		{FPS: 30, Frames: 1, Duration: time.Second},
		{FPS: 30, Frames: 1, KeyPLen: 13},
		{FPS: 30, Frames: 1, DeltaPackets: -1},
		{FPS: 30, Frames: 1, Width: 0x4000},
		{FPS: 30, Frames: 1, SpreadUs: -1},
	} {
		_, err := GenerateVP8(o)
		assert.Error(t, err, "%+v", o)
	}
}

func TestGenerateOpus(t *testing.T) {
	o := OpusOptions{Duration: time.Second, StartSeq: 65535, StartTS: 4294967295 - 960, SSRC: 3}
	tr, err := GenerateOpus(o)
	require.NoError(t, err)
	require.NoError(t, tr.Validate())

	again, err := GenerateOpus(o)
	require.NoError(t, err)
	assert.Equal(t, tr, again)

	assert.Equal(t, NewSyntheticHeader(KindAudio, 3, "corpus.GenerateOpus"), tr.Header)
	require.Len(t, tr.Records, 50)

	for i, r := range tr.Records {
		assert.Equal(t, uint16(65535)+uint16(i), r.Seq)
		assert.Equal(t, uint32(4294967295-960)+uint32(i)*960, r.TS)
		assert.Equal(t, int64(i)*20000, r.AtUs)
		assert.Equal(t, i == 0, r.M)
		assert.Equal(t, uint8(111), r.PT)
		assert.Equal(t, 80, r.PLen)
		assert.Equal(t, []byte{0xFC}, r.Pay)

		var op codecs.OpusPacket
		_, err := op.Unmarshal(r.Packet().Payload)
		require.NoError(t, err)
	}

	assert.Equal(t, uint8(31), tr.Records[0].Pay[0]>>3, "TOC config 31: CELT fullband, 20 ms")

	_, err = GenerateOpus(OpusOptions{})
	assert.Error(t, err)
	_, err = GenerateOpus(OpusOptions{Frames: 1, PLen: -1})
	assert.Error(t, err)
}
