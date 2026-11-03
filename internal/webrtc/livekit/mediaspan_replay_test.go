package livekit

import (
	"fmt"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
)

// PTS are whole ms, so the span can start up to 1 ms away from its
// expected start.
const mediaSpanTolerance = time.Millisecond

// mediaSpanViolations checks the recorder's media span against a replay of
// one trace. The span must cover the file's media, and start at startUs
// when set.
//
// Otherwise a VP8 span starts at oracle.Placement's IdealStartUs. An Opus
// span starts at the first frame's read, less the lag oracle's backlog. A
// track its oracle cannot place fails.
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
	case track.Codec == oracle.CodecOpus:
		if r.audioLag == nil || r.audioLagErr != nil {
			return append(v, fmt.Sprintf("no audio lag: %v", r.audioLagErr))
		}

		firstUs = r.audioLag.StartAtUs
	case track.Codec == oracle.CodecVP8:
		if r.videoPlace == nil || r.videoPlaceErr != nil {
			return append(v, fmt.Sprintf("no video placement: %v", r.videoPlaceErr))
		}

		firstUs = r.videoPlace.IdealStartUs
	default:
		return v
	}

	want := r.readStart.Add(time.Duration(firstUs * float64(time.Microsecond)))
	if d := r.span.Start.Sub(want); d.Abs() > mediaSpanTolerance {
		v = append(v, fmt.Sprintf("media span starts %v from its expected start, %.0f µs", d, firstUs))
	}

	return v
}
