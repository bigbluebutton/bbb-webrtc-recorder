package oracle_test

import (
	"testing"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func opusRead(seq uint16, ts uint32, atUs int64) corpus.Record {
	return corpus.Record{T: corpus.RecordRTP, AtUs: atUs, Seq: seq, TS: ts, PT: 111, PLen: 80, Pay: []byte{0xFC}}
}

func TestTimelineLag(t *testing.T) {
	// Three 20 ms frames read 25 ms apart, across the timestamp wrap; a
	// duplicate of the second, read later, does not move its read time.
	recs := []corpus.Record{
		opusRead(1, 0xFFFFFC40, 0),
		opusRead(2, 0x00000000, 25000),
		opusRead(2, 0x00000000, 26000),
		{T: corpus.RecordRTP, AtUs: 30000, Seq: 9, TS: 12345, Pad: true},
		opusRead(3, 0x000003C0, 50000),
		{T: corpus.RecordEOF, AtUs: 60000},
	}

	tr := &corpus.Trace{Header: corpus.NewSyntheticHeader(corpus.KindAudio, 1, "test"), Records: recs}
	lag, err := oracle.TimelineLag(&oracle.Track{Blocks: 3, PTS: []int64{100, 120, 140}}, tr)
	require.NoError(t, err)
	assert.Equal(t, oracle.Lag{Frames: 3, MaxMs: -10, FinalMs: -10}, lag)

	lag, err = oracle.TimelineLag(&oracle.Track{Blocks: 3, PTS: []int64{100, 130, 150}}, tr)
	require.NoError(t, err)
	assert.Equal(t, oracle.Lag{Frames: 3, MaxMs: 5, FinalMs: 0}, lag)

	_, err = oracle.TimelineLag(&oracle.Track{Blocks: 2, PTS: []int64{0, 20}}, tr)
	assert.Error(t, err, "a frame read but not written")
}

func TestTimelineLagAfterAStall(t *testing.T) {
	// 21 frames 20 ms apart, sent on time; frames 2 to 15 wait in a stall
	// and are read together at 300 ms, then delivery is on time again. The
	// frames after the stall place the ones read in it where they belong.
	var recs []corpus.Record
	pts := make([]int64, 21)
	for i := range 21 {
		at := int64(i) * 20000
		if i >= 2 && i <= 15 {
			at = 300000 + int64(i)
		}
		recs = append(recs, opusRead(uint16(i), uint32(i)*960, at))
		pts[i] = int64(i) * 20
	}
	tr := &corpus.Trace{Header: corpus.NewSyntheticHeader(corpus.KindAudio, 1, "test"), Records: recs}

	lag, err := oracle.TimelineLag(&oracle.Track{Blocks: 21, PTS: pts}, tr)
	require.NoError(t, err)
	assert.Equal(t, oracle.Lag{Frames: 21}, lag)

	// A step wider than two frames, here a realignment after frame 14, starts
	// over: frame 14 keeps its read time, 20 ms late, and so the frames of
	// the stall before it are placed 20 ms late too.
	for i := 15; i < 21; i++ {
		recs[i].TS += 48000
	}
	lag, err = oracle.TimelineLag(&oracle.Track{Blocks: 21, PTS: pts}, tr)
	require.NoError(t, err)
	assert.InDelta(t, -20, lag.MaxMs, 0.1)
}

func TestTimelineLagFastTimestamps(t *testing.T) {
	// 200 frames read 20 ms apart whose timestamps step by 21 ms, written as
	// they are: the file plays the last one 199 ms late.
	var recs []corpus.Record
	pts := make([]int64, 200)
	for i := range 200 {
		recs = append(recs, opusRead(uint16(i), uint32(i)*1008, int64(i)*20000))
		pts[i] = int64(i) * 21
	}
	tr := &corpus.Trace{Header: corpus.NewSyntheticHeader(corpus.KindAudio, 1, "test"), Records: recs}

	lag, err := oracle.TimelineLag(&oracle.Track{Blocks: 200, PTS: pts}, tr)
	require.NoError(t, err)
	assert.Equal(t, oracle.Lag{Frames: 200, MaxMs: 199, FinalMs: 199}, lag)
}

func TestTimelineLagBacklogAtStart(t *testing.T) {
	// The first five 20 ms frames are read together at 100 ms, the rest on
	// time: the first read found 80 ms of backlog.
	var recs []corpus.Record
	for k := range 8 {
		at := int64(100000)
		if k > 4 {
			at += int64(k-4) * 20000
		}
		recs = append(recs, opusRead(uint16(k), uint32(k)*960, at))
	}

	tr := &corpus.Trace{Header: corpus.NewSyntheticHeader(corpus.KindAudio, 1, "test"), Records: recs}
	lag, err := oracle.TimelineLag(&oracle.Track{Blocks: 8, PTS: []int64{0, 20, 40, 60, 80, 100, 120, 140}}, tr)
	require.NoError(t, err)
	assert.Equal(t, oracle.Lag{Frames: 8, StartMs: 80}, lag)
}
