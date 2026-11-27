package corpus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSwitch(t *testing.T) {
	// The schedule is cut after the first packet of frame 1, seq 102, and
	// another layer's two frames follow it.
	next := []Record{
		{T: RecordRTP, AtUs: 500000, Seq: 7000, TS: 50000, PT: 96, PLen: 30, Pay: []byte{9}},
		{T: RecordRTP, AtUs: 501000, Seq: 7001, TS: 50000, M: true, PT: 96, PLen: 30},
		{T: RecordRTP, AtUs: 503000, Seq: 7000, TS: 50000, PT: 96, PLen: 30, RTX: true},
		{T: RecordRTP, AtUs: 540000, Seq: 7002, TS: 53600, M: true, PT: 96, PLen: 30},
		{T: RecordEOF, AtUs: 600000},
	}

	out := apply(t, Switch(102, next))
	assert.Equal(t, []uint16{100, 101, 102, 103, 104, 103, 105}, seqs(out))
	assert.Equal(t, []int64{0, 1000, 10000, 10000, 11000, 13000, 50000}, times(out), "next keeps its spacing from seq's read")

	var ts []uint32
	for _, r := range out {
		ts = append(ts, r.TS)
	}
	assert.Equal(t, []uint32{1000, 1000, 1900, 1901, 1901, 1901, 5501}, ts, "next starts one tick after seq's timestamp")
	assert.Equal(t, []byte{9}, out[3].Pay)
	assert.True(t, out[5].RTX, "copies are kept")
	assert.Equal(t, RecordRTP, out[len(out)-1].T, "the eofs are dropped")

	_, err := Apply(schedule(), Switch(99, next))
	require.Error(t, err)
	_, err = Apply(schedule(), Switch(102, nil))
	require.Error(t, err)
}

func TestPadding(t *testing.T) {
	// Two padding packets after frame 0's marker, seq 101.
	out := apply(t, Padding(101, 2, 255))
	assert.Equal(t, []uint16{100, 101, 102, 103, 104, 105, 106, 107}, seqs(out))
	assert.Equal(t, []int64{0, 1000, 1000, 1000, 10000, 11000, 20000, 21000, 30000}, times(out))

	for _, r := range out[2:4] {
		assert.Equal(t, Record{T: RecordRTP, AtUs: 1000, Seq: r.Seq, TS: 1000, PT: 96, Pad: true, PLen: 255}, r)
	}
	assert.Equal(t, []byte{1}, out[4].Pay, "frame 1 moved up by two sequence numbers")

	_, err := Apply(schedule(), Padding(99, 1, 255))
	require.Error(t, err)
	_, err = Apply(schedule(), Padding(101, 0, 255))
	require.Error(t, err)
}

func TestH264BlankClose(t *testing.T) {
	out := apply(t, H264BlankClose(3))
	require.Len(t, out, 6+3)
	assert.Equal(t, []uint16{100, 101, 102, 103, 104, 105, 106, 107, 108}, seqs(out))

	for k, r := range out[6:] {
		assert.Equal(t, uint32(1000+2*900+3000*(k+1)), r.TS)
		assert.Equal(t, int64(21000+[]int{33333, 66667, 100000}[k]), r.AtUs, "30 fps after the last read")
		assert.True(t, r.M)
		assert.Equal(t, len(r.Pay), r.PLen)
		assert.True(t, H264KeyFrameStart(r.Pay))
		assert.True(t, H264HoldsSlice(r.Pay))
		assert.Equal(t, byte(0x18), r.Pay[0], "LiveKit's STAP-A header")
	}

	_, err := Apply(schedule(), H264BlankClose(0))
	require.Error(t, err)
}

func TestH264BlankCloseInsideAFrame(t *testing.T) {
	// The last frame is cut after its first packet, seq 104.
	out, err := Apply(schedule(), DropSeqs(105), H264BlankClose(2))
	require.NoError(t, err)
	require.NoError(t, validateRecords(out))
	assert.Equal(t, []uint16{100, 101, 102, 103, 104, 105, 106, 107}, seqs(out))

	end := out[5]
	assert.Equal(t, uint32(1000+2*900), end.TS, "a blank packet ends the cut frame, at its timestamp")
	assert.True(t, end.M)
	assert.Equal(t, H264BlankFrame(), end.Pay)
	assert.Equal(t, out[6].AtUs, end.AtUs, "read with the first blank frame")
	for k, r := range out[6:] {
		assert.Equal(t, uint32(1000+2*900+3000*(k+1)), r.TS)
		assert.Equal(t, int64(20000+[]int{33333, 66667}[k]), r.AtUs, "30 fps after the last read")
	}
}

func TestPauseH264(t *testing.T) {
	tr, err := GenerateH264(H264Options{FPS: 25, Frames: 6, KeyInterval: 3, KeySlicePackets: 2, StartSeq: 65530})
	require.NoError(t, err)

	// Frames 1 and 2 are not sent; key frame 3 follows frame 0.
	out, err := Apply(tr.Records, PauseH264(40000, 120000))
	require.NoError(t, err)
	require.NoError(t, validateRecords(out))
	assert.Equal(t, []uint16{65530, 65531, 65532, 65533, 65534, 65535, 0, 1}, seqs(out))
	assert.Equal(t, uint32(3*3600), out[3].TS, "the timestamp keeps the sender's clock")
	assert.True(t, H264KeyFrameStart(out[3].Pay))
}
