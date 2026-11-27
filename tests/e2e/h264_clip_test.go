//go:build e2e && linux

package e2e

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/google/uuid"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// README.md's "The H.264 clip" says what the clip holds and how to make it.
const (
	h264ClipPath   = "testdata/h264-cbp-640x360-15fps.h264"
	h264ClipFPS    = 15
	h264ClipFrames = 60
	// Under the clip's smallest slice, 334 bytes: every slice goes as FU-A
	// fragments.
	h264ClipMTU = 300
	// A frame's packets go out over this span, as in the synthetic schedules.
	h264ClipSpreadUs = 2000
)

// h264ClipSchedule packetizes the clip with pion's H264Payloader into a sent
// schedule, one frame per access unit.
func h264ClipSchedule(t *testing.T) *corpus.Trace {
	t.Helper()

	aus := h264AccessUnits(t, h264ClipPath)
	require.Len(t, aus, h264ClipFrames, "access units in %s", h264ClipPath)

	h := corpus.NewSyntheticHeader(corpus.KindVideo, 0, "tests/e2e "+h264ClipPath)
	h.Codec, h.ClockRate = corpus.CodecH264, 90000
	trace := &corpus.Trace{Header: h}

	var (
		p   codecs.H264Payloader
		seq uint16
	)
	for f, au := range aus {
		pays := p.Payload(h264ClipMTU, au)
		require.NotEmpty(t, pays, "access unit %d", f)
		for _, pay := range pays {
			nalType := pay[0] & 0x1f
			require.False(t, nalType == 1 || nalType == 5, "access unit %d: a slice fits one packet at h264ClipMTU", f)
		}

		for j, pay := range pays {
			at := int64(f) * 1_000_000 / h264ClipFPS
			if len(pays) > 1 {
				at += int64(j) * h264ClipSpreadUs / int64(len(pays)-1)
			}

			trace.Records = append(trace.Records, corpus.Record{
				T: corpus.RecordRTP, AtUs: at, Seq: seq, TS: uint32(f * 90000 / h264ClipFPS),
				M: j == len(pays)-1, PT: 102, PLen: len(pay), Pay: pay,
			})
			seq++
		}
	}

	return trace
}

// h264AccessUnits reads the Annex-B stream at path into access units, each
// its NAL units with start codes. A unit starts at a parameter set or SEI
// after a slice, or at a slice whose first_mb_in_slice is 0.
func h264AccessUnits(t *testing.T, path string) [][]byte {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	r, err := h264reader.NewReaderWithOptions(bytes.NewReader(data), h264reader.WithIncludeSEI(true))
	require.NoError(t, err)

	var (
		aus      [][]byte
		cur      []byte
		hasSlice bool
	)
	for {
		nal, err := r.NextNAL()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)

		slice := nal.UnitType == h264reader.NalUnitTypeCodedSliceNonIdr || nal.UnitType == h264reader.NalUnitTypeCodedSliceIdr
		// first_mb_in_slice 0 is exp-Golomb's single 1 bit.
		if hasSlice && (!slice || len(nal.Data) > 1 && nal.Data[1]&0x80 != 0) {
			aus, cur, hasSlice = append(aus, cur), nil, false
		}

		cur = append(append(cur, 0, 0, 0, 1), nal.Data...)
		hasSlice = hasSlice || slice
	}

	if hasSlice {
		aus = append(aus, cur)
	}

	return aus
}

// TestH264ClipDecodes records a real x264 clip, packetized as lksdk does
// but at h264ClipMTU. KeyframeFromSchedule keeps the stream decodable
// across PLIs. Beyond the oracle, ffmpeg must decode the file with no
// error, and remux it as BBB's archive step does. README.md's row lists
// each check.
func TestH264ClipDecodes(t *testing.T) {
	const (
		// The clip loops past its 4 s.
		recordFor     = 6 * time.Second
		frameInterval = time.Second / h264ClipFPS
		// The subscription, LiveKit's PLI and the key frame that answers it.
		startupAllowance = time.Second
	)

	s := NewStack(t, StackOptions{})
	room := "e2e-h264-clip-" + uuid.NewString()[:8]
	pub := s.NewPublisher(room, "e2e-publisher")
	cam := pub.PublishH264(TrackOptions{Name: "camera", Schedule: h264ClipSchedule(t), KeyframeFromSchedule: true})

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	require.NoError(t, pub.WaitPublished(ctx, cam))

	started := time.Now()
	rec := record(ctx, t, s, room, cam.SID)
	time.Sleep(recordFor)
	stopSent := time.Now()
	stop, err := s.Driver.StopAndWait(ctx, rec.SessionID)
	require.NoError(t, err)
	assert.Equal(t, events.StopReasonNormal, stop.Reason)

	out := ReadOutput(t, rec, cam.SID)
	mkv := out.WebM
	sent := sentInFile(t, out, cam)

	t.Logf("mkv: blocks=%d h264Keyframes=%d size=%dx%d pts=[%d,%d] maxGap=%dms; sent in its span=%d; plis=%v",
		mkv.Blocks, mkv.H264Keyframes, mkv.Width, mkv.Height, mkv.FirstPTS, mkv.LastPTS, mkv.MaxGapMs, sent, out.PLIs)

	require.Equal(t, oracle.CodecH264, mkv.Codec)
	assert.NoError(t, mkv.CheckH264())
	assert.Equal(t, [2]int{640, 360}, [2]int{mkv.Width, mkv.Height}, "the SPS's size in the track entry")
	assert.NoError(t, mkv.CheckMonotonic())
	assert.NoError(t, mkv.CheckNoDuplicatePTS())
	assert.Empty(t, out.PLIs, "recorder keyframe requests")
	assert.GreaterOrEqual(t, mkv.Blocks, int(h264ClipFPS*(stopSent.Sub(started)-startupAllowance).Seconds()), "frames written")
	assert.Equal(t, sent, mkv.Blocks, "blocks: the frames sent from the first block to the last")

	span := time.Duration(mkv.LastPTS-mkv.FirstPTS)*time.Millisecond + frameInterval
	t.Run("ffmpeg", func(t *testing.T) { checkDecodes(t, rec.Path, mkv.Blocks, span, frameInterval) })
	t.Run("pictures", func(t *testing.T) { checkClipPictures(t, rec.Path, h264ClipPath, mkv.Blocks) })
}

// sentInFile counts the frames tr sent from the frame of out's first block
// to that of its last. The file starts at the first key frame read, and
// LiveKit moves timestamps by a constant, so PTS steps map to the trace's
// timestamps (traceFrames).
func sentInFile(t *testing.T, out *Output, tr *PubTrack) int {
	t.Helper()

	frames := traceFrames(t, out.Trace, tr)
	l := readTraceLoss(out.Trace)
	require.NotEmpty(t, l.Keys, "key frames read")

	first := l.Keys[0].TS
	last := first + uint32((out.WebM.LastPTS-out.WebM.FirstPTS)*90)

	// PTS are whole ms: the last frame read within 1 ms of last is the
	// file's last.
	var read []uint32
	for ts := range frames {
		read = append(read, ts)
	}
	i := slices.IndexFunc(read, func(ts uint32) bool { d := int32(ts - last); return d >= 0 && d < 90 })
	require.GreaterOrEqual(t, i, 0, "no frame read at the last block's PTS")

	return frames[read[i]] - frames[first] + 1
}
