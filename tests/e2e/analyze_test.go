//go:build e2e && linux

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/stretchr/testify/require"
)

// Output is what one single-track recording session left on disk, read back.
type Output struct {
	WebMPath  string
	TracePath string
	WebM      *oracle.Track // the session's only track
	Trace     *corpus.Trace
	// ReadFrames counts the distinct frames (by RTP timestamp) among the
	// trace's original media records; ReadPackets the distinct sequence
	// numbers. Padding (LiveKit's bandwidth probes, with timestamps of their
	// own) is counted apart; RTXPackets counts the retransmitted reads.
	ReadFrames  int
	ReadPackets int
	PadPackets  int
	RTXPackets  int
	// PLIs counts the trace's pli records by reason.
	PLIs map[string]int
	EOF  *corpus.Record
}

// ReadOutput reads rec's WebM with the oracle and the trace the capture
// wrote next to it for trackID, failing t if either is missing or invalid,
// and saves a summary next to them.
func ReadOutput(t *testing.T, rec *Recording, trackID string) *Output {
	t.Helper()

	o := &Output{WebMPath: rec.Path, TracePath: TracePath(rec.Path, trackID), PLIs: map[string]int{}}

	sum, err := oracle.SummarizeWebM(o.WebMPath)
	require.NoError(t, err)
	require.Len(t, sum.Tracks, 1, "a session records one track")

	for _, tr := range sum.Tracks {
		o.WebM = tr
	}

	o.Trace, err = corpus.ReadFile(o.TracePath)
	require.NoError(t, err, "trace capture")

	frames := map[uint32]bool{}
	seqs := map[uint16]bool{}

	for i, r := range o.Trace.Records {
		switch r.T {
		case corpus.RecordRTP:
			if r.Pad {
				o.PadPackets++
				continue
			}

			// pion restores the original seq on an RTX read, so a frame
			// repaired over RTX still counts as read.
			if r.RTX {
				o.RTXPackets++
			}

			frames[r.TS] = true
			seqs[r.Seq] = true
		case corpus.RecordPLI:
			o.PLIs[r.Reason]++
		case corpus.RecordEOF:
			o.EOF = &o.Trace.Records[i]
		}
	}

	o.ReadFrames, o.ReadPackets = len(frames), len(seqs)

	o.writeSummary(t)

	return o
}

// TracePath is where the trace capture writes trackID's trace for the
// recording at webmPath: <base>.<trackID>.trace.ndjson.gz, with the
// characters the adapter's sanitizer replaces mapped to '_'.
func TracePath(webmPath, trackID string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, trackID)

	return strings.TrimSuffix(webmPath, filepath.Ext(webmPath)) + "." + safe + ".trace.ndjson.gz"
}

func (o *Output) writeSummary(t *testing.T) {
	s := map[string]any{
		"webm": map[string]any{
			"codec":        o.WebM.Codec,
			"blocks":       o.WebM.Blocks,
			"keyBlocks":    o.WebM.KeyBlocks,
			"vp8Keyframes": o.WebM.VP8Keyframes,
			"firstPTS":     o.WebM.FirstPTS,
			"lastPTS":      o.WebM.LastPTS,
			"maxGapMs":     o.WebM.MaxGapMs,
			"monotonic":    o.WebM.Monotonic,
			"duplicatePTS": o.WebM.DuplicatePTS,
		},
		"trace": map[string]any{
			"header":      o.Trace.Header,
			"records":     len(o.Trace.Records),
			"readFrames":  o.ReadFrames,
			"readPackets": o.ReadPackets,
			"padPackets":  o.PadPackets,
			"rtxPackets":  o.RTXPackets,
			"plis":        o.PLIs,
			"eof":         o.EOF,
		},
	}

	data, err := json.MarshalIndent(s, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(strings.TrimSuffix(o.WebMPath, ".webm")+".summary.json", data, 0o644))
}
