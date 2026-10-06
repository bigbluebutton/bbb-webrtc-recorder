package oracle_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/at-wat/ebml-go/mkvcore"
	"github.com/at-wat/ebml-go/webm"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 25 fps keeps the recorder's float tick-to-ms conversion on whole ms.
const videoTicks = 3600

var opusPayload = []byte{0xFC, 0xFF, 0xFE}

type vp8Stream struct {
	seq uint16
	pic uint16
}

// frame returns the RTP packets of one VP8 frame split into n packets. The
// frame is 16 bytes, so it passes the recorder's size and dimension checks.
func (s *vp8Stream) frame(ts uint32, key bool, n int) []*rtp.Packet {
	data := []byte{0x11, 0x02, 0x00}
	if key {
		data = []byte{0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x40, 0x01, 0xf0, 0x00} // 320x240
	}
	data = append(data, make([]byte, 16-len(data))...)

	s.pic++
	chunk := (len(data) + n - 1) / n
	var pkts []*rtp.Packet
	for i := 0; i < n; i++ {
		desc := byte(0x80) // X
		if i == 0 {
			desc |= 0x10 // S, partition 0
		}
		pay := []byte{desc, 0x80, 0x80 | byte(s.pic>>8), byte(s.pic)} // I, 15-bit picture ID
		pay = append(pay, data[i*chunk:min((i+1)*chunk, len(data))]...)

		s.seq++
		pkts = append(pkts, &rtp.Packet{
			Header:  rtp.Header{SequenceNumber: s.seq, Timestamp: ts, Marker: i == n-1},
			Payload: pay,
		})
	}

	return pkts
}

func newRecorder(t *testing.T, video, audio bool) (*recorder.WebmRecorder, string) {
	file := filepath.Join(t.TempDir(), "out.webm")
	rec := recorder.NewWebmRecorder(file, 0o600, 256, 32, true, false)
	rec.SetHasVideo(video)
	rec.SetHasAudio(audio)

	return rec, file
}

func TestSummarizeWebMAudioDuplicateTimestamp(t *testing.T) {
	rec, file := newRecorder(t, false, true)

	// The fourth frame lands one tick after the third, in the same ms; the
	// recorder must skip it rather than write a duplicate PTS.
	for i, ts := range []uint32{0, 960, 1920, 1921, 1921 + 1968} {
		rec.PushAudio(&rtp.Packet{
			Header:  rtp.Header{SequenceNumber: uint16(100 + i), Timestamp: 48000 + ts},
			Payload: opusPayload,
		})
	}
	rec.Close()

	s, err := oracle.SummarizeWebM(file)
	require.NoError(t, err)
	require.Len(t, s.Tracks, 1)
	assert.Nil(t, s.Track(oracle.CodecVP8))

	a := s.Track(oracle.CodecOpus)
	require.NotNil(t, a)
	assert.Equal(t, &oracle.Track{
		Number:    1,
		Codec:     oracle.CodecOpus,
		Blocks:    4,
		KeyBlocks: 4,
		PTS:       []int64{0, 20, 40, 81},
		MaxGapMs:  41,
		Monotonic: true,
		FirstPTS:  0,
		LastPTS:   81,
	}, a)
}

func TestSummarizeWebMVideoKeyAndDeltaFrames(t *testing.T) {
	rec, file := newRecorder(t, true, false)

	var v vp8Stream
	frames := []struct {
		key  bool
		pkts int
	}{{true, 1}, {false, 1}, {false, 2}, {true, 3}, {false, 2}, {false, 1}}
	for i, f := range frames {
		for _, p := range v.frame(uint32(i*videoTicks), f.key, f.pkts) {
			rec.PushVideo(p)
		}
	}
	rec.Close()

	s, err := oracle.SummarizeWebM(file)
	require.NoError(t, err)
	require.Len(t, s.Tracks, 1)

	vt := s.Track(oracle.CodecVP8)
	require.NotNil(t, vt)
	assert.Equal(t, uint64(1), vt.Number)
	assert.Equal(t, 6, vt.Blocks)
	assert.Equal(t, 2, vt.VP8Keyframes)
	// The writer takes the keyframe flag from the frame's last packet, so
	// only the single-packet key frame carries it.
	assert.Equal(t, 1, vt.KeyBlocks)
	assert.Equal(t, []int64{0, 40, 80, 120, 160, 200}, vt.PTS)
	assert.Empty(t, vt.DuplicatePTS)
	assert.True(t, vt.Monotonic)
	assert.Equal(t, int64(40), vt.MaxGapMs)
	assert.Equal(t, int64(0), vt.FirstPTS)
	assert.Equal(t, int64(200), vt.LastPTS)
}

func TestSummarizeWebMVideoDuplicatePTS(t *testing.T) {
	rec, file := newRecorder(t, true, false)

	// A frame that repeats the previous RTP timestamp is still written, at
	// the previous frame's PTS.
	var v vp8Stream
	for i, ts := range []uint32{0, videoTicks, videoTicks, 2 * videoTicks} {
		for _, p := range v.frame(ts, i == 0, 1) {
			rec.PushVideo(p)
		}
	}
	rec.Close()

	s, err := oracle.SummarizeWebM(file)
	require.NoError(t, err)

	vt := s.Track(oracle.CodecVP8)
	require.NotNil(t, vt)
	assert.Equal(t, []int64{0, 40, 40, 80}, vt.PTS)
	assert.Equal(t, []int64{40}, vt.DuplicatePTS)
	assert.True(t, vt.Monotonic)
	require.Error(t, vt.CheckNoDuplicatePTS())
	assert.Contains(t, vt.CheckNoDuplicatePTS().Error(), "[40]")
	assert.NoError(t, vt.CheckMonotonic())
}

func TestSummarizeWebMAudioAndVideo(t *testing.T) {
	rec, file := newRecorder(t, true, true)

	// Interleaved on one 20 ms clock, video first, so the audio writer exists
	// before the first audio sample pops out of the sample builder.
	var v vp8Stream
	for i := range 25 {
		if i%2 == 0 {
			for _, p := range v.frame(uint32(i/2*videoTicks), i == 0, 2) {
				rec.PushVideo(p)
			}
		}
		rec.PushAudio(&rtp.Packet{
			Header:  rtp.Header{SequenceNumber: uint16(500 + i), Timestamp: uint32(i * 960)},
			Payload: opusPayload,
		})
	}
	rec.Close()

	s, err := oracle.SummarizeWebM(file)
	require.NoError(t, err)
	require.Len(t, s.Tracks, 2)

	vt := s.Track(oracle.CodecVP8)
	require.NotNil(t, vt)
	assert.Equal(t, uint64(1), vt.Number)
	assert.Equal(t, 13, vt.Blocks)
	assert.Equal(t, 1, vt.VP8Keyframes)
	assert.Equal(t, 0, vt.KeyBlocks)
	assert.Equal(t, int64(0), vt.FirstPTS)
	assert.Equal(t, int64(480), vt.LastPTS)
	assert.Equal(t, int64(40), vt.MaxGapMs)

	at := s.Track(oracle.CodecOpus)
	require.NotNil(t, at)
	assert.Equal(t, uint64(2), at.Number)
	assert.Equal(t, 25, at.Blocks)
	assert.Equal(t, at.Blocks, at.KeyBlocks)
	assert.Equal(t, 0, at.VP8Keyframes)
	assert.Equal(t, int64(0), at.FirstPTS)
	assert.Equal(t, int64(480), at.LastPTS)
	assert.Equal(t, int64(20), at.MaxGapMs)

}

// writeRaw writes blocks as given, with no sorter dropping late ones, to
// cover what the recorder cannot produce on demand.
func writeRaw(t *testing.T, tracks []webm.TrackEntry, blocks func(ws []webm.BlockWriteCloser)) string {
	file := filepath.Join(t.TempDir(), "raw.webm")
	f, err := os.Create(file)
	require.NoError(t, err)

	ws, err := webm.NewSimpleBlockWriter(f, tracks, mkvcore.WithBlockInterceptor(nil))
	require.NoError(t, err)
	blocks(ws)
	for _, w := range ws {
		require.NoError(t, w.Close())
	}

	return file
}

func TestSummarizeWebMBackwardPTSAndClusters(t *testing.T) {
	key := []byte{0x10, 0x02, 0x00}
	delta := []byte{0x11, 0x02, 0x00}
	file := writeRaw(t, []webm.TrackEntry{
		{Name: "Video", TrackNumber: 1, TrackUID: 1, CodecID: oracle.CodecVP8, TrackType: 1},
	}, func(ws []webm.BlockWriteCloser) {
		// 40000 ms forces a second cluster: block timecodes are int16.
		for _, b := range []struct {
			key  bool
			pts  int64
			data []byte
		}{
			{true, 0, key}, {false, 20, delta}, {false, 20, delta}, {false, 10, key},
			{false, 60, delta}, {true, 40000, delta},
		} {
			_, err := ws[0].Write(b.key, b.pts, b.data)
			require.NoError(t, err)
		}
	})

	s, err := oracle.SummarizeWebM(file)
	require.NoError(t, err)

	vt := s.Track(oracle.CodecVP8)
	require.NotNil(t, vt)
	assert.Equal(t, []int64{0, 20, 20, 10, 60, 40000}, vt.PTS)
	assert.Equal(t, []int64{20}, vt.DuplicatePTS)
	assert.False(t, vt.Monotonic)
	assert.Equal(t, int64(40000-60), vt.MaxGapMs)
	assert.Equal(t, 2, vt.KeyBlocks)
	assert.Equal(t, 2, vt.VP8Keyframes)
	assert.Equal(t, int64(40000), vt.LastPTS)

	err = vt.CheckMonotonic()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "from 20 to 10 at block 3")
}

func TestSummarizeWebMErrors(t *testing.T) {
	_, err := oracle.SummarizeWebM(filepath.Join(t.TempDir(), "missing.webm"))
	require.ErrorIs(t, err, os.ErrNotExist)

	junk := filepath.Join(t.TempDir(), "junk.webm")
	require.NoError(t, os.WriteFile(junk, []byte("not a webm file"), 0o600))
	_, err = oracle.SummarizeWebM(junk)
	require.Error(t, err)

	twin := writeRaw(t, []webm.TrackEntry{
		{Name: "A", TrackNumber: 1, TrackUID: 1, CodecID: oracle.CodecOpus, TrackType: 2},
		{Name: "B", TrackNumber: 2, TrackUID: 2, CodecID: oracle.CodecOpus, TrackType: 2},
	}, func([]webm.BlockWriteCloser) {})
	_, err = oracle.SummarizeWebM(twin)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "more than one A_OPUS track")
}
