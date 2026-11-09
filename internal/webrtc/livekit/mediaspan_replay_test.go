package livekit

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
)

// PTS are whole ms, so the span can start up to 1 ms away from the read of
// the first frame written.
const mediaSpanTolerance = time.Millisecond

// mediaSpanViolations checks the recorder's media span against a replay of
// one trace. The span must cover the file's media. It must start at startUs
// when set, else at the read of the first frame written, less the lag
// oracle's backlog for Opus.
//
// For VP8 that frame is taken to be the first key frame, by timestamp,
// whose packets are all read. That holds while the jitter buffer releases
// it. A VP8 seed whose buffer drops that frame pins its start in its expect
// file's mediaStartUs. So does one whose later frames, inside the camera's
// 2 s window, arrived sooner than their PTS imply.
func mediaSpanViolations(r replayResult, traces []*corpus.Trace, startUs *float64) []string {
	if r.webm == nil || len(traces) != 1 {
		return nil
	}

	var track *oracle.Track
	for _, tr := range r.webm.Tracks {
		track = tr
	}

	if track == nil || track.Blocks == 0 {
		if r.spanOK {
			return []string{"a media span without a frame written"}
		}

		return nil
	}

	if !r.spanOK {
		return []string{"no media span after writing frames"}
	}

	var v []string
	media := time.Duration(track.LastPTS-track.FirstPTS) * time.Millisecond
	if d := r.span.End.Sub(r.span.Start); d < media || d > media+time.Second {
		v = append(v, fmt.Sprintf("media span lasts %v, the file's media %v", d, media))
	}

	var firstUs float64
	switch {
	case startUs != nil:
		firstUs = *startUs
	case track.Codec == oracle.CodecOpus && r.audioLag != nil && r.audioLagErr == nil:
		firstUs = float64(firstFrameRead(traces[0], nil)) - r.audioLag.StartMs*1000
	case track.Codec == oracle.CodecVP8:
		firstUs = float64(firstFrameRead(traces[0], wholeKeyFrame))
	default:
		return v
	}

	want := r.readStart.Add(time.Duration(firstUs * float64(time.Microsecond)))
	if d := r.span.Start.Sub(want); d.Abs() > mediaSpanTolerance {
		v = append(v, fmt.Sprintf("media span starts %v from its expected start, %.0f µs", d, firstUs))
	}

	return v
}

// firstFrameRead returns the earliest read, in µs, of the first frame by
// timestamp that whole accepts, or of the first one if whole is nil.
func firstFrameRead(tr *corpus.Trace, whole func([]corpus.Record) bool) int64 {
	type frame struct {
		ts   int64 // unwrapped
		recs []corpus.Record
	}

	var (
		frames []*frame
		index  = map[int64]*frame{}
		ext    int64
		prev   uint32
	)

	for _, r := range tr.Records {
		if r.T != corpus.RecordRTP || r.Pad {
			continue
		}

		if len(frames) > 0 {
			ext += int64(int32(r.TS - prev))
		}
		prev = r.TS

		f := index[ext]
		if f == nil {
			f = &frame{ts: ext}
			index[ext] = f
			frames = append(frames, f)
		}
		f.recs = append(f.recs, r)
	}

	slices.SortFunc(frames, func(a, b *frame) int { return cmp.Compare(a.ts, b.ts) })

	for _, f := range frames {
		if whole == nil || whole(f.recs) {
			return slices.MinFunc(f.recs, func(a, b corpus.Record) int { return cmp.Compare(a.AtUs, b.AtUs) }).AtUs
		}
	}

	return -1
}

// wholeKeyFrame reports whether recs, one frame's reads, hold a key frame's
// packets from its first to its marked last, none missing.
func wholeKeyFrame(recs []corpus.Record) bool {
	seqs := map[uint16]bool{}
	var first, last uint16
	var hasFirst, hasLast bool

	for _, r := range recs {
		seqs[r.Seq] = true
		if recorder.IsVP8KeyFrame(r.Packet()) {
			first, hasFirst = r.Seq, true
		}
		if r.M {
			last, hasLast = r.Seq, true
		}
	}

	if !hasFirst || !hasLast {
		return false
	}

	for s := first; s != last+1; s++ {
		if !seqs[s] {
			return false
		}
	}

	return true
}
