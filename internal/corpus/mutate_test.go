package corpus

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schedule is 3 frames of 2 packets at 10 ms intervals, packets 1 ms apart, then eof.
func schedule() []Record {
	var recs []Record
	for f := range 3 {
		for j := range 2 {
			recs = append(recs, Record{
				T: RecordRTP, AtUs: int64(f*10000 + j*1000), Seq: uint16(100 + f*2 + j),
				TS: uint32(1000 + f*900), M: j == 1, PT: 96, PLen: 20, Pay: []byte{byte(f)},
			})
		}
	}

	return append(recs, Record{T: RecordEOF, AtUs: 30000})
}

func seqs(recs []Record) []uint16 {
	out := make([]uint16, 0, len(recs))
	for _, r := range recs {
		if r.T == RecordRTP {
			out = append(out, r.Seq)
		}
	}

	return out
}

func times(recs []Record) []int64 {
	out := make([]int64, len(recs))
	for i, r := range recs {
		out[i] = r.AtUs
	}

	return out
}

// apply runs ops, checks the input is untouched and the result validates.
func apply(t *testing.T, ops ...Op) []Record {
	t.Helper()

	in := schedule()
	out, err := Apply(in, ops...)
	require.NoError(t, err)
	assert.Equal(t, schedule(), in, "the input schedule is not modified")
	require.NoError(t, validateRecords(out))

	return out
}

func TestDropSeqs(t *testing.T) {
	out := apply(t, Retransmit(103, 25000), DropSeqs(101, 103))

	assert.Equal(t, []uint16{100, 102, 104, 105, 103}, seqs(out), "the rtx copy of 103 survives")
	assert.True(t, out[4].RTX)
}

func TestRetransmitAndDuplicate(t *testing.T) {
	out := apply(t, Retransmit(101, 12500), Duplicate(100, 30000))

	assert.Equal(t, []uint16{100, 101, 102, 103, 101, 104, 105, 100}, seqs(out))
	assert.Equal(t, []int64{0, 1000, 10000, 11000, 12500, 20000, 21000, 30000, 30000}, times(out))

	rtx, dup := out[4], out[7]
	assert.True(t, rtx.RTX)
	assert.False(t, rtx.Dup)
	assert.Equal(t, []byte{0}, rtx.Pay)
	assert.True(t, dup.Dup)
	assert.False(t, dup.RTX)
	assert.Equal(t, RecordEOF, out[8].T, "a copy at the eof time goes before the eof")

	_, err := Apply(schedule(), Retransmit(999, 30000))
	assert.ErrorContains(t, err, "no packet 999")
	_, err = Apply(schedule(), Retransmit(104, 15000))
	assert.ErrorContains(t, err, "no packet 104 sent at or before 15000", "a copy cannot precede its original")
	_, err = Apply(schedule(), Duplicate(100, 40000))
	assert.ErrorContains(t, err, "after eof")
}

func TestRetransmitPicksTheLatestSendAfterAWrap(t *testing.T) {
	recs := []Record{
		{T: RecordRTP, AtUs: 0, Seq: 7, TS: 1},
		{T: RecordRTP, AtUs: 100, Seq: 7, TS: 2},
	}

	out, err := Apply(recs, Retransmit(7, 150))
	require.NoError(t, err)
	assert.Equal(t, uint32(2), out[2].TS)
}

func TestReorder(t *testing.T) {
	run := func(seed uint64) []Record {
		return apply(t, Reorder(2, 0.5, rand.New(rand.NewPCG(seed, 0))))
	}

	a := run(1)
	assert.Equal(t, a, run(1), "the same seed gives the same order")
	assert.Equal(t, times(schedule()), times(a), "read times stay in place")
	assert.ElementsMatch(t, seqs(schedule()), seqs(a))
	assert.Equal(t, RecordEOF, a[len(a)-1].T)

	moved := false
	for seed := range uint64(20) {
		out := run(seed)
		if !slices.Equal(seqs(out), seqs(schedule())) {
			moved = true
		}

		// A packet is delayed past at most depth later packets.
		for slot, r := range out[:len(out)-1] {
			assert.LessOrEqual(t, slot-int(r.Seq-100), 2, "seed %d: %v", seed, seqs(out))
		}
	}
	assert.True(t, moved, "p=0.5 reorders some seed")

	assert.Equal(t, schedule(), apply(t, Reorder(3, 0, rand.New(rand.NewPCG(1, 0)))))

	all := apply(t, Reorder(1, 1, rand.New(rand.NewPCG(1, 0))))
	assert.Equal(t, []uint16{100, 101, 102, 103, 104, 105}, seqs(all), "delaying every packet by one keeps the order")

	_, err := Apply(schedule(), Reorder(0, 0.5, rand.New(rand.NewPCG(1, 0))))
	assert.Error(t, err)
	_, err = Apply(schedule(), Reorder(1, 0.5, nil))
	assert.Error(t, err)
}

func TestStartAt(t *testing.T) {
	out := apply(t, StartAt(3))

	assert.Equal(t, []uint16{103, 104, 105}, seqs(out))
	assert.Equal(t, []int64{0, 9000, 10000, 19000}, times(out))
	assert.Equal(t, schedule(), apply(t, StartAt(0)))

	_, err := Apply(schedule(), StartAt(6))
	assert.Error(t, err)
}

func TestEOFAt(t *testing.T) {
	out := apply(t, EOFAt(11000))
	assert.Equal(t, []uint16{100, 101, 102, 103}, seqs(out))
	assert.Equal(t, Record{T: RecordEOF, AtUs: 11000}, out[len(out)-1])

	out = apply(t, EOFAt(50000))
	assert.Equal(t, seqs(schedule()), seqs(out))
	assert.Equal(t, int64(50000), out[len(out)-1].AtUs)

	_, err := Apply(schedule(), EOFAt(-1))
	assert.Error(t, err)
}

func TestMediaClock(t *testing.T) {
	// At half speed, the 10 ms from 10000 us take 20 ms, and what follows
	// comes 10 ms later.
	out := apply(t, MediaClock(10000, 20000, 0.5))
	assert.Equal(t, seqs(schedule()), seqs(out))
	assert.Equal(t, []int64{0, 1000, 10000, 12000, 30000, 31000, 40000}, times(out))

	for i, r := range out {
		assert.Equal(t, schedule()[i].TS, r.TS, "record %d keeps its RTP timestamp", i)
	}

	for _, op := range []Op{MediaClock(-1, 10, 0.9), MediaClock(10, 10, 0.9), MediaClock(0, 10, 0)} {
		_, err := Apply(schedule(), op)
		assert.Error(t, err)
	}
}

func TestDelay(t *testing.T) {
	// The records sent from 10 to 20 ms wait 15 ms more; the one sent at
	// 20 ms queues behind them.
	out := apply(t, Delay(10000, 20000, 15000))
	assert.Equal(t, seqs(schedule()), seqs(out))
	assert.Equal(t, []int64{0, 1000, 25000, 26000, 26000, 26000, 30000}, times(out))

	for _, op := range []Op{Delay(-1, 10, 1), Delay(10, 10, 1), Delay(0, 10, -1)} {
		_, err := Apply(schedule(), op)
		assert.Error(t, err)
	}
}

func TestSenderReports(t *testing.T) {
	// A sender at half speed: 20 ms frames 40 ms apart, a report every 50 ms.
	tr, err := GenerateOpus(OpusOptions{Frames: 5, StartTS: 0xFFFFFFFF - 1000})
	require.NoError(t, err)
	recs, err := Apply(tr.Records, MediaClock(0, 100000, 0.5), EOFAt(200000), SenderReports(50000, 48000))
	require.NoError(t, err)
	require.NoError(t, validateRecords(recs))

	var srs []Record
	for _, r := range recs {
		if r.T == RecordSR {
			srs = append(srs, r)
		}
	}

	start := tr.Records[0].TS
	require.Len(t, srs, 3, "at 50, 100 and 150 ms, up to the last packet at 160 ms")
	for i, want := range []struct {
		atUs  int64
		ticks uint32 // past the first packet's timestamp
	}{
		{50000, 960 + 480},     // packet 1, sent at 40 ms, plus 10 ms
		{100000, 1920 + 960},   // packet 2, at 80 ms, plus 20 ms
		{150000, 3*960 + 1440}, // packet 3, at 120 ms, plus 30 ms
	} {
		assert.Equal(t, want.atUs, srs[i].AtUs, "report %d", i)
		assert.Equal(t, SenderNTP(want.atUs), srs[i].NTP, "report %d", i)
		assert.Equal(t, start+want.ticks, srs[i].TS, "report %d", i)
	}

	assert.Equal(t, SenderNTP(0)+1<<31, SenderNTP(500000), "half a second is half of the fraction")

	_, err = Apply(tr.Records, SenderReports(0, 48000))
	assert.Error(t, err)
}

func TestApplyOnGeneratedSchedule(t *testing.T) {
	tr, err := GenerateVP8(VP8Options{FPS: 30, Frames: 60, KeyInterval: 30, KeyPackets: 6, SSRC: 9})
	require.NoError(t, err)

	key := tr.Records[6+29*2] // frame 30, the second key frame
	tr.Records, err = Apply(tr.Records,
		Retransmit(key.Seq+1, 1050000),
		DropSeqs(key.Seq+1),
		Reorder(3, 0.1, rand.New(rand.NewPCG(7, 0))),
		EOFAt(2500000),
	)
	require.NoError(t, err)
	assert.NoError(t, tr.Validate())
}
