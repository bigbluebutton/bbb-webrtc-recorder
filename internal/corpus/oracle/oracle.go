// Package oracle reads a recorded WebM file back into per-track summaries
// that corpus replay tests assert on.
package oracle

import (
	"fmt"
	"os"
	"slices"

	"github.com/at-wat/ebml-go"
	"github.com/at-wat/ebml-go/webm"
)

const (
	CodecVP8  = "V_VP8"
	CodecOpus = "A_OPUS"
)

// Summary holds one Track per codec id found in the file.
type Summary struct {
	Tracks map[string]*Track
}

// Track summarizes the blocks of one WebM track, in file order.
type Track struct {
	Number uint64
	Codec  string
	Blocks int
	// KeyBlocks counts blocks carrying the WebM keyframe flag.
	KeyBlocks int
	// VP8Keyframes counts frames whose VP8 frame tag marks a key frame
	// (byte 0, bit 0 clear). It stays 0 for other codecs.
	VP8Keyframes int
	// PTS is cluster timecode plus block timecode, in ms.
	PTS []int64
	// DuplicatePTS lists every block whose PTS an earlier block of the track
	// already used: three blocks at the same PTS add two entries.
	DuplicatePTS []int64
	// MaxGapMs is the largest step between consecutive distinct PTS values
	// once sorted, so a backward jump does not hide the gap it leaves.
	MaxGapMs int64
	// Monotonic reports that PTS never decreases in file order.
	Monotonic bool
	FirstPTS  int64
	LastPTS   int64
}

// Track returns the summary for a codec id, or nil when the file has none.
func (s *Summary) Track(codec string) *Track {
	return s.Tracks[codec]
}

// CheckNoDuplicatePTS returns an error naming the duplicate PTS values, if any.
func (t *Track) CheckNoDuplicatePTS() error {
	if len(t.DuplicatePTS) == 0 {
		return nil
	}

	return fmt.Errorf("%s track %d: %d duplicate PTS: %v", t.Codec, t.Number, len(t.DuplicatePTS), t.DuplicatePTS)
}

// CheckMonotonic returns an error naming the first backward PTS step, if any.
func (t *Track) CheckMonotonic() error {
	for i := 1; i < len(t.PTS); i++ {
		if t.PTS[i] < t.PTS[i-1] {
			return fmt.Errorf("%s track %d: PTS goes back from %d to %d at block %d",
				t.Codec, t.Number, t.PTS[i-1], t.PTS[i], i)
		}
	}

	return nil
}

// SummarizeWebM parses the whole file. BlockGroups, which WebmRecorder never
// writes, are taken after the SimpleBlocks of their cluster, as keyframes when
// they carry no ReferenceBlock.
func SummarizeWebM(path string) (*Summary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only

	var doc struct {
		Header  webm.EBMLHeader `ebml:"EBML"`
		Segment webm.Segment    `ebml:"Segment"`
	}
	if err := ebml.Unmarshal(f, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(doc.Segment.Tracks.TrackEntry) == 0 {
		return nil, fmt.Errorf("parse %s: no tracks", path)
	}

	s := &Summary{Tracks: make(map[string]*Track)}
	byNumber := make(map[uint64]*Track)
	for _, e := range doc.Segment.Tracks.TrackEntry {
		if _, ok := s.Tracks[e.CodecID]; ok {
			return nil, fmt.Errorf("parse %s: more than one %s track", path, e.CodecID)
		}
		if _, ok := byNumber[e.TrackNumber]; ok {
			return nil, fmt.Errorf("parse %s: track number %d used twice", path, e.TrackNumber)
		}
		t := &Track{Number: e.TrackNumber, Codec: e.CodecID}
		s.Tracks[e.CodecID] = t
		byNumber[e.TrackNumber] = t
	}

	scale := int64(doc.Segment.Info.TimecodeScale)
	if scale == 0 {
		scale = 1000000
	}

	add := func(b ebml.Block, key bool, cluster uint64) error {
		t, ok := byNumber[b.TrackNumber]
		if !ok {
			return fmt.Errorf("parse %s: block for unknown track %d", path, b.TrackNumber)
		}
		pts := (int64(cluster) + int64(b.Timecode)) * scale / 1000000
		for _, frame := range b.Data {
			t.Blocks++
			if key {
				t.KeyBlocks++
			}
			if t.Codec == CodecVP8 && len(frame) > 0 && frame[0]&0x01 == 0 {
				t.VP8Keyframes++
			}
			t.PTS = append(t.PTS, pts)
		}

		return nil
	}

	for _, c := range doc.Segment.Cluster {
		for _, b := range c.SimpleBlock {
			if err := add(b, b.Keyframe, c.Timecode); err != nil {
				return nil, err
			}
		}
		for _, g := range c.BlockGroup {
			if err := add(g.Block, g.ReferenceBlock == 0, c.Timecode); err != nil {
				return nil, err
			}
		}
	}

	for _, t := range s.Tracks {
		t.finish()
	}

	return s, nil
}

func (t *Track) finish() {
	t.Monotonic = true
	if len(t.PTS) == 0 {
		return
	}

	t.FirstPTS = t.PTS[0]
	t.LastPTS = t.PTS[len(t.PTS)-1]

	seen := make(map[int64]bool, len(t.PTS))
	for i, pts := range t.PTS {
		if seen[pts] {
			t.DuplicatePTS = append(t.DuplicatePTS, pts)
		}
		seen[pts] = true
		if i > 0 && pts < t.PTS[i-1] {
			t.Monotonic = false
		}
	}

	sorted := slices.Clone(t.PTS)
	slices.Sort(sorted)
	for i := 1; i < len(sorted); i++ {
		t.MaxGapMs = max(t.MaxGapMs, sorted[i]-sorted[i-1])
	}
}
