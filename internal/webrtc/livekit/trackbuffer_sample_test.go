package livekit

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
)

// sampleView is a released sample's sequence numbers and follows flag.
type sampleView struct {
	seqs    []uint16
	follows bool
}

func viewSamples(samples []bufferSample) []sampleView {
	views := make([]sampleView, 0, len(samples))
	for _, s := range samples {
		v := sampleView{follows: s.follows}
		for _, p := range s.packets {
			v.seqs = append(v.seqs, p.SequenceNumber)
		}
		views = append(views, v)
	}

	return views
}

func pushSamplesAll(b *trackBuffer, packets ...*rtp.Packet) []sampleView {
	var views []sampleView
	for _, p := range packets {
		views = append(views, viewSamples(b.pushSamples(p, time.Now()))...)
	}

	return views
}

func TestTrackBufferSamplesFollowWithoutLoss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, _ := newCountingBuffer(recorder.CodecVP8, testLatency, 0)
		defer b.close()

		got := pushSamplesAll(b,
			vp8Packet(1, 3000, true, false), vp8Packet(2, 3000, false, true),
			vp8Packet(3, 6000, true, true),
			vp8Packet(4, 9000, true, false), vp8Packet(5, 9000, false, true))

		assert.Equal(t, []sampleView{
			{seqs: []uint16{1, 2}, follows: false},
			{seqs: []uint16{3}, follows: true},
			{seqs: []uint16{4, 5}, follows: true},
		}, got)
	})
}

func TestTrackBufferSamplesFollowPaddingAndEmptyPackets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, _ := newCountingBuffer(recorder.CodecVP8, testLatency, 0)
		defer b.close()

		padding := func(seq uint16) *rtp.Packet {
			return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: 3000, Padding: true}}
		}
		empty := &rtp.Packet{Header: rtp.Header{SequenceNumber: 5, Timestamp: 3000}}

		got := pushSamplesAll(b,
			vp8Packet(1, 3000, true, false), vp8Packet(2, 3000, false, true),
			padding(3), padding(4), empty,
			vp8Packet(6, 6000, true, true))

		assert.Equal(t, []sampleView{
			{seqs: []uint16{1, 2}, follows: false},
			{seqs: []uint16{6}, follows: true},
		}, got)
	})
}

func TestTrackBufferSampleAfterALostFrameDoesNotFollow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, _ := newCountingBuffer(recorder.CodecVP8, testLatency, 0)
		defer b.close()

		// Frame 3-4 never arrives.
		got := pushSamplesAll(b,
			vp8Packet(1, 3000, true, false), vp8Packet(2, 3000, false, true),
			vp8Packet(5, 9000, true, true))
		assert.Equal(t, []sampleView{{seqs: []uint16{1, 2}}}, got)

		time.Sleep(testLatency + time.Millisecond)
		synctest.Wait()

		got = pushSamplesAll(b, vp8Packet(6, 12000, true, true))
		assert.Equal(t, []sampleView{
			{seqs: []uint16{5}, follows: false},
			{seqs: []uint16{6}, follows: true},
		}, got)
	})
}

func TestTrackBufferSampleAfterADroppedPartialFrameDoesNotFollow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, _ := newCountingBuffer(recorder.CodecVP8, testLatency, 0)
		defer b.close()

		// 4, the middle of frame 3-5, is lost: the buffer drops 3 and 5.
		got := pushSamplesAll(b,
			vp8Packet(1, 3000, true, true),
			vp8Packet(3, 6000, true, false), vp8Packet(5, 6000, false, true),
			vp8Packet(6, 9000, true, true))
		assert.Equal(t, []sampleView{{seqs: []uint16{1}}}, got)

		time.Sleep(testLatency + time.Millisecond)
		synctest.Wait()

		got = pushSamplesAll(b, vp8Packet(7, 12000, true, true))
		assert.Equal(t, []sampleView{
			{seqs: []uint16{6}, follows: false},
			{seqs: []uint16{7}, follows: true},
		}, got)
	})
}

func TestTrackBufferSampleAfterASequenceJumpDoesNotFollow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, _ := newCountingBuffer(recorder.CodecVP8, testLatency, 0)
		defer b.close()

		got := pushSamplesAll(b,
			vp8Packet(1, 3000, true, true),
			vp8Packet(9000, 6000, true, true),
			vp8Packet(9001, 9000, true, true))

		assert.Equal(t, []sampleView{
			{seqs: []uint16{1}, follows: false},
			{seqs: []uint16{9000}, follows: false},
			{seqs: []uint16{9001}, follows: true},
		}, got)
	})
}

func TestTrackBufferSamplesFollowAcrossTheSequenceWrap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, _ := newCountingBuffer(recorder.CodecOpus, audioJitterLatency, 0)
		defer b.close()

		padding := &rtp.Packet{Header: rtp.Header{SequenceNumber: 0, Timestamp: 960, Padding: true}}
		got := pushSamplesAll(b, opusPacket(65534), opusPacket(65535), padding, opusPacket(1))

		assert.Equal(t, []sampleView{
			{seqs: []uint16{65534}, follows: false},
			{seqs: []uint16{65535}, follows: true},
			{seqs: []uint16{1}, follows: true},
		}, got)
	})
}

// A track can go a whole sequence cycle without padding. A frame lost at a
// sequence number that held padding a cycle earlier is still a loss.
func TestTrackBufferForgetsThePaddingOfTheLastSequenceCycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, _ := newCountingBuffer(recorder.CodecVP8, testLatency, 0)
		defer b.close()

		ts := uint32(0)
		frame := func(seq uint16) *rtp.Packet {
			ts += 3000

			return vp8Packet(seq, ts, true, true)
		}

		for seq := uint16(1); seq < 100; seq++ {
			b.pushSamples(frame(seq), time.Now())
		}
		b.pushSamples(&rtp.Packet{Header: rtp.Header{SequenceNumber: 100, Timestamp: ts, Padding: true}}, time.Now())
		// Up to 99 again, a cycle later. Then frame 100 is lost.
		for seq := uint16(101); seq != 100; seq++ {
			b.pushSamples(frame(seq), time.Now())
		}

		assert.Empty(t, pushSamplesAll(b, frame(101)))
		time.Sleep(testLatency + time.Millisecond)
		synctest.Wait()

		assert.Equal(t, []sampleView{
			{seqs: []uint16{101}, follows: false},
			{seqs: []uint16{102}, follows: true},
		}, pushSamplesAll(b, frame(102)))
	})
}

func TestTrackBufferDuplicatesDoNotBreakSamples(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, _ := newCountingBuffer(recorder.CodecVP8, testLatency, 0)
		defer b.close()

		got := pushSamplesAll(b,
			vp8Packet(1, 3000, true, true),
			vp8Packet(1, 3000, true, true),
			vp8Packet(2, 6000, true, true))

		assert.Equal(t, []sampleView{
			{seqs: []uint16{1}, follows: false},
			{seqs: []uint16{2}, follows: true},
		}, got)
	})
}

func TestTrackBufferFlushSamplesKeepsFollows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, _ := newCountingBuffer(recorder.CodecVP8, videoJitterLatency, lossKeyframeInterval)
		defer b.close()

		assert.Equal(t, []sampleView{{seqs: []uint16{1, 2}}}, pushSamplesAll(b,
			vp8Packet(1, 3000, true, false), vp8Packet(2, 3000, false, true)))
		// 4 never arrives: frame 3-5 is dropped at the flush, 6 is released.
		assert.Empty(t, pushSamplesAll(b,
			vp8Packet(3, 6000, true, false), vp8Packet(5, 6000, false, true),
			vp8Packet(6, 9000, true, true), vp8Packet(7, 12000, true, true)))

		assert.Equal(t, []sampleView{
			{seqs: []uint16{6}, follows: false},
			{seqs: []uint16{7}, follows: true},
		}, viewSamples(b.flushSamples()))
	})
}

func TestSeqWindowContains(t *testing.T) {
	var w seqWindow
	assert.False(t, w.contains(10), "nothing recorded")

	w.add(10)
	assert.True(t, w.contains(10))
	assert.False(t, w.contains(11))
	// The same bit, one window ahead of the newest number recorded.
	assert.False(t, w.contains(10+seqWindowSize))

	w.add(65535)
	assert.True(t, w.contains(65535), "11 behind 10, across the wrap")

	w.add(10 + seqResetDistance)
	assert.True(t, w.contains(10+seqResetDistance))
	assert.False(t, w.contains(10), "a jump of seqResetDistance or more starts over")
}
