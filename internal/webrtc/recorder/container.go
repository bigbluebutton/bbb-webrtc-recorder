package recorder

import (
	"os"

	"github.com/at-wat/ebml-go/mkv"
	"github.com/at-wat/ebml-go/mkvcore"
	"github.com/at-wat/ebml-go/webm"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal"
)

// A Format is a recording's container. Its extension ends the file name.
type Format string

const (
	FormatWebM Format = "webm"
	FormatMKV  Format = "mkv"
)

// Extension is the file name extension of f, with its dot.
func (f Format) Extension() string {
	return "." + string(f)
}

// Every recording uses these track UIDs. Readers do not compare them
// across files, but GStreamer refuses a track with UID 0.
const (
	videoTrackUID = 12345
	audioTrackUID = 54321
)

// trackConfig is one track of a recording, as it is known when the file
// is created.
type trackConfig struct {
	codec         *Codec
	width, height int // video only
	codecPrivate  []byte
}

func (t trackConfig) entry(number int) webm.TrackEntry {
	e := webm.TrackEntry{
		TrackNumber:  uint64(number),
		CodecID:      t.codec.MatroskaID,
		CodecPrivate: t.codecPrivate,
	}

	if t.codec.Video {
		e.Name, e.TrackUID, e.TrackType = "Video", videoTrackUID, 1
		e.Video = &webm.Video{PixelWidth: uint64(t.width), PixelHeight: uint64(t.height)}
	} else {
		e.Name, e.TrackUID, e.TrackType = "Audio", audioTrackUID, 2
		e.Audio = &webm.Audio{SamplingFrequency: float64(t.codec.ClockRate), Channels: 2}
	}

	return e
}

// openContainer creates file in format f with tracks and returns a block
// writer for each, in the same order. A block sorter in front of the
// writers keeps up to maxDelayedBlocks blocks to interleave the tracks by
// timestamp.
func openContainer(file string, mode os.FileMode, f Format, tracks []trackConfig, maxDelayedBlocks int) ([]webm.BlockWriteCloser, error) {
	w, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return nil, err
	}

	entries := make([]webm.TrackEntry, 0, len(tracks))
	for i, t := range tracks {
		entries = append(entries, t.entry(i+1))
	}

	interceptor, err := mkvcore.NewMultiTrackBlockSorter(
		mkvcore.WithMaxDelayedPackets(maxDelayedBlocks),
		mkvcore.WithSortRule(mkvcore.BlockSorterWriteOutdated),
	)
	if err != nil {
		_ = w.Close()

		return nil, err
	}

	opts := []mkvcore.BlockWriterOption{
		mkvcore.WithSegmentInfo(&webm.Info{
			TimecodeScale: 1000000, // 1ms
			MuxingApp:     internal.AppName,
			WritingApp:    internal.AppName,
		}),
		mkvcore.WithBlockInterceptor(interceptor),
	}

	// The webm package writes its own header unless told otherwise. A copy
	// keeps the shared default unchanged.
	if f == FormatMKV {
		header := *mkv.DefaultEBMLHeader
		opts = append(opts, mkvcore.WithEBMLHeader(&header))
	}

	writers, err := webm.NewSimpleBlockWriter(w, entries, opts...)
	if err != nil {
		_ = w.Close()

		return nil, err
	}

	return writers, nil
}
