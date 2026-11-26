package recorder

import (
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/at-wat/ebml-go"
	"github.com/at-wat/ebml-go/mkvcore"
	"github.com/at-wat/ebml-go/webm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type containerDoc struct {
	Header  webm.EBMLHeader `ebml:"EBML"`
	Segment webm.Segment    `ebml:"Segment"`
}

func readContainer(t *testing.T, file string) containerDoc {
	f, err := os.Open(file)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	var doc containerDoc
	require.NoError(t, ebml.Unmarshal(f, &doc))

	return doc
}

func readBlocks(t *testing.T, file string) [][]int64 {
	f, err := os.Open(file)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	readers, err := mkvcore.NewSimpleBlockReader(f)
	require.NoError(t, err)

	// One goroutine feeds every reader, so they are read together.
	tracks := make([][]int64, len(readers))
	errs := make([]error, len(readers))
	var wg sync.WaitGroup
	for i, r := range readers {
		wg.Go(func() {
			for {
				_, _, ts, err := r.Read()
				if err != nil {
					if err != io.EOF {
						errs[i] = err
					}

					return
				}
				tracks[i] = append(tracks[i], ts)
			}
		})
	}
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}

	return tracks
}

func TestOpenContainerWritesWebMTracks(t *testing.T) {
	file := filepath.Join(t.TempDir(), "av.webm")
	writers, err := openContainer(file, 0o600, FormatWebM, []trackConfig{
		{codec: CodecVP8, width: 320, height: 240},
		{codec: CodecOpus},
	}, 32)
	require.NoError(t, err)
	require.Len(t, writers, 2)

	_, err = writers[0].Write(true, 0, []byte{1})
	require.NoError(t, err)
	_, err = writers[1].Write(true, 0, []byte{2})
	require.NoError(t, err)
	_, err = writers[0].Write(false, 33, []byte{3})
	require.NoError(t, err)
	for _, w := range writers {
		require.NoError(t, w.Close())
	}

	doc := readContainer(t, file)
	assert.Equal(t, "webm", doc.Header.DocType)

	tracks := doc.Segment.Tracks.TrackEntry
	require.Len(t, tracks, 2)

	assert.Equal(t, webm.TrackEntry{
		Name: "Video", TrackNumber: 1, TrackUID: 12345, CodecID: "V_VP8", TrackType: 1,
		Video: &webm.Video{PixelWidth: 320, PixelHeight: 240},
	}, tracks[0])
	assert.Equal(t, webm.TrackEntry{
		Name: "Audio", TrackNumber: 2, TrackUID: 54321, CodecID: "A_OPUS", TrackType: 2,
		Audio: &webm.Audio{SamplingFrequency: 48000, Channels: 2},
	}, tracks[1])

	assert.Equal(t, [][]int64{{0, 33}, {0}}, readBlocks(t, file))
}

func TestOpenContainerWritesMatroskaWithCodecPrivate(t *testing.T) {
	file := filepath.Join(t.TempDir(), "video.mkv")
	private := []byte{0x01, 0x42, 0xC0, 0x1E, 0xFF, 0xE1}
	writers, err := openContainer(file, 0o600, FormatMKV, []trackConfig{
		{codec: CodecVP8, width: 640, height: 360, codecPrivate: private},
	}, 32)
	require.NoError(t, err)
	require.Len(t, writers, 1)

	_, err = writers[0].Write(true, 0, []byte{1})
	require.NoError(t, err)
	require.NoError(t, writers[0].Close())

	doc := readContainer(t, file)
	assert.Equal(t, "matroska", doc.Header.DocType)
	require.Len(t, doc.Segment.Tracks.TrackEntry, 1)
	assert.Equal(t, private, doc.Segment.Tracks.TrackEntry[0].CodecPrivate)
	assert.Equal(t, uint64(640), doc.Segment.Tracks.TrackEntry[0].Video.PixelWidth)
	assert.Equal(t, uint64(360), doc.Segment.Tracks.TrackEntry[0].Video.PixelHeight)
}

func TestOpenContainerFailsOnAMissingDirectory(t *testing.T) {
	_, err := openContainer(filepath.Join(t.TempDir(), "missing", "x.webm"), 0o600, FormatWebM,
		[]trackConfig{{codec: CodecOpus}}, 32)
	assert.Error(t, err)
}

func TestFormatExtension(t *testing.T) {
	assert.Equal(t, ".webm", FormatWebM.Extension())
	assert.Equal(t, ".mkv", FormatMKV.Extension())
}
