package oracle

import (
	"fmt"
	"math"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
)

// Placement compares when a VP8 track's blocks play with when their
// frames were read. Each block names its frame by the RTP timestamp
// tagged into it with corpus.TagFrame. Matching by order instead pairs a
// block with the wrong read once a frame before it is dropped.
type Placement struct {
	Frames  int // distinct frames read
	Placed  int // blocks, each paired with a frame read
	Dropped int // frames read but not written
	// A block's timeline error is its PTS since the first block, less its
	// RTP time since the first block's. These are the lowest and the
	// highest, and their block indexes.
	TimelineMinMs, TimelineMaxMs float64
	TimelineMinAt, TimelineMaxAt int
	// IdealStartUs is the latest media start at which no block plays
	// after its frame was read, in µs on the trace's atUs timeline. It is
	// the least, over blocks, of the read less the PTS since the first
	// block.
	IdealStartUs float64
}

// VideoPlacement pairs the blocks of t, a VP8 track, with the frames read
// in tr by their tags. It fails on a block with no tag, a tag no frame read
// has, or tags that do not increase in file order.
func VideoPlacement(t *Track, tr *corpus.Trace) (Placement, error) {
	frames := map[uint32]readFrame{}
	for _, f := range framesRead(tr) {
		frames[f.ts] = f
	}

	p := Placement{Frames: len(frames), IdealStartUs: math.Inf(1)}
	if len(t.TagOK) != t.Blocks {
		return Placement{}, fmt.Errorf("%d blocks, %d tags read", t.Blocks, len(t.TagOK))
	}

	rate := float64(tr.Header.ClockRate)
	var (
		first, last readFrame
		firstPTS    int64
	)
	for k := range t.Blocks {
		if !t.TagOK[k] {
			return Placement{}, fmt.Errorf("block %d (PTS %d ms) has no frame tag", k, t.PTS[k])
		}

		f, ok := frames[t.Tags[k]]
		if !ok {
			return Placement{}, fmt.Errorf("block %d (PTS %d ms): no frame read at timestamp %d", k, t.PTS[k], t.Tags[k])
		}

		if k == 0 {
			first, firstPTS = f, t.PTS[k]
			p.TimelineMinAt, p.TimelineMaxAt = k, k
		} else if f.ext <= last.ext {
			return Placement{}, fmt.Errorf("block %d (PTS %d ms): tags not increasing, timestamp %d after %d",
				k, t.PTS[k], t.Tags[k], t.Tags[k-1])
		}
		last = f

		tau := float64(f.ext-first.ext) * 1000 / rate
		e := float64(t.PTS[k]-firstPTS) - tau
		if e < p.TimelineMinMs {
			p.TimelineMinMs, p.TimelineMinAt = e, k
		}
		if e > p.TimelineMaxMs {
			p.TimelineMaxMs, p.TimelineMaxAt = e, k
		}

		rel := float64(t.PTS[k] - t.PTS[0])
		p.IdealStartUs = min(p.IdealStartUs, float64(f.at)-rel*1000)
		p.Placed++
	}

	p.Dropped = p.Frames - p.Placed
	if p.Placed == 0 {
		p.IdealStartUs = 0
	}

	return p, nil
}
