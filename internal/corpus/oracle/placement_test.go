package oracle_test

import (
	"testing"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func vp8Read(seq uint16, ts uint32, atUs int64) corpus.Record {
	return corpus.Record{T: corpus.RecordRTP, AtUs: atUs, Seq: seq, TS: ts, PT: 96, PLen: 400}
}

func vp8Trace(recs ...corpus.Record) *corpus.Trace {
	return &corpus.Trace{Header: corpus.NewSyntheticHeader(corpus.KindVideo, 1, "test"), Records: recs}
}

// tagged is a VP8 track whose blocks, at pts, carry tags as their RTP
// timestamps.
func tagged(pts []int64, tags ...uint32) *oracle.Track {
	ok := make([]bool, len(tags))
	for i := range ok {
		ok[i] = true
	}

	return &oracle.Track{Codec: oracle.CodecVP8, Blocks: len(tags), PTS: pts, Tags: tags, TagOK: ok}
}

// fiveFrames is five frames at 25 fps, one packet each, read on time from
// 1 s. Frame 2 has a second packet, and a late RTX copy of its first.
func fiveFrames() *corpus.Trace {
	return vp8Trace(
		vp8Read(1, 0, 1000000),
		vp8Read(2, 3600, 1040000),
		vp8Read(3, 7200, 1080000),
		vp8Read(4, 7200, 1081000),
		corpus.Record{T: corpus.RecordRTP, AtUs: 1090000, Seq: 9, TS: 99, Pad: true},
		vp8Read(5, 10800, 1120000),
		vp8Read(6, 14400, 1160000),
		func() corpus.Record { r := vp8Read(3, 7200, 1200000); r.RTX = true; return r }(),
		corpus.Record{T: corpus.RecordEOF, AtUs: 1300000},
	)
}

func TestVideoPlacementDroppedFrames(t *testing.T) {
	// Frames 1 and 2 are read but dropped. In the second file, frame 3 is
	// written one frame step after frame 0, so it plays 80 ms early.
	tr := fiveFrames()

	p, err := oracle.VideoPlacement(tagged([]int64{0, 120, 160}, 0, 10800, 14400), tr)
	require.NoError(t, err)
	assert.Equal(t, 5, p.Frames)
	assert.Equal(t, 3, p.Placed)
	assert.Equal(t, 2, p.Dropped)
	assert.InDelta(t, 0, p.TimelineMinMs, 1e-9)
	assert.InDelta(t, 0, p.TimelineMaxMs, 1e-9)
	assert.InDelta(t, 1000000, p.IdealStartUs, 1e-6)

	p, err = oracle.VideoPlacement(tagged([]int64{0, 40, 80}, 0, 10800, 14400), tr)
	require.NoError(t, err)
	assert.InDelta(t, -80, p.TimelineMinMs, 1e-9, "a gap collapsed to one frame step")
	assert.Equal(t, 1, p.TimelineMinAt)
}

func TestVideoPlacementReadIsTheFirstCopy(t *testing.T) {
	// Frame 2 alone: its read is its first packet's at 1080 ms, not its
	// RTX copy's at 1200 ms.
	p, err := oracle.VideoPlacement(tagged([]int64{0}, 7200), fiveFrames())
	require.NoError(t, err)
	assert.InDelta(t, 1080000, p.IdealStartUs, 1e-6)
}

func TestVideoPlacementTimestampWrap(t *testing.T) {
	ts0 := uint32(0xFFFFF000)
	tr := vp8Trace(
		vp8Read(1, ts0, 0),
		vp8Read(2, ts0+3600, 40000),
		vp8Read(3, ts0+7200, 80000),
	)

	p, err := oracle.VideoPlacement(tagged([]int64{0, 40, 80}, ts0, ts0+3600, ts0+7200), tr)
	require.NoError(t, err)
	assert.Less(t, ts0+7200, ts0, "the last frame is past the wrap")
	assert.InDelta(t, 0, p.TimelineMinMs, 1e-9)
	assert.InDelta(t, 0, p.TimelineMaxMs, 1e-9)
}

func TestVideoPlacementErrors(t *testing.T) {
	tr := fiveFrames()

	untagged := tagged([]int64{0, 40}, 0, 3600)
	untagged.TagOK[1] = false
	_, err := oracle.VideoPlacement(untagged, tr)
	assert.ErrorContains(t, err, "block 1 (PTS 40 ms) has no frame tag")

	_, err = oracle.VideoPlacement(tagged([]int64{0, 40}, 0, 99), tr)
	assert.ErrorContains(t, err, "no frame read at timestamp 99", "a padding timestamp is no frame")

	_, err = oracle.VideoPlacement(tagged([]int64{0, 40, 80}, 0, 7200, 3600), tr)
	assert.ErrorContains(t, err, "tags not increasing")

	_, err = oracle.VideoPlacement(tagged([]int64{0, 40}, 0, 0), tr)
	assert.ErrorContains(t, err, "tags not increasing", "a frame written twice")
}

func TestVideoPlacementKeepsBothErrorSigns(t *testing.T) {
	// Block 1 plays 0.9 ms after its RTP time and block 2 1 ms before. The
	// larger error must not hide the other.
	tr := vp8Trace(
		vp8Read(1, 0, 0),
		vp8Read(2, 8919, 99100),
		vp8Read(3, 18000, 200000),
	)

	p, err := oracle.VideoPlacement(tagged([]int64{0, 100, 199}, 0, 8919, 18000), tr)
	require.NoError(t, err)
	assert.InDelta(t, 0.9, p.TimelineMaxMs, 1e-9)
	assert.Equal(t, 1, p.TimelineMaxAt)
	assert.InDelta(t, -1, p.TimelineMinMs, 1e-9)
	assert.Equal(t, 2, p.TimelineMinAt)
}

func TestVideoPlacementSkipsEmptyProbes(t *testing.T) {
	// pcap-extract writes a padding-only probe with plen 0 and no padding
	// bit. Its timestamp is no frame, so it is not counted as dropped.
	tr := fiveFrames()
	probe := corpus.Record{T: corpus.RecordRTP, AtUs: 900000, Seq: 0, TS: 0xFFFFF000, PT: 96}
	tr.Records = append([]corpus.Record{probe}, tr.Records...)

	p, err := oracle.VideoPlacement(tagged([]int64{0, 40, 80, 120, 160}, 0, 3600, 7200, 10800, 14400), tr)
	require.NoError(t, err)
	assert.Equal(t, 5, p.Frames)
	assert.Equal(t, 0, p.Dropped)
}

func TestVideoPlacementIdealStart(t *testing.T) {
	// Frames 0 and 1 are read 300 ms late, and frames 2 and 3 260.5 ms
	// late, which sets the ideal start at 260.5 ms.
	tr := vp8Trace(
		vp8Read(1, 0, 300000),
		vp8Read(2, 3600, 340000),
		vp8Read(3, 7200, 340500),
		vp8Read(4, 10800, 380500),
	)

	p, err := oracle.VideoPlacement(tagged([]int64{0, 40, 80, 120}, 0, 3600, 7200, 10800), tr)
	require.NoError(t, err)
	assert.InDelta(t, 260500, p.IdealStartUs, 1e-6)
}
