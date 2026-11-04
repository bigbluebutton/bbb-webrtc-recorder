// Package oracle reads a recorded WebM file back into per-track summaries
// that corpus replay tests assert on.
package oracle

import (
	"fmt"
	"os"
	"slices"

	"github.com/at-wat/ebml-go"
	"github.com/at-wat/ebml-go/webm"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
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
	// Tags holds each VP8 block's corpus.TaggedFrame value, parallel to
	// PTS, and TagOK whether the block has one. Both stay nil for other
	// codecs.
	Tags  []uint32
	TagOK []bool
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

// Block is one frame of a WebM block, with the block's track, PTS and flags.
type Block struct {
	Track uint64
	PTS   int64 // ms
	Key   bool  // the block's keyframe flag
	Group bool  // from a BlockGroup
	Data  []byte
}

// ReadBlocks parses the whole file and returns its tracks and its blocks:
// each cluster's SimpleBlocks, then its BlockGroups.
// WebmRecorder never writes BlockGroups. One with no ReferenceBlock is a
// keyframe.
func ReadBlocks(path string) ([]webm.TrackEntry, []Block, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close() //nolint:errcheck // read-only

	var doc struct {
		Header  webm.EBMLHeader `ebml:"EBML"`
		Segment webm.Segment    `ebml:"Segment"`
	}
	if err := ebml.Unmarshal(f, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}

	scale := int64(doc.Segment.Info.TimecodeScale)
	if scale == 0 {
		scale = 1000000
	}

	var blocks []Block
	add := func(b ebml.Block, key, group bool, cluster uint64) {
		pts := (int64(cluster) + int64(b.Timecode)) * scale / 1000000
		for _, data := range b.Data {
			blocks = append(blocks, Block{Track: b.TrackNumber, PTS: pts, Key: key, Group: group, Data: data})
		}
	}

	for _, c := range doc.Segment.Cluster {
		for _, b := range c.SimpleBlock {
			add(b, b.Keyframe, false, c.Timecode)
		}
		for _, g := range c.BlockGroup {
			add(g.Block, g.ReferenceBlock == 0, true, c.Timecode)
		}
	}

	return doc.Segment.Tracks.TrackEntry, blocks, nil
}

// SummarizeWebM parses the whole file, through ReadBlocks.
func SummarizeWebM(path string) (*Summary, error) {
	entries, blocks, err := ReadBlocks(path)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("parse %s: no tracks", path)
	}

	s := &Summary{Tracks: make(map[string]*Track)}
	byNumber := make(map[uint64]*Track)
	for _, e := range entries {
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

	for _, fr := range blocks {
		t, ok := byNumber[fr.Track]
		if !ok {
			return nil, fmt.Errorf("parse %s: block for unknown track %d", path, fr.Track)
		}
		t.Blocks++
		if fr.Key {
			t.KeyBlocks++
		}
		if t.Codec == CodecVP8 {
			if len(fr.Data) > 0 && fr.Data[0]&0x01 == 0 {
				t.VP8Keyframes++
			}

			tag, ok := corpus.TaggedFrame(fr.Data)
			t.Tags = append(t.Tags, tag)
			t.TagOK = append(t.TagOK, ok)
		}
		t.PTS = append(t.PTS, fr.PTS)
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
