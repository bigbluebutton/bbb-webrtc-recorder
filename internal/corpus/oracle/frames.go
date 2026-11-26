package oracle

import (
	"cmp"
	"slices"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
)

// readFrame is a frame read from a trace: its RTP timestamp, the same
// unwrapped in read order, and its first read in µs. first and last are its
// lowest and highest sequence numbers.
type readFrame struct {
	ts          uint32
	ext         int64
	at          int64
	first, last int64
}

// framesRead returns the distinct frames read in tr, in timestamp order,
// each at the earliest read of its media packets. It also returns every
// sequence number read, padding included. An H.264 timestamp whose packets
// hold no slice data, such as parameter sets alone, is no frame.
func framesRead(tr *corpus.Trace) ([]readFrame, map[int64]bool) {
	var (
		frames  []readFrame
		index   = map[int64]int{}
		ext     int64
		prev    uint32
		h264    = tr.Header.Codec == corpus.CodecH264
		sliced  = map[int64]bool{}
		seqs    = map[int64]bool{}
		seq     int64
		prevSeq uint16
	)

	for _, r := range tr.Records {
		if r.T != corpus.RecordRTP {
			continue
		}

		if len(seqs) == 0 {
			seq = int64(r.Seq)
		} else {
			seq += int64(int16(r.Seq - prevSeq))
		}
		prevSeq = r.Seq
		seqs[seq] = true

		// pcap-extract records a padding-only probe with plen 0, not
		// always with the padding bit. It carries no frame.
		if r.Pad || max(r.PLen, len(r.Pay)) == 0 {
			continue
		}

		if len(frames) > 0 {
			ext += int64(int32(r.TS - prev))
		}
		prev = r.TS

		// Check the payload as replay reads it, zero-padded to PLen. A STAP-A's
		// last NAL unit can run past the prefix the trace keeps, and r.Pay
		// alone would cut it short.
		if h264 && corpus.H264HoldsSlice(r.Packet().Payload) {
			sliced[ext] = true
		}

		if i, ok := index[ext]; ok {
			frames[i].at = min(frames[i].at, r.AtUs)
			frames[i].first, frames[i].last = min(frames[i].first, seq), max(frames[i].last, seq)

			continue
		}

		index[ext] = len(frames)
		frames = append(frames, readFrame{ts: r.TS, ext: ext, at: r.AtUs, first: seq, last: seq})
	}

	if h264 {
		frames = slices.DeleteFunc(frames, func(f readFrame) bool { return !sliced[f.ext] })
	}

	slices.SortFunc(frames, func(a, b readFrame) int { return cmp.Compare(a.ext, b.ext) })

	return frames, seqs
}
