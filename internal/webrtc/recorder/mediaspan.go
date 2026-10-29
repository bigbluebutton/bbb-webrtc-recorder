package recorder

import (
	"slices"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/types"
)

const (
	// The first reads can find packets that waited in the receive buffers,
	// all read at once, and a camera can start behind and catch up. So until
	// a track's frames written span this much media, a frame that arrived
	// sooner than its PTS implies moves the start back.
	//
	// At BBB's 30-50 kbps profiles, Chromium's fake camera is on time only
	// 1.0-1.2 s in. A microphone gets the same window. A delay on the path
	// both share then comes off both starts, if it clears within 2 s of each
	// file's first frame.
	MediaSpanWindow = 2 * time.Second
	// Opus frames are 20 ms apart. A gap that MediaClock inserts moves the
	// PTS ahead of the arrivals, and would move the start back by the gap.
	// So a longer step between a microphone's frames settles its start.
	mediaSpanAudioJumpStep = 120 * time.Millisecond
	// The start moves back at most this far from the first frame's arrival.
	// bbb-webrtc-sfu ignores a start more than PREVIOUS_FILE_TOLERANCE_MS
	// (1 s) before its request, and the first frame arrives after that
	// request: change both together. So a track whose first frames stall
	// over 1 s starts late by the stall less 1 s.
	mediaSpanMaxBacklog = time.Second
	// The step before the last frame stands for its duration, up to these.
	// Opus packets are 120 ms at most.
	maxAudioStep = 120 * time.Millisecond
	maxVideoStep = time.Second
	// Frames whose arrivals are kept until the start settles.
	mediaSpanMaxArrivals = 256
)

type spanRules struct {
	jumpStep time.Duration // a longer step between frames settles the start; 0 for none
	maxStep  time.Duration // the step that stands for the last frame's duration, at most
}

var (
	audioSpan = spanRules{jumpStep: mediaSpanAudioJumpStep, maxStep: maxAudioStep}
	// Video has no jump step; a screen share gets a camera's rules. A PTS
	// step longer than its arrival step means the frames before it were
	// late, or the PTS after it ran ahead. Moving the start back then places
	// every later frame where it arrived. A step of elapsed time has an
	// equal arrival gap, so it does not move the start.
	videoSpan = spanRules{maxStep: maxVideoStep}
)

// mediaSpan finds where the media a recorder writes sits on the wall clock.
// A frame arrives with its earliest packet; a retransmission can trail the
// rest. A frame that repeats a written frame's timestamp, as after an Opus
// step back, gets its own arrival.
//
// Two tracks in one file are followed as one, from the first frame either
// writes. Only mediasoup writes such files for bbb-webrtc-sfu, and it never
// reports the span.
type mediaSpan struct {
	arrivals map[uint32]time.Time // by RTP timestamp, until the start settles
	order    []uint32             // the keys of arrivals, oldest first

	written  bool
	settled  bool
	first    time.Time // the arrival of the first frame written
	start    time.Time
	firstPTS time.Duration
	lastPTS  time.Duration
	lastStep time.Duration
}

func (s *mediaSpan) arrived(ts uint32, at time.Time) {
	if s.settled {
		return
	}

	if prev, ok := s.arrivals[ts]; ok {
		if at.Before(prev) {
			s.arrivals[ts] = at
		}

		return
	}

	if s.arrivals == nil {
		s.arrivals = make(map[uint32]time.Time)
	}

	if len(s.order) == mediaSpanMaxArrivals {
		delete(s.arrivals, s.order[0])
		s.order = s.order[1:]
	}

	s.arrivals[ts] = at
	s.order = append(s.order, ts)
}

// wrote notes that the frame at RTP timestamp ts was written at pts, step
// after the frame written before it, under its kind's rules r.
func (s *mediaSpan) wrote(ts uint32, pts, step time.Duration, r spanRules) {
	at, known := s.arrivals[ts]
	if known {
		delete(s.arrivals, ts)
		if i := slices.Index(s.order, ts); i >= 0 {
			s.order = slices.Delete(s.order, i, i+1)
		}
	}

	switch {
	case !s.written:
		if !known {
			at = time.Now()
		}

		s.written = true
		s.first = at
		s.start = at
		s.firstPTS = pts
	case s.settled:
	case pts-s.firstPTS > MediaSpanWindow || r.jumpStep > 0 && step > r.jumpStep:
		s.settled = true
		s.arrivals = nil
		s.order = nil
	case known:
		start := at.Add(-(pts - s.firstPTS))
		if floor := s.first.Add(-mediaSpanMaxBacklog); start.Before(floor) {
			start = floor
		}

		if start.Before(s.start) {
			s.start = start
		}
	}

	s.lastPTS = pts
	s.lastStep = min(step, r.maxStep)
}

// span returns false while no frame is written. The span starts at the
// first frame written, not at PTS 0. record-and-playback's archive step
// remuxes the file, which puts that frame at time 0.
func (s *mediaSpan) span() (types.MediaSpan, bool) {
	if !s.written {
		return types.MediaSpan{}, false
	}

	return types.MediaSpan{
		Start:   s.start,
		End:     s.start.Add(s.lastPTS - s.firstPTS + s.lastStep),
		Settled: s.settled,
	}, true
}
