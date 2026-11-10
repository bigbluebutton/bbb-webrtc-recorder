package oracle

import (
	"cmp"
	"slices"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
)

// readFrame is a frame read from a trace: its RTP timestamp, the same
// unwrapped in read order, and its first read in µs.
type readFrame struct {
	ts  uint32
	ext int64
	at  int64
}

// framesRead returns the distinct frames read in tr, in timestamp order. A
// frame's read is the earliest read of any of its media packets, when the
// recorder sees it arrive.
func framesRead(tr *corpus.Trace) []readFrame {
	var (
		frames []readFrame
		index  = map[int64]int{}
		ext    int64
		prev   uint32
	)

	for _, r := range tr.Records {
		// pcap-extract records a padding-only probe with plen 0, not
		// always with the padding bit. It carries no frame.
		if r.T != corpus.RecordRTP || r.Pad || max(r.PLen, len(r.Pay)) == 0 {
			continue
		}

		if len(frames) > 0 {
			ext += int64(int32(r.TS - prev))
		}
		prev = r.TS

		if i, ok := index[ext]; ok {
			frames[i].at = min(frames[i].at, r.AtUs)

			continue
		}

		index[ext] = len(frames)
		frames = append(frames, readFrame{ts: r.TS, ext: ext, at: r.AtUs})
	}

	slices.SortFunc(frames, func(a, b readFrame) int { return cmp.Compare(a.ext, b.ext) })

	return frames
}
