package livekit

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/at-wat/ebml-go"
	"github.com/at-wat/ebml-go/webm"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/utils"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var h264Info = trackInfo{
	id: "video", kind: TrackKindVideo, mime: "video/h264", codec: recorder.CodecH264,
	source: "camera", ssrc: 1234, pubMime: "video/H264",
}

func newH264LoopHarness(t *testing.T) (*loopHarness, *recorder.WebmRecorder) {
	t.Helper()

	rec := recorder.NewWebmRecorder(filepath.Join(t.TempDir(), "camera.mkv"), 0o600, 256, 32, true, false)
	rec.SetHasVideo(true)
	h := newLoopHarness(rec, h264Info.id)
	require.True(t, h.w.acceptCodec(h264Info.id, h264Info.kind, recorder.CodecH264))

	return h, rec
}

// h264Blocks reads back the key flags of a recording's video blocks.
func h264Blocks(t *testing.T, file string) (keys []bool, entry webm.TrackEntry) {
	t.Helper()

	f, err := os.Open(file)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	var doc struct {
		Header  webm.EBMLHeader `ebml:"EBML"`
		Segment webm.Segment    `ebml:"Segment"`
	}
	require.NoError(t, ebml.Unmarshal(f, &doc))
	require.Len(t, doc.Segment.Tracks.TrackEntry, 1)
	assert.Equal(t, "matroska", doc.Header.DocType)

	_, blocks, err := oracle.ReadBlocks(file)
	require.NoError(t, err)
	for _, b := range blocks {
		keys = append(keys, b.Key)
	}

	return keys, doc.Segment.Tracks.TrackEntry[0]
}

func h264Reads(t *testing.T, o corpus.H264Options, ops ...corpus.Op) []trackRead {
	t.Helper()

	o.SSRC = h264Info.ssrc
	tr, err := corpus.GenerateH264(o)
	require.NoError(t, err)
	tr.Records, err = corpus.Apply(tr.Records, ops...)
	require.NoError(t, err)

	reads, _ := traceReads(tr)

	return reads
}

func TestReadTrackHandsH264SamplesToTheRecorder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, rec := newH264LoopHarness(t)
		reads := h264Reads(t, corpus.H264Options{
			FPS: 15, Frames: 30, KeyInterval: 15, KeySlicePackets: 3, Slices: 2, StartSeq: 65500,
		})

		h.run(newFakeTrack(reads), h264Info)
		rec.Close()

		keys, entry := h264Blocks(t, rec.GetFilePath())
		require.Len(t, keys, 30)
		for i, key := range keys {
			assert.Equal(t, i%15 == 0, key, "block %d", i)
		}
		assert.Equal(t, "V_MPEG4/ISO/AVC", entry.CodecID)
		assert.Equal(t, uint64(640), entry.Video.PixelWidth)
		assert.Equal(t, uint64(360), entry.Video.PixelHeight)
		assert.NotEmpty(t, entry.CodecPrivate)
	})
}

func TestReadTrackDropsH264FramesAfterALostFrameUntilAKeyFrame(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, rec := newH264LoopHarness(t)
		// Frame 5, a one-packet delta frame after a four-packet key frame (a
		// STAP-A and 3 FU-A fragments), is lost and never repaired.
		reads := h264Reads(t, corpus.H264Options{FPS: 15, Frames: 30, KeyInterval: 15, KeySlicePackets: 3},
			corpus.DropSeqs(1+3+4))

		h.run(newFakeTrack(reads), h264Info)
		rec.Close()

		keys, _ := h264Blocks(t, rec.GetFilePath())
		assert.Len(t, keys, 5+15, "frames 0-4, then from the key frame at 15")
		assert.True(t, keys[5])
	})
}

// LiveKit pauses a downtrack inside frame 20, after its first slice. It sends
// padding with no payload at that frame's timestamp, then resumes at the key
// frame 30 with no sequence gap.
func TestReadTrackDropsAnH264FrameCutAtAPause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, rec := newH264LoopHarness(t)
		var frames [][]trackRead
		for _, r := range h264Reads(t, corpus.H264Options{FPS: 15, Frames: 45, KeyInterval: 15, Slices: 2}) {
			n := len(frames)
			switch {
			case r.pkt == nil:
			case n == 0 || r.pkt.Timestamp != frames[n-1][0].pkt.Timestamp:
				frames = append(frames, []trackRead{r})
			default:
				frames[n-1] = append(frames[n-1], r)
			}
		}
		require.Len(t, frames, 45)

		cut := frames[20][0]
		cut.pkt.Marker = false
		pad := &rtp.Packet{Header: cut.pkt.Header}
		pad.Padding = true
		pad.SequenceNumber++
		reads := slices.Concat(slices.Concat(frames[:20]...), []trackRead{cut, {at: cut.at, pkt: pad}})
		seq := pad.SequenceNumber
		for _, r := range slices.Concat(frames[30:]...) {
			seq++
			r.pkt.SequenceNumber = seq
			reads = append(reads, r)
		}

		h.run(newFakeTrack(reads), h264Info)
		rec.Close()

		keys, _ := h264Blocks(t, rec.GetFilePath())
		require.Len(t, keys, 20+15, "frames 0-19, then from the key frame at 30")
		assert.True(t, keys[20])
	})
}

func TestAcceptCodecRefusesAResubscriptionWithAnotherCodec(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, rec := newH264LoopHarness(t)

		assert.False(t, h.w.acceptCodec(h264Info.id, TrackKindVideo, recorder.CodecVP8), "a codec regression")
		assert.True(t, h.w.acceptCodec(h264Info.id, TrackKindVideo, recorder.CodecH264), "the codec it had")
		assert.Equal(t, []string{"video/h264"}, rec.Codecs())

		states, _ := h.reported()
		assert.Empty(t, states, "the first reader's EOF ends the session")
	})
}

func TestAcceptCodecFailsACodecTheFormatCannotHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := recorder.NewWebmRecorder(filepath.Join(t.TempDir(), "camera.webm"), 0o600, 256, 32, true, false)
		rec.SetHasVideo(true)
		h := newLoopHarness(rec, h264Info.id)

		assert.False(t, h.w.acceptCodec(h264Info.id, TrackKindVideo, recorder.CodecH264))
		states, _ := h.reported()
		assert.Equal(t, []utils.ConnectionState{utils.ConnectionStateFailed}, states)
	})
}

func TestCodecForFindsH264(t *testing.T) {
	c, ok := recorder.CodecFor("video/H264")
	require.True(t, ok)
	assert.Same(t, recorder.CodecH264, c)
}
