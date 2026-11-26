package livekit

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// h264Media is the byte the live H.264 tests put where media would be.
const h264Media = 0xa5

func TestH264HeaderPrefix(t *testing.T) {
	sps := corpus.H264SPS(640, 360)
	pps := []byte{0x68, 0xcb, 0x83, 0xcb, 0x20}
	media := bytes.Repeat([]byte{h264Media}, 40)
	cat := func(parts ...[]byte) []byte { return slices.Concat(parts...) }
	size := func(n int) []byte { return binary.BigEndian.AppendUint16(nil, uint16(n)) }
	zeros := func(n int) []byte { return make([]byte, n) }
	idr := cat([]byte{0x65, 0x88, 0x84}, media)
	p := cat([]byte{0x41, 0x9a}, media)
	sei := cat([]byte{0x06}, media)
	params := cat([]byte{0x78}, size(len(sps)), sps, size(len(pps)), pps)

	tests := []struct {
		name string
		pay  []byte
		want []byte
	}{
		{name: "empty", pay: nil, want: nil},
		{name: "SPS", pay: sps, want: sps},
		{name: "PPS", pay: pps, want: pps},
		{name: "IDR slice", pay: idr, want: idr[:9]},
		{name: "short P slice", pay: p[:5], want: p[:5]},
		{name: "SEI", pay: sei, want: sei[:1]},
		{name: "STAP-A of parameter sets", pay: params, want: params},
		{name: "STAP-A ending in an IDR", pay: cat(params, size(len(idr)), idr), want: cat(params, size(len(idr)), idr[:9])},
		{
			name: "STAP-A of two slices", pay: cat([]byte{0x58}, size(len(p)), p, size(len(p)), p),
			want: cat([]byte{0x58}, size(len(p)), p[:9], zeros(len(p)-9), size(len(p)), p[:9]),
		},
		{
			name: "STAP-A with an SEI", pay: cat([]byte{0x78}, size(len(sei)), sei, size(len(p)), p),
			want: cat([]byte{0x78}, size(len(sei)), sei[:1], zeros(len(sei)-1), size(len(p)), p[:9]),
		},
		{name: "STAP-A size past the payload", pay: cat(params, size(90), p), want: cat(params, size(90))},
		{name: "STAP-A zero size", pay: cat([]byte{0x78}, size(0), p), want: cat([]byte{0x78}, size(0))},
		{name: "STAP-A truncated size", pay: cat(params, []byte{0x00}), want: cat(params, []byte{0x00})},
		{name: "FU-A start of an IDR", pay: cat([]byte{0x7c, 0x85}, idr[1:]), want: cat([]byte{0x7c, 0x85}, idr[1:9])},
		{name: "FU-A middle", pay: cat([]byte{0x7c, 0x05}, media), want: []byte{0x7c, 0x05}},
		{name: "FU-A end", pay: cat([]byte{0x7c, 0x45}, media), want: []byte{0x7c, 0x45}},
		{name: "FU-A of an SPS", pay: cat([]byte{0x7c, 0x87}, sps[1:]), want: cat([]byte{0x7c, 0x87}, sps[1:])},
		{name: "FU-A start of an SEI", pay: cat([]byte{0x7c, 0x86}, media), want: []byte{0x7c, 0x86}},
		{name: "FU-A without its header", pay: []byte{0x7c}, want: []byte{0x7c}},
		{name: "FU-B", pay: cat([]byte{0x7d, 0x85}, media), want: []byte{0x7d}},
		{name: "STAP-B", pay: cat([]byte{0x79}, media), want: []byte{0x79}},
		{name: "LiveKit padding", pay: zeros(255), want: []byte{0x00}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pay := bytes.Clone(tt.pay)
			got := h264HeaderPrefix(pay)
			if len(tt.want) == 0 {
				assert.Empty(t, got)
			} else {
				assert.Equal(t, tt.want, got)
			}
			assert.Equal(t, tt.pay, pay, "the packet is left as it is")
		})
	}
}

// h264WithMedia is r's payload with media past the header prefix that the
// generator wrote.
func h264WithMedia(r corpus.Record) []byte {
	pay := r.Packet().Payload
	for j := len(r.Pay); j < len(pay); j++ {
		pay[j] = h264Media
	}

	return pay
}

// h264Live is an H.264 camera whose payloads carry media past their slice
// headers. Frame 5's slices share a STAP-A, and a padding packet follows
// frame 8's marker. Frame 30 loses its first slice, so the writer resumes at
// key frame 40.
func h264Live(t *testing.T) *corpus.Trace {
	t.Helper()

	tr, err := corpus.GenerateH264(corpus.H264Options{
		FPS: 30, Frames: 60, KeyInterval: 20, Slices: 2, KeySlicePackets: 3, DeltaPLen: 60,
		SpreadUs: 2000, StartSeq: 2000, StartTS: 90000, SSRC: 4242,
	})
	require.NoError(t, err)

	frame := func(r corpus.Record) uint32 { return (r.TS - 90000) / 3000 }
	var (
		recs []corpus.Record
		seq  = uint16(2000)
		lost uint16
	)
	for i, r := range tr.Records {
		switch {
		case frame(r) == 5 && !r.M:
			continue
		case frame(r) == 5:
			stap := []byte{0x58}
			for _, nal := range [][]byte{h264WithMedia(tr.Records[i-1]), h264WithMedia(r)} {
				stap = binary.BigEndian.AppendUint16(stap, uint16(len(nal)))
				stap = append(stap, nal...)
			}
			r.Pay, r.PLen = stap, len(stap)
		default:
			r.Pay = h264WithMedia(r)
		}

		if frame(r) == 30 && lost == 0 {
			lost = seq
		}
		r.Seq = seq
		seq++
		recs = append(recs, r)

		// LiveKit pads only at a frame boundary.
		if frame(r) == 8 && r.M {
			recs = append(recs, corpus.Record{T: corpus.RecordRTP, AtUs: r.AtUs, Seq: seq, TS: r.TS, PT: r.PT, Pad: true, PLen: 255})
			seq++
		}
	}

	tr.Records, err = corpus.Apply(recs, corpus.DropSeqs(lost), corpus.EOFAt(recs[len(recs)-1].AtUs+50000))
	require.NoError(t, err)
	require.NoError(t, tr.Validate())

	return tr
}

// h264Slices counts the slice NAL units an H.264 payload starts.
func h264Slices(p []byte) int {
	if len(p) > 0 && p[0]&0x1f == 24 {
		n := 0
		for rest := p[1:]; len(rest) > 2; {
			size := int(binary.BigEndian.Uint16(rest))
			if size == 0 || 2+size > len(rest) {
				break
			}
			if t := rest[2] & 0x1f; t == 1 || t == 5 {
				n++
			}
			rest = rest[2+size:]
		}

		return n
	}

	if corpus.H264SliceStart(p) {
		return 1
	}

	return 0
}

func blockKeys(t *testing.T, file []byte) []bool {
	t.Helper()

	path := filepath.Join(t.TempDir(), "blocks.mkv")
	require.NoError(t, os.WriteFile(path, file, 0o600))
	_, blocks, err := oracle.ReadBlocks(path)
	require.NoError(t, err)

	keys := make([]bool, 0, len(blocks))
	for _, b := range blocks {
		keys = append(keys, b.Key)
	}

	return keys
}

// A header capture of an H.264 camera keeps no more than 8 media bytes per
// slice, and replays to the blocks the live session wrote.
func TestTraceCaptureH264(t *testing.T) {
	in := h264Live(t)
	for _, set := range [][]byte{corpus.H264SPS(640, 360), {0x68, 0xcb, 0x83, 0xcb, 0x20}} {
		require.NotContains(t, set, byte(h264Media), "the media byte must tell media apart")
	}

	var live, again captureRun
	synctest.Test(t, func(t *testing.T) {
		live = replayCaptured(t, headerCapture(), in)
	})
	got := live.read(t, 0)
	synctest.Test(t, func(t *testing.T) {
		again = replayCaptured(t, headerCapture(), got)
	})

	assert.Equal(t, corpus.CodecH264, got.Header.Codec)
	assert.Equal(t, uint32(90000), got.Header.ClockRate)
	assert.Equal(t, "bbb-webrtc-recorder devel traceCapture/header", got.Header.Generator)

	captured := recordsOf(got, corpus.RecordRTP)
	require.Len(t, captured, len(recordsOf(in, corpus.RecordRTP)))
	for i, r := range captured {
		assert.Equal(t, in.Records[i].PLen, r.PLen, "record %d keeps its length", i)
		assert.LessOrEqual(t, bytes.Count(r.Pay, []byte{h264Media}), 8*h264Slices(in.Records[i].Pay),
			"record %d: % x", i, r.Pay)
	}

	keys := blockKeys(t, live.r.file)
	require.Len(t, keys, 50, "frames 0-29, then from key frame 40")
	for i, key := range keys {
		assert.Equal(t, i == 0 || i == 20 || i == 30, key, "block %d", i)
	}
	assert.Equal(t, keys, blockKeys(t, again.r.file), "the capture replays to the same blocks")
	assert.Equal(t, live.r.packets, again.r.packets)
	assert.Equal(t, microsOf(live.r.requests()), microsOf(again.r.requests()), "the same keyframe requests")
}
