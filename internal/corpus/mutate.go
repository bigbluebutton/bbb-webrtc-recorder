package corpus

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"time"
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

// MediaClock models a sender whose media clock runs at rate times the wall
// clock between fromUs and toUs, such as one that loses captured audio before
// encoding it. Only read times change: the records of that span are read over
// 1/rate as long, and every record after it later by the time lost.
func MediaClock(fromUs, toUs int64, rate float64) Op {
	return func(recs []Record) ([]Record, error) {
		if fromUs < 0 || toUs <= fromUs || rate <= 0 {
			return nil, fmt.Errorf("invalid media clock span [%d, %d) us at rate %v", fromUs, toUs, rate)
		}

		stretch := func(atUs int64) int64 {
			switch {
			case atUs <= fromUs:
				return atUs
			case atUs < toUs:
				return fromUs + int64(math.Round(float64(atUs-fromUs)/rate))
			default:
				return atUs + int64(math.Round(float64(toUs-fromUs)/rate)) - (toUs - fromUs)
			}
		}

		out := slices.Clone(recs)
		for i := range out {
			out[i].AtUs = stretch(out[i].AtUs)
		}

		return out, nil
	}
}

// Delay models a network queue that holds what was sent between fromUs and
// toUs for delayUs longer, then drains: no record is read before one sent
// ahead of it. Apply it after SenderReports, whose times are the sender's.
func Delay(fromUs, toUs, delayUs int64) Op {
	return func(recs []Record) ([]Record, error) {
		if fromUs < 0 || toUs <= fromUs || delayUs < 0 {
			return nil, fmt.Errorf("invalid delay of %d us over [%d, %d) us", delayUs, fromUs, toUs)
		}

		return delayEach(recs, func(r Record) int64 {
			if r.AtUs >= fromUs && r.AtUs < toUs {
				return delayUs
			}

			return 0
		}), nil
	}
}

// delayEach reads each record by(record) later, then drains as in Delay.
func delayEach(recs []Record, by func(Record) int64) []Record {
	out := slices.Clone(recs)
	var last int64
	for i := range out {
		out[i].AtUs = max(out[i].AtUs+by(out[i]), last)
		last = out[i].AtUs
	}

	return out
}

// Blackout removes every original packet read between fromUs and toUs, as
// a path that carries nothing then would. Copies read then, such as RTX,
// stay. The gaps in sequence number, picture ID and timestamp are what the
// sender sent meanwhile.
func Blackout(fromUs, toUs int64) Op {
	return func(recs []Record) ([]Record, error) {
		if fromUs < 0 || toUs <= fromUs {
			return nil, fmt.Errorf("invalid blackout [%d, %d) us", fromUs, toUs)
		}

		return slices.DeleteFunc(slices.Clone(recs), func(r Record) bool {
			return isOriginal(r) && r.AtUs >= fromUs && r.AtUs < toUs
		}), nil
	}
}

// Stall models a receiver that reads nothing between fromUs and toUs. It
// then reads what came meanwhile in one burst: every record of the span is
// read at toUs, in the same order.
func Stall(fromUs, toUs int64) Op {
	return func(recs []Record) ([]Record, error) {
		if fromUs < 0 || toUs <= fromUs {
			return nil, fmt.Errorf("invalid stall [%d, %d) us", fromUs, toUs)
		}

		out := slices.Clone(recs)
		for i := range out {
			if out[i].AtUs >= fromUs && out[i].AtUs < toUs {
				out[i].AtUs = toUs
			}
		}

		return out, nil
	}
}

// Pause models a camera that sends no frame between fromUs and toUs. An
// encoder that skips frames does this, and so does LiveKit across a pause
// and resume.
//
// It removes the frames whose first original packet is read then, copies
// included. Later sequence numbers and VP8 picture IDs follow on with no
// gap, and the timestamps keep the sender's clock. Every payload except
// padding must use the VP8Payload layout, with a 15-bit picture ID.
func Pause(fromUs, toUs int64) Op { return pause(fromUs, toUs, true) }

// PauseOpus is Pause for an Opus track, which has no picture ID.
func PauseOpus(fromUs, toUs int64) Op { return pause(fromUs, toUs, false) }

func pause(fromUs, toUs int64, vp8 bool) Op {
	return func(recs []Record) ([]Record, error) {
		if fromUs < 0 || toUs <= fromUs {
			return nil, fmt.Errorf("invalid pause [%d, %d) us", fromUs, toUs)
		}

		// Sequence numbers and picture IDs are unwrapped in read order, so
		// a record counts the removed values before it across a wrap.
		var (
			cut     = map[uint32]bool{}
			seen    = map[uint32]bool{}
			seqs    = make([]int64, len(recs))
			pics    = make([]int64, len(recs))
			seq     unwrapper
			pic     unwrapper
			cutSeqs []int64
			cutPics []int64
		)

		for i, r := range recs {
			if r.T != RecordRTP {
				continue
			}

			seqs[i] = seq.next(int64(r.Seq), 1<<16)
			if vp8 && !r.Pad {
				id, bits := VP8PictureID(r.Pay)
				if bits != 15 {
					return nil, fmt.Errorf("record %d: no 15-bit VP8 picture ID", i)
				}

				pics[i] = pic.next(int64(id), 1<<15)
			}

			if isOriginal(r) && !seen[r.TS] {
				seen[r.TS] = true
				cut[r.TS] = r.AtUs >= fromUs && r.AtUs < toUs
				if cut[r.TS] {
					cutPics = append(cutPics, pics[i])
				}
			}

			if cut[r.TS] && isOriginal(r) {
				cutSeqs = append(cutSeqs, seqs[i])
			}
		}

		slices.Sort(cutSeqs)
		slices.Sort(cutPics)
		before := func(cut []int64, v int64) int {
			n, _ := slices.BinarySearch(cut, v)

			return n
		}

		out := make([]Record, 0, len(recs))
		for i, r := range recs {
			if r.T != RecordRTP {
				out = append(out, r)

				continue
			}

			if cut[r.TS] {
				continue
			}

			r.Seq -= uint16(before(cutSeqs, seqs[i]))
			if vp8 && !r.Pad {
				r.Pay = slices.Clone(r.Pay)
				SetVP8PictureID(r.Pay, uint16(pics[i]-int64(before(cutPics, pics[i]))))
			}

			out = append(out, r)
		}

		return out, nil
	}
}

// unwrapper extends a counter that wraps at some modulus, in read order.
type unwrapper struct {
	started bool
	last    int64
}

func (u *unwrapper) next(v, mod int64) int64 {
	if !u.started {
		u.started, u.last = true, v

		return v
	}

	d := (v - u.last%mod + mod) % mod
	if d >= mod/2 {
		d -= mod
	}

	u.last += d

	return u.last
}

// DelayFrames models frames that each wait their own time, as a camera's
// first frames do while its encoder starts. Every packet of the frame at
// timestamp ts is read delays[ts] µs later, then drains as in Delay.
func DelayFrames(delays map[uint32]int64) Op {
	return func(recs []Record) ([]Record, error) {
		for ts, d := range delays {
			if d < 0 {
				return nil, fmt.Errorf("negative delay of %d us for timestamp %d", d, ts)
			}
		}

		return delayEach(recs, func(r Record) int64 {
			if r.T != RecordRTP {
				return 0
			}

			return delays[r.TS]
		}), nil
	}
}

// SenderReports adds an RTCP sender report every everyUs from the first
// packet. Its NTP time is the read time on the sender's clock. Its RTP time
// is the last packet's timestamp plus the time since that packet, at
// clockRate, as pion and libwebrtc compute it. Apply it after MediaClock, so
// that the reports show the slow media clock.
func SenderReports(everyUs int64, clockRate uint32) Op {
	return func(recs []Record) ([]Record, error) {
		if everyUs <= 0 || clockRate == 0 {
			return nil, fmt.Errorf("invalid sender report period %d us at %d Hz", everyUs, clockRate)
		}

		body, eof := splitEOF(recs)
		out := make([]Record, 0, len(recs)+len(recs)/10)

		var (
			last Record
			sent bool
			next int64
		)

		report := func(atUs int64) Record {
			ticks := math.Round(float64(atUs-last.AtUs) * float64(clockRate) / 1e6)

			return Record{T: RecordSR, AtUs: atUs, NTP: SenderNTP(atUs), TS: last.TS + uint32(ticks)}
		}

		for _, r := range body {
			for sent && next <= r.AtUs {
				out = append(out, report(next))
				next += everyUs
			}

			out = append(out, r)

			if !isOriginal(r) {
				continue
			}

			if !sent {
				sent, next, last = true, r.AtUs+everyUs, r
			} else if int32(r.TS-last.TS) >= 0 {
				last = r
			}
		}

		return append(out, eof...), nil
	}
}

// senderEpoch is the sender's wall-clock time at atUs 0 in the sender
// reports that SenderReports adds.
var senderEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// ntpEpochOffset is the seconds from the NTP epoch (1900) to the Unix epoch.
const ntpEpochOffset = 2208988800

// SenderNTP is the 64-bit NTP time of atUs on the sender's clock, as
// SenderReports writes it.
func SenderNTP(atUs int64) uint64 {
	return NTP(senderEpoch.Add(time.Duration(atUs) * time.Microsecond))
}

// NTP is t as a 64-bit NTP time.
func NTP(t time.Time) uint64 {
	secs := uint64(t.Unix() + ntpEpochOffset)
	frac := (uint64(t.Nanosecond()) << 32) / uint64(time.Second)

	return secs<<32 | frac
}

// NTPTime is the time of a 64-bit NTP time.
func NTPTime(ntp uint64) time.Time {
	return time.Unix(int64(ntp>>32)-ntpEpochOffset, int64((ntp&0xFFFFFFFF)*uint64(time.Second)>>32))
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
