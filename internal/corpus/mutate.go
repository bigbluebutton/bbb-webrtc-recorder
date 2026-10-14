package corpus

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
)

// Op is a mutation of a sent schedule; it returns a new slice and never
// modifies its input. Mutations model the network, so apply them to sent
// schedules, not to read traces whose RTX timing came from the reader.
type Op func([]Record) ([]Record, error)

// Apply runs ops in order on a copy of recs.
func Apply(recs []Record, ops ...Op) ([]Record, error) {
	out := slices.Clone(recs)

	for i, op := range ops {
		var err error
		if out, err = op(out); err != nil {
			return nil, fmt.Errorf("op %d: %w", i, err)
		}
	}

	return out, nil
}

// DropSeqs removes every original (non-rtx, non-dup) packet whose sequence
// number is in seqs; apply Retransmit first to model a repaired loss.
func DropSeqs(seqs ...uint16) Op {
	drop := make(map[uint16]bool, len(seqs))
	for _, s := range seqs {
		drop[s] = true
	}

	return func(recs []Record) ([]Record, error) {
		return slices.DeleteFunc(slices.Clone(recs), func(r Record) bool {
			return isOriginal(r) && drop[r.Seq]
		}), nil
	}
}

// Retransmit adds an rtx copy of packet seq read at atUs.
func Retransmit(seq uint16, atUs int64) Op {
	return copyAt(seq, atUs, true)
}

// Duplicate adds a dup copy of packet seq read at atUs.
func Duplicate(seq uint16, atUs int64) Op {
	return copyAt(seq, atUs, false)
}

// copyAt copies the latest original send of seq at or before atUs, so a
// sequence number reused after a wrap resolves to the right packet.
func copyAt(seq uint16, atUs int64, rtx bool) Op {
	return func(recs []Record) ([]Record, error) {
		src := -1
		for i, r := range recs {
			if isOriginal(r) && r.Seq == seq && r.AtUs <= atUs {
				src = i
			}
		}

		if src < 0 {
			return nil, fmt.Errorf("no packet %d sent at or before %d us", seq, atUs)
		}

		c := recs[src]
		c.AtUs, c.RTX, c.Dup = atUs, rtx, !rtx

		pos := len(recs)
		for i, r := range recs {
			if r.T == RecordEOF || r.AtUs > atUs {
				pos = i
				break
			}
		}

		if pos < len(recs) && recs[pos].T == RecordEOF && recs[pos].AtUs < atUs {
			return nil, fmt.Errorf("copy at %d us is after eof at %d us", atUs, recs[pos].AtUs)
		}

		return slices.Insert(slices.Clone(recs), pos, c), nil
	}
}

// Reorder delays each packet, with probability p, past 1..depth later
// packets. The read times stay in place; only the packets move between them.
func Reorder(depth int, p float64, rng *rand.Rand) Op {
	return func(recs []Record) ([]Record, error) {
		if depth < 1 || p < 0 || p > 1 || rng == nil {
			return nil, fmt.Errorf("invalid reorder depth %d, p %v", depth, p)
		}

		body, eof := splitEOF(recs)
		keys := make([]float64, len(body))
		order := make([]int, len(body))

		for i := range body {
			order[i] = i
			keys[i] = float64(i)

			if rng.Float64() < p {
				keys[i] += float64(1+rng.IntN(depth)) + 0.5
			}
		}

		sort.SliceStable(order, func(a, b int) bool { return keys[order[a]] < keys[order[b]] })

		out := make([]Record, len(body), len(recs))
		for slot, i := range order {
			out[slot] = body[i]
			out[slot].AtUs = body[slot].AtUs
		}

		return append(out, eof...), nil
	}
}

// StartAt drops the first k records and shifts the rest so the trace still
// starts at the original first read time, like a subscriber joining late.
func StartAt(k int) Op {
	return func(recs []Record) ([]Record, error) {
		body, _ := splitEOF(recs)
		if k < 0 || k >= len(body) {
			return nil, fmt.Errorf("start %d out of range for %d packets", k, len(body))
		}

		shift := body[k].AtUs - body[0].AtUs
		out := slices.Clone(recs[k:])

		for i := range out {
			out[i].AtUs -= shift
		}

		return out, nil
	}
}

// EOFAt truncates the schedule after atUs and ends it there with an eof.
func EOFAt(atUs int64) Op {
	return func(recs []Record) ([]Record, error) {
		if atUs < 0 {
			return nil, fmt.Errorf("negative eof time %d", atUs)
		}

		out := slices.DeleteFunc(slices.Clone(recs), func(r Record) bool {
			return r.T == RecordEOF || r.AtUs > atUs
		})

		return append(out, Record{T: RecordEOF, AtUs: atUs}), nil
	}
}

func isOriginal(r Record) bool {
	return r.T == RecordRTP && !r.RTX && !r.Dup
}

// splitEOF returns the records before a trailing eof, and the eof if any.
func splitEOF(recs []Record) ([]Record, []Record) {
	if n := len(recs); n > 0 && recs[n-1].T == RecordEOF {
		return recs[:n-1], recs[n-1:]
	}

	return recs, nil
}
