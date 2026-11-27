package recorder

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// frameTestCodec is a test codec with frames of its own, one per RTP
// timestamp. A payload starting with 'K' makes a key frame, and one with
// 'X' is malformed.
var frameTestCodec = &Codec{
	MimeType:        "video/x-test",
	Video:           true,
	ClockRate:       90000,
	MatroskaID:      "V_TEST",
	NewDepacketizer: func() rtp.Depacketizer { return &codecs.VP8Packet{} },
	KeyFrameStart:   func(p *rtp.Packet) bool { return len(p.Payload) > 0 && p.Payload[0] == 'K' },
	frames:          frameTestFrames,
}

const (
	frameTestWidth  = 320
	frameTestHeight = 180
)

var frameTestPrivate = []byte{0xCA, 0xFE}

func frameTestFrames(packets []*rtp.Packet) ([]videoFrame, error) {
	var frames []videoFrame
	var err error
	for _, p := range packets {
		if p.Payload[0] == 'X' {
			frames, err = append(frames, videoFrame{lost: true}), errMalformedTestFrame

			continue
		}

		if n := len(frames); n > 0 && frames[n-1].timestamp == p.Timestamp {
			frames[n-1].data = append(frames[n-1].data, p.Payload...)

			continue
		}

		f := videoFrame{data: bytes.Clone(p.Payload), timestamp: p.Timestamp, keyFrame: p.Payload[0] == 'K'}
		if f.keyFrame {
			f.width, f.height, f.codecPrivate = frameTestWidth, frameTestHeight, frameTestPrivate
		}
		frames = append(frames, f)
	}

	return frames, err
}

type malformedTestFrame struct{}

func (malformedTestFrame) Error() string { return "malformed test frame" }

var errMalformedTestFrame = malformedTestFrame{}

// frameRequester counts the writer's keyframe requests.
type frameRequester struct {
	mu sync.Mutex
	n  int
}

func (r *frameRequester) RequestKeyframe() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
}

func (r *frameRequester) RequestKeyframeForSSRC(uint32) { r.RequestKeyframe() }

func (r *frameRequester) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.n
}

// frameTracker is a frameRequester that also reports a request a track
// buffer made at requestedAt.
type frameTracker struct {
	frameRequester
	requestedAt time.Time
}

func (r *frameTracker) KeyframeRequestedSince(_ uint32, since time.Time) bool {
	return r.requestedAt.After(since)
}

func newFrameRecorder(t *testing.T) (*WebmRecorder, *frameRequester) {
	t.Helper()

	rec := NewWebmRecorder(filepath.Join(t.TempDir(), "frames.mkv"), 0o600, 256, 32, true, false)
	rec.WithContext(testSessionContext())
	rec.SetHasVideo(true)
	require.NoError(t, rec.SetVideoCodec(frameTestCodec))

	req := &frameRequester{}
	rec.SetKeyframeRequester(req)

	return rec, req
}

// frameSample is a sample of one frame at ts, one packet per payload from
// seq on, all arriving now.
func frameSample(follows bool, seq uint16, ts uint32, payloads ...string) VideoSample {
	s := VideoSample{Follows: follows}
	for i, p := range payloads {
		s.Packets = append(s.Packets, ArrivedPacket{
			Packet: &rtp.Packet{
				Header:  rtp.Header{SequenceNumber: seq + uint16(i), Timestamp: ts, SSRC: 1234, Marker: true},
				Payload: []byte(p),
			},
			At: time.Now(),
		})
	}

	return s
}

type writtenBlock struct {
	key  bool
	pts  int64
	data string
}

func readVideoBlocks(t *testing.T, file string) []writtenBlock {
	t.Helper()

	entries, read, err := oracle.ReadBlocks(file)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	var blocks []writtenBlock
	for _, b := range read {
		blocks = append(blocks, writtenBlock{key: b.Key, pts: b.PTS, data: string(b.Data)})
	}

	return blocks
}

// The recorder starts a Cluster every 32.767 s, so a 40 s recording has
// more than one.
func TestBlockReadersReadEveryCluster(t *testing.T) {
	rec, _ := newFrameRecorder(t)
	const frames = 600 // 40 s at 15 fps
	for i := range frames {
		rec.PushVideoSample(frameSample(i > 0, uint16(i), uint32(i)*6000, "K"))
	}
	rec.Close()

	file := rec.GetFilePath()
	require.Greater(t, len(readContainer(t, file).Segment.Cluster), 1)

	blocks := readVideoBlocks(t, file)
	require.Len(t, blocks, frames)
	assert.Equal(t, int64(39933), blocks[frames-1].pts)

	tracks := readBlocks(t, file)
	require.Len(t, tracks, 1)
	require.Len(t, tracks[0], frames)
	assert.Equal(t, int64(39933), tracks[0][frames-1])
}

func TestFrameWriterWaitsForAKeyFrame(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, req := newFrameRecorder(t)

		rec.PushVideoSample(frameSample(false, 1, 0, "D0"))
		assert.Equal(t, 1, req.count(), "a request before the first key frame")
		assert.NoFileExists(t, rec.GetFilePath(), "no file before a key frame")

		rec.PushVideoSample(frameSample(true, 2, 3000, "K1"))
		rec.PushVideoSample(frameSample(true, 3, 6000, "D2"))
		rec.Close()

		assert.Equal(t, []writtenBlock{{true, 0, "K1"}, {false, 33, "D2"}}, readVideoBlocks(t, rec.GetFilePath()))
		assert.Equal(t, 1, req.count())

		doc := readContainer(t, rec.GetFilePath())
		assert.Equal(t, "matroska", doc.Header.DocType)
		require.Len(t, doc.Segment.Tracks.TrackEntry, 1)
		entry := doc.Segment.Tracks.TrackEntry[0]
		assert.Equal(t, "V_TEST", entry.CodecID)
		assert.Equal(t, frameTestPrivate, entry.CodecPrivate)
		assert.Equal(t, uint64(frameTestWidth), entry.Video.PixelWidth)
		assert.Equal(t, uint64(frameTestHeight), entry.Video.PixelHeight)

		stats := rec.GetStats().Video
		assert.Equal(t, 2, stats.WrittenSamples)
		assert.Equal(t, 1, stats.KeyframeCount)
	})
}

func TestFrameWriterResumesAtAKeyFrameAfterASampleThatDoesNotFollow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, req := newFrameRecorder(t)

		rec.PushVideoSample(frameSample(false, 1, 0, "K0"))
		rec.PushVideoSample(frameSample(true, 2, 3000, "D1"))
		// Packets 3 and 4 were lost.
		rec.PushVideoSample(frameSample(false, 5, 9000, "D3"))
		assert.Equal(t, 1, req.count(), "the loss requests a key frame")
		rec.PushVideoSample(frameSample(true, 6, 12000, "D4"))
		rec.PushVideoSample(frameSample(true, 7, 15000, "K5"))
		rec.PushVideoSample(frameSample(true, 8, 18000, "D6"))
		rec.Close()

		assert.Equal(t, []writtenBlock{
			{true, 0, "K0"}, {false, 33, "D1"}, {true, 166, "K5"}, {false, 200, "D6"},
		}, readVideoBlocks(t, rec.GetFilePath()))
		assert.Equal(t, 1, req.count(), "no repeat while the request is due")
	})
}

func TestFrameWriterCountsTheSequenceGapBeforeASample(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, _ := newFrameRecorder(t)

		rec.PushVideoSample(frameSample(false, 1, 0, "K0"))
		rec.PushVideoSample(frameSample(true, 2, 3000, "D1"))
		// Packets 3 and 4 were lost.
		rec.PushVideoSample(frameSample(false, 5, 12000, "K4"))
		rec.Close()

		gaps := rec.GetStats().Video.RTPDiscontInfo
		assert.Equal(t, 1, gaps.Count)
		assert.Equal(t, uint16(2), gaps.MaxGap)
	})
}

func TestFrameWriterResumesAtAKeyFrameAfterAMalformedFrame(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, req := newFrameRecorder(t)

		rec.PushVideoSample(frameSample(false, 1, 0, "K0"))
		rec.PushVideoSample(frameSample(true, 2, 3000, "D1", "X"))
		rec.PushVideoSample(frameSample(true, 4, 6000, "D2"))
		rec.PushVideoSample(frameSample(true, 5, 9000, "K3"))
		rec.Close()

		assert.Equal(t, []writtenBlock{
			{true, 0, "K0"}, {false, 33, "D1"}, {true, 100, "K3"},
		}, readVideoBlocks(t, rec.GetFilePath()))
		assert.Equal(t, 1, req.count())
	})
}

func TestFrameWriterLeavesToTheTrackerOnlyRequestsAfterTheLastKeyFrame(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, _ := newFrameRecorder(t)
		tracker := &frameTracker{}
		rec.SetKeyframeRequester(tracker)

		time.Sleep(100 * time.Millisecond)
		tracker.requestedAt = time.Now()
		time.Sleep(100 * time.Millisecond)
		rec.PushVideoSample(frameSample(false, 1, 0, "K0"))
		time.Sleep(100 * time.Millisecond)
		rec.PushVideoSample(frameSample(true, 2, 9000, "X"))
		assert.Equal(t, 1, tracker.count(), "K0 answered the tracker's request")

		time.Sleep(2 * time.Second)
		tracker.requestedAt = time.Now()
		rec.PushVideoSample(frameSample(true, 3, 18000, "X"))
		assert.Equal(t, 1, tracker.count(), "the tracker's new request is due")
		rec.Close()
	})
}

func TestFrameWriterDropsAFrameWhoseTimestampGoesBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, req := newFrameRecorder(t)

		rec.PushVideoSample(frameSample(false, 1, 9000, "K0"))
		rec.PushVideoSample(frameSample(true, 2, 6000, "D1"))
		assert.Equal(t, 1, req.count())
		// D2 may reference D1, so the writer waits for a key frame.
		rec.PushVideoSample(frameSample(true, 3, 12000, "D2"))
		rec.PushVideoSample(frameSample(true, 4, 15000, "K3"))
		rec.Close()

		assert.Equal(t, []writtenBlock{{true, 0, "K0"}, {true, 66, "K3"}}, readVideoBlocks(t, rec.GetFilePath()))
		assert.Equal(t, 1, req.count())
	})
}

func TestFrameWriterResumesAtAKeyFrameBehindTheLastFrame(t *testing.T) {
	const frame = 40 * time.Millisecond

	cases := []struct {
		name  string
		gap   time.Duration // the wait between pushing the second frame and the key frame
		early time.Duration // how much sooner than its push the key frame arrived
		back  uint32        // ticks the key frame's timestamp is behind the second frame's
		want  []writtenBlock
	}{
		{"clock steps back 60 s", 2 * frame, 0, 60*90000 - 3600, []writtenBlock{
			{true, 0, "K0"}, {false, 40, "D1"}, {true, 120, "K3"}, {false, 160, "D4"},
		}},
		// A jump of 2^31 to 2^32 ticks reads as a step back.
		{"7 hours forward", 7 * time.Hour, 0, 1<<32 - 7*3600*90000, []writtenBlock{
			{true, 0, "K0"}, {false, 40, "D1"}, {true, 25200040, "K3"}, {false, 25200080, "D4"},
		}},
		// The key frame arrives with D1, as in a burst after a stall.
		{"no arrival gap", 0, 0, 60*90000 - 3600, []writtenBlock{
			{true, 0, "K0"}, {false, 40, "D1"}, {true, 41, "K3"}, {false, 81, "D4"},
		}},
		// D1 was resent, so the key frame arrived before it.
		{"last frame resent late", 0, 30 * time.Millisecond, 60*90000 - 3600, []writtenBlock{
			{true, 0, "K0"}, {false, 40, "D1"}, {true, 41, "K3"}, {false, 81, "D4"},
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rec, _ := newFrameRecorder(t)
				const ts = 9000000

				rec.PushVideoSample(frameSample(false, 1, ts, "K0"))
				time.Sleep(frame)
				rec.PushVideoSample(frameSample(true, 2, ts+3600, "D1"))
				time.Sleep(c.gap)
				rec.PushVideoSample(frameSample(true, 3, ts-c.back, "D2"))
				k3 := frameSample(true, 4, ts+3600-c.back, "K3")
				k3.Packets[0].At = k3.Packets[0].At.Add(-c.early)
				rec.PushVideoSample(k3)
				time.Sleep(frame)
				rec.PushVideoSample(frameSample(true, 5, ts+7200-c.back, "D4"))
				rec.Close()

				assert.Equal(t, c.want, readVideoBlocks(t, rec.GetFilePath()))
			})
		})
	}
}

func TestFrameWriterTakesAFramesArrivalFromItsEarliestPacket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, _ := newFrameRecorder(t)
		start := time.Now()

		rec.PushVideoSample(frameSample(false, 1, 9000000, "K0"))
		time.Sleep(80 * time.Millisecond)
		// One sample holds D1 and K2, which is behind it. K2's first packet
		// was resent, so its second one arrived first, 20 ms after D1, and
		// its third 10 ms later.
		s := frameSample(true, 2, 9003600, "D1")
		s.Packets = append(s.Packets, frameSample(true, 3, 1000, "K2", "b", "c").Packets...)
		s.Packets[0].At = start.Add(40 * time.Millisecond)
		s.Packets[2].At = start.Add(60 * time.Millisecond)
		s.Packets[3].At = start.Add(70 * time.Millisecond)
		rec.PushVideoSample(s)
		rec.Close()

		assert.Equal(t, []writtenBlock{{true, 0, "K0"}, {false, 40, "D1"}, {true, 60, "K2bc"}},
			readVideoBlocks(t, rec.GetFilePath()))
	})
}

func TestFrameWriterStatsGiveNoIntervalToAFrameWhoseTimestampGoesBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, _ := newFrameRecorder(t)

		rec.PushVideoSample(frameSample(false, 1, 9000, "K0"))
		rec.PushVideoSample(frameSample(true, 2, 6000, "D1"))
		rec.PushVideoSample(frameSample(true, 3, 12000, "K2"))
		rec.Close()

		// The field holds milliseconds in a time.Duration.
		assert.Equal(t, time.Duration(66), rec.GetStats().Video.MaxSampleDurationMs)
	})
}

func TestFrameWriterDropsAFrameOverMaxFrameSize(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, req := newFrameRecorder(t)
		big := "K" + strings.Repeat("0", rec.maxFrameSize)

		rec.PushVideoSample(frameSample(false, 1, 0, "K0"))
		logged := warnings(func() {
			rec.PushVideoSample(frameSample(true, 2, 3000, big))
			// D2 may reference the frame dropped.
			rec.PushVideoSample(frameSample(true, 3, 6000, "D2"))
			rec.PushVideoSample(frameSample(true, 4, 9000, big))
		})
		rec.PushVideoSample(frameSample(true, 5, 12000, "K4"))
		rec.Close()

		var pts []int64
		for _, b := range readVideoBlocks(t, rec.GetFilePath()) {
			pts = append(pts, b.pts)
		}
		assert.Equal(t, []int64{0, 133}, pts)
		assert.Equal(t, 1, req.count())
		assert.Equal(t, []string{"Discarding video/x-test frame of 10485761 bytes: the limit is 10485760"}, logged)
	})
}

func TestFrameWriterWarnsOnceOfEachKindOfBadFrame(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, _ := newFrameRecorder(t)
		rec.PushVideoSample(frameSample(false, 1, 0, "K"))

		logged := warnings(func() {
			for i := range uint32(2) {
				seq, ts := uint16(2+5*i), 9000*(i+1)
				rec.PushVideoSample(frameSample(true, seq, ts, "K"))
				rec.PushVideoSample(frameSample(true, seq+1, ts+1, "K"))
				rec.PushVideoSample(frameSample(true, seq+2, ts-3000, "D"))
				rec.PushVideoSample(frameSample(true, seq+3, ts, "X"))
				rec.PushVideoSample(frameSample(true, seq+4, ts-2000, "K"))
			}
		})
		rec.Close()

		assert.Equal(t, []string{
			"Duplicate frame detected: pts=100, ts=9001",
			"Skipping video frame whose timestamp goes back",
			"Discarding malformed video/x-test frame: malformed test frame",
			"Video timestamps restart at a key frame",
		}, logged)
	})
}

func TestFrameWriterKeepsMultipleFramesOfASample(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, _ := newFrameRecorder(t)

		s := frameSample(false, 1, 0, "K0")
		s.Packets = append(s.Packets, frameSample(true, 2, 3000, "D1").Packets...)
		rec.PushVideoSample(s)
		rec.Close()

		assert.Equal(t, []writtenBlock{{true, 0, "K0"}, {false, 33, "D1"}}, readVideoBlocks(t, rec.GetFilePath()))
	})
}

func TestFrameWriterOpensTheFileOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, _ := newFrameRecorder(t)

		rec.PushVideoSample(frameSample(false, 1, 0, "K0"))
		rec.PushVideoSample(frameSample(true, 2, 3000, "D1"))
		rec.PushVideoSample(frameSample(true, 3, 6000, "K2"))
		rec.Close()

		assert.Len(t, readVideoBlocks(t, rec.GetFilePath()), 3)
	})
}

func TestFrameWriterRateLimitsRequestsBeforeTheFirstKeyFrame(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, req := newFrameRecorder(t)

		for i := range 60 {
			rec.PushVideoSample(frameSample(i > 0, uint16(1+i), uint32(i)*3000, "D"))
			time.Sleep(33 * time.Millisecond)
		}
		rec.Close()

		assert.Equal(t, 2, req.count(), "one a second for 2 s")
	})
}

func TestFrameWriterPlacesTheMediaSpan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, _ := newFrameRecorder(t)
		start := time.Now()

		for i := range 3 {
			rec.PushVideoSample(frameSample(i > 0, uint16(1+i), uint32(i)*3000, map[bool]string{true: "K", false: "D"}[i == 0]))
			time.Sleep(33333 * time.Microsecond)
		}

		span, ok := rec.MediaSpan()
		require.True(t, ok)
		assert.Equal(t, start, span.Start)
		rec.Close()
	})
}

func TestFrameWriterIgnoresSamplesAfterClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, _ := newFrameRecorder(t)

		rec.PushVideoSample(frameSample(false, 1, 0, "K0"))
		rec.Close()
		rec.PushVideoSample(frameSample(true, 2, 3000, "D1"))

		assert.Len(t, readVideoBlocks(t, rec.GetFilePath()), 1)
	})
}

func TestPushVideoSampleWhileTheCodecChanges(t *testing.T) {
	rec, _ := newFrameRecorder(t)
	defer rec.Close()

	// Delta frames only, so the file never opens and SetVideoCodec keeps
	// succeeding.
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 20000 {
			rec.PushVideoSample(frameSample(true, uint16(i), uint32(i)*3000, "D"))
		}
	})
	wg.Go(func() {
		for range 20000 {
			assert.NoError(t, rec.SetVideoCodec(CodecVP8))
			assert.NoError(t, rec.SetVideoCodec(frameTestCodec))
		}
	})
	wg.Wait()
}

func TestPushVideoSampleOfAPerPacketCodecPushesItsPackets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		vp8, err := corpus.GenerateVP8(corpus.VP8Options{FPS: 30, Frames: 3})
		require.NoError(t, err)

		bySample := NewWebmRecorder(filepath.Join(dir, "sample.webm"), 0o600, 256, 32, true, false)
		bySample.SetHasVideo(true)
		byPacket := NewWebmRecorder(filepath.Join(dir, "packet.webm"), 0o600, 256, 32, true, false)
		byPacket.SetHasVideo(true)

		s := VideoSample{}
		for _, r := range vp8.Records {
			byPacket.PushVideoAt(r.Packet(), time.Now())
			s.Packets = append(s.Packets, ArrivedPacket{Packet: r.Packet(), At: time.Now()})
		}
		bySample.PushVideoSample(s)
		bySample.Close()
		byPacket.Close()

		a, err := os.ReadFile(bySample.GetFilePath())
		require.NoError(t, err)
		b, err := os.ReadFile(byPacket.GetFilePath())
		require.NoError(t, err)
		assert.Equal(t, b, a)
	})
}

func TestSetVideoCodec(t *testing.T) {
	webmRec := NewWebmRecorder(filepath.Join(t.TempDir(), "x.webm"), 0o600, 256, 32, true, false)
	assert.Error(t, webmRec.SetVideoCodec(frameTestCodec), "WebM cannot hold the codec")
	assert.NoError(t, webmRec.SetVideoCodec(CodecVP8))
	assert.Error(t, webmRec.SetVideoCodec(CodecOpus), "not a video codec")

	synctest.Test(t, func(t *testing.T) {
		rec, _ := newFrameRecorder(t)
		assert.NoError(t, rec.SetVideoCodec(CodecVP8), "nothing written yet")
		assert.NoError(t, rec.SetVideoCodec(frameTestCodec))

		rec.PushVideoSample(frameSample(false, 1, 0, "K0"))
		assert.NoError(t, rec.SetVideoCodec(frameTestCodec), "the codec the file holds")
		assert.Error(t, rec.SetVideoCodec(CodecVP8), "the file holds another codec")
		rec.Close()
	})
}

func TestSetFormatPicksTheExtension(t *testing.T) {
	dir := t.TempDir()
	rec := NewWebmRecorder(filepath.Join(dir, "x.webm"), 0o600, 256, 32, true, false)
	assert.Equal(t, FormatWebM, rec.Format())

	require.NoError(t, rec.SetFormat(FormatMKV))
	assert.Equal(t, FormatMKV, rec.Format())
	assert.Equal(t, filepath.Join(dir, "x.mkv"), rec.GetFilePath())

	require.NoError(t, rec.SetFormat(FormatWebM))
	assert.Equal(t, filepath.Join(dir, "x.webm"), rec.GetFilePath())

	require.NoError(t, rec.SetVideoCodec(CodecVP8))
	require.NoError(t, rec.SetFormat(FormatMKV))
	require.NoError(t, rec.SetVideoCodec(frameTestCodec))
	assert.Error(t, rec.SetFormat(FormatWebM), "WebM cannot hold the codec set")

	devNull := NewWebmRecorder(os.DevNull, 0o600, 256, 32, true, false)
	require.NoError(t, devNull.SetFormat(FormatMKV))
	assert.Equal(t, os.DevNull, devNull.GetFilePath())
}

// The server reads the path for getRecordings while the LiveKit adapter's
// Init sets the format.
func TestGetFilePathWhileSetFormatRuns(t *testing.T) {
	dir := t.TempDir()
	rec := NewWebmRecorder(filepath.Join(dir, "x.webm"), 0o600, 256, 32, true, false)

	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 100 {
			if !assert.NoError(t, rec.SetFormat([]Format{FormatMKV, FormatWebM}[i%2])) {
				return
			}
		}
	})
	for range 100 {
		assert.Contains(t, []string{filepath.Join(dir, "x.webm"), filepath.Join(dir, "x.mkv")}, rec.GetFilePath())
	}
	wg.Wait()
}

// See fileMu.
func TestGetFilePathDoesNotWaitForTheRecorderLock(t *testing.T) {
	rec := NewWebmRecorder(filepath.Join(t.TempDir(), "x.webm"), 0o600, 256, 32, true, false)
	rec.m.Lock()
	defer rec.m.Unlock()

	got := make(chan string, 1)
	go func() { got <- rec.GetFilePath() }()

	select {
	case <-got:
	case <-time.After(2 * time.Second):
		assert.Fail(t, "GetFilePath waits for the recorder's lock")
	}
}

// Either format holds VP8, so only the open file refuses the change.
func TestSetFormatFailsOnceTheFileIsOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "camera.webm")
		rec := NewWebmRecorder(file, 0o600, 256, 32, true, false)
		rec.SetHasVideo(true)

		vp8, err := corpus.GenerateVP8(corpus.VP8Options{FPS: 30, Frames: 3})
		require.NoError(t, err)
		for _, r := range vp8.Records {
			rec.PushVideoAt(r.Packet(), time.Now())
		}
		require.FileExists(t, file)

		assert.NoError(t, rec.SetFormat(FormatWebM), "the format it has")
		assert.Error(t, rec.SetFormat(FormatMKV))
		assert.Equal(t, file, rec.GetFilePath())
		rec.Close()
	})
}

func TestFormatFor(t *testing.T) {
	for _, c := range []struct {
		mimeTypes []string
		want      Format
	}{
		{nil, FormatWebM},
		{[]string{"video/vp8"}, FormatWebM},
		{[]string{"video/VP8", "audio/opus"}, FormatWebM},
		{[]string{"video/vp9"}, FormatWebM},
		{[]string{"video/av1"}, FormatWebM},
		{[]string{"", "video/vp8"}, FormatWebM},
		{[]string{"video/h264"}, FormatMKV},
		{[]string{"video/H264", "video/vp8"}, FormatMKV},
		// A codec the recorder does not write fails its track in either format.
		{[]string{"video/vp8", "video/h265"}, FormatWebM},
		{[]string{"video/h265"}, FormatWebM},
		{[]string{"audio/red"}, FormatWebM},
	} {
		assert.Equal(t, c.want, FormatFor(c.mimeTypes), "%v", c.mimeTypes)
	}
}

func TestNewRecorderIgnoresTheRequestedExtension(t *testing.T) {
	dir := t.TempDir()
	cfg := (&config.Config{}).GetDefaults().Recorder
	cfg.Directory = dir

	for name, want := range map[string]string{
		"a/one.webm": "a/one.webm",
		"a/two.mkv":  "a/two.webm",
		"a/three":    "a/three.webm",
		"a/four.ogg": "a/four.ogg.webm",
	} {
		rec, err := NewRecorder(testSessionContext(), cfg, name)
		require.NoError(t, err, name)
		assert.Equal(t, filepath.Join(dir, want), rec.GetFilePath(), name)
		assert.Equal(t, FormatWebM, rec.(*WebmRecorder).Format(), name)
	}
}

func TestNewRecorderWritesToDevNullInEitherFormat(t *testing.T) {
	cfg := (&config.Config{}).GetDefaults().Recorder
	cfg.Directory = t.TempDir()
	cfg.WriteToDevNull = true

	rec, err := NewRecorder(testSessionContext(), cfg, "a/camera.webm")
	require.NoError(t, err)
	assert.Equal(t, os.DevNull, rec.GetFilePath())

	require.NoError(t, rec.SetFormat(FormatMKV))
	require.NoError(t, rec.SetVideoCodec(CodecH264))
	assert.Equal(t, os.DevNull, rec.GetFilePath())

	rec.SetHasVideo(true)
	assert.NotPanics(t, func() {
		rec.PushVideoSample((&h264Stream{seq: 1}).sample(false, testAU(t, 0)))
		rec.Close()
	})
	assert.Equal(t, 1, rec.GetStats().Video.WrittenSamples)
}

func TestNewRecorderRefusesANameNotUnderTheDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "rec")
	require.NoError(t, os.Mkdir(dir, 0o700))
	cfg := (&config.Config{}).GetDefaults().Recorder
	cfg.Directory = dir

	for _, name := range []string{"", "/", ".webm", ".mkv", "a/..", "../x", "a/../../x", "../x.webm", "../b/x", "/../x", "../rec", "../rec.webm"} {
		_, err := NewRecorder(testSessionContext(), cfg, name)
		assert.Error(t, err, "%q", name)
	}
	assert.NoDirExists(t, filepath.Join(root, "b"))

	rec, err := NewRecorder(testSessionContext(), cfg, "a/../x")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "x.webm"), rec.GetFilePath())
}

func TestNewRecorderRefusesANameThatEndsInADirectory(t *testing.T) {
	cfg := (&config.Config{}).GetDefaults().Recorder
	cfg.Directory = t.TempDir()

	for _, name := range []string{"sub/.webm", "sub/.mkv", "sub/", "sub/.", "sub/a/.."} {
		_, err := NewRecorder(testSessionContext(), cfg, name)
		assert.Error(t, err, "%q", name)
	}
}

func TestNewRecorderRefusesANameInUseWithEitherExtension(t *testing.T) {
	dir := t.TempDir()
	cfg := (&config.Config{}).GetDefaults().Recorder
	cfg.Directory = dir
	require.NoError(t, os.WriteFile(filepath.Join(dir, "used.mkv"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "taken.webm"), nil, 0o600))

	for _, name := range []string{"used", "used.webm", "taken", "taken.mkv"} {
		_, err := NewRecorder(testSessionContext(), cfg, name)
		assert.Error(t, err, name)
	}
}

func TestCodecsOfARecording(t *testing.T) {
	rec := NewWebmRecorder(filepath.Join(t.TempDir(), "x.webm"), 0o600, 256, 32, true, false)
	assert.Empty(t, rec.Codecs())

	rec.SetHasVideo(true)
	rec.SetHasAudio(true)
	assert.Equal(t, []string{"video/vp8", "audio/opus"}, rec.Codecs())

	require.NoError(t, rec.SetFormat(FormatMKV))
	require.NoError(t, rec.SetVideoCodec(frameTestCodec))
	assert.Equal(t, []string{"video/x-test", "audio/opus"}, rec.Codecs())
}
