package recorder

import (
	"bytes"
	"cmp"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pion/rtp"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The STAP-A LiveKit sends as a blank key frame when a downtrack closes
// (livekit-server pkg/sfu/downtrack.go, H264KeyFrame2x2).
var (
	h264Blank2x2SPS = []byte{
		0x67, 0x42, 0xc0, 0x1f, 0x0f, 0xd9, 0x1f, 0x88, 0x88, 0x84, 0x00, 0x00,
		0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xc8, 0x3c, 0x60, 0xc9, 0x20,
	}
	h264Blank2x2PPS = []byte{0x68, 0x87, 0xcb, 0x83, 0xcb, 0x20}
	h264Blank2x2IDR = []byte{0x65, 0x88, 0x84, 0x0a, 0xf2, 0x62, 0x80, 0x00, 0xa7, 0xbe}
)

// h264Stream pushes access units to a recorder as samples, one per unit,
// with consecutive sequence numbers and 15 fps timestamps.
type h264Stream struct {
	rec *WebmRecorder
	seq uint16
	ts  uint32
	mtu int
}

func (s *h264Stream) sample(follows bool, nals [][]byte) VideoSample {
	sample := VideoSample{Follows: follows}
	payloads := h264Packetize(nals, cmp.Or(s.mtu, 1200))
	for i, p := range payloads {
		sample.Packets = append(sample.Packets, ArrivedPacket{
			Packet: &rtp.Packet{
				Header:  rtp.Header{SequenceNumber: s.seq, Timestamp: s.ts, SSRC: 1234, Marker: i == len(payloads)-1},
				Payload: bytes.Clone(p),
			},
			At: time.Now(),
		})
		s.seq++
	}
	s.ts += 6000

	return sample
}

func (s *h264Stream) push(follows bool, nals [][]byte) {
	s.rec.PushVideoSample(s.sample(follows, nals))
	time.Sleep(time.Second / 15)
}

func newH264Recorder(t *testing.T, ivf bool) (*WebmRecorder, *frameRequester) {
	t.Helper()

	rec := NewWebmRecorder(filepath.Join(t.TempDir(), "camera.mkv"), 0o600, 256, 32, true, ivf)
	rec.WithContext(testSessionContext())
	rec.SetHasVideo(true)
	require.NoError(t, rec.SetVideoCodec(CodecH264))

	req := &frameRequester{}
	rec.SetKeyframeRequester(req)

	return rec, req
}

// avccOf is the AVCC block data for nals: AUD and filler dropped.
func avccOf(nals [][]byte) string {
	return string(wantUnit(0, 0, 0, nals).data)
}

// ffmpegDecode decodes file with ffmpeg and returns the errors ffmpeg
// printed and the frames it decoded. It skips t on a host without ffmpeg.
func ffmpegDecode(t *testing.T, file string) (errs string, frames int) {
	t.Helper()

	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("no %s on PATH", bin)
		}
	}

	// -xerror and -err_detect explode make ffmpeg fail on a frame it would
	// conceal, such as one that misses a slice.
	out, err := exec.Command("ffmpeg", "-v", "error", "-xerror", "-err_detect", "explode",
		"-i", file, "-f", "null", "-").CombinedOutput()
	require.NoError(t, err, "%s", out)

	count, err := exec.Command("ffprobe", "-v", "error", "-count_frames", "-select_streams", "v:0",
		"-show_entries", "stream=nb_read_frames", "-of", "csv=p=0", file).Output()
	require.NoError(t, err)

	n, err := strconv.Atoi(strings.TrimSpace(string(count)))
	require.NoError(t, err)

	return string(out), n
}

func TestWebmRecorderWritesH264ToMatroska(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, req := newH264Recorder(t, false)
		s := &h264Stream{rec: rec, seq: 100, ts: 90000}

		for i := range h264TestAUs {
			s.push(i > 0, testAU(t, i))
		}
		rec.Close()

		doc := readContainer(t, rec.GetFilePath())
		assert.Equal(t, "matroska", doc.Header.DocType)
		require.Len(t, doc.Segment.Tracks.TrackEntry, 1)
		entry := doc.Segment.Tracks.TrackEntry[0]
		assert.Equal(t, "V_MPEG4/ISO/AVC", entry.CodecID)
		assert.Equal(t, uint64(320), entry.Video.PixelWidth)
		assert.Equal(t, uint64(180), entry.Video.PixelHeight)

		first := wantUnit(0, 0, 0, testAU(t, 0))
		private, err := h264CodecPrivate(first.sps, first.pps)
		require.NoError(t, err)
		assert.Equal(t, private, entry.CodecPrivate)

		assert.Equal(t, []writtenBlock{
			{true, 0, avccOf(testAU(t, 0))},
			{false, 66, avccOf(testAU(t, 1))},
			{false, 133, avccOf(testAU(t, 2))},
		}, readVideoBlocks(t, rec.GetFilePath()))
		assert.Zero(t, req.count())
		assert.Equal(t, []string{"video/h264"}, rec.Codecs())
	})
}

func TestWebmRecorderH264DecodesWithFFmpeg(t *testing.T) {
	// At 1200 bytes the slices of a P frame share one STAP-A. At 100 the
	// second slice of each P frame goes as FU-A fragments.
	for _, mtu := range []int{1200, 100} {
		t.Run("mtu="+strconv.Itoa(mtu), func(t *testing.T) {
			rec, _ := newH264Recorder(t, false)
			s := &h264Stream{rec: rec, seq: 65530, ts: 0xFFFFF000, mtu: mtu}
			for i := range h264TestAUs {
				rec.PushVideoSample(s.sample(i > 0, testAU(t, i)))
			}
			rec.Close()

			errs, frames := ffmpegDecode(t, rec.GetFilePath())
			assert.Empty(t, errs)
			assert.Equal(t, len(h264TestAUs), frames)

			// BBB's archive remuxes each file to its own extension.
			remuxed := filepath.Join(t.TempDir(), "remuxed.mkv")
			out, err := exec.Command("ffmpeg", "-v", "error", "-i", rec.GetFilePath(), "-c", "copy", remuxed).CombinedOutput()
			require.NoError(t, err, "%s", out)
		})
	}
}

// A 90 kHz timestamp wraps every 13.3 hours, and senders start it at a
// random value.
func TestWebmRecorderH264KeepsThePTSAcrossATimestampWrap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, _ := newH264Recorder(t, false)
		s := &h264Stream{rec: rec, seq: 1, ts: 0xFFFFF000}
		for i := range h264TestAUs {
			s.push(i > 0, testAU(t, i))
		}
		require.Less(t, s.ts, uint32(0xFFFFF000), "the timestamp wrapped")
		rec.Close()

		var pts []int64
		for _, b := range readVideoBlocks(t, rec.GetFilePath()) {
			pts = append(pts, b.pts)
		}
		assert.Equal(t, []int64{0, 66, 133}, pts)
	})
}

func TestWebmRecorderH264WaitsForAKeyFrameWithParameterSets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, req := newH264Recorder(t, false)
		s := &h264Stream{rec: rec, seq: 1, ts: 0}

		// A delta frame, then an IDR whose STAP-A of SPS and PPS was lost.
		s.push(false, testAU(t, 1))
		idr := testAU(t, 0)
		s.push(false, [][]byte{idr[0], idr[3], idr[4]})
		assert.NoFileExists(t, rec.GetFilePath())
		assert.Equal(t, 1, req.count(), "rate-limited to one a second")

		s.push(false, testAU(t, 0))
		s.push(true, testAU(t, 1))
		rec.Close()

		blocks := readVideoBlocks(t, rec.GetFilePath())
		require.Len(t, blocks, 2)
		assert.True(t, blocks[0].key)
		assert.Equal(t, int64(0), blocks[0].pts)
		assert.Equal(t, int64(66), blocks[1].pts)
	})
}

func TestWebmRecorderH264DropsBlankKeyFrames(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, _ := newH264Recorder(t, false)
		s := &h264Stream{rec: rec, seq: 1, ts: 0}

		s.push(false, testAU(t, 0))
		s.push(true, testAU(t, 1))
		blank := [][]byte{h264Blank2x2SPS, h264Blank2x2PPS, h264Blank2x2IDR}
		s.push(true, blank)
		s.push(true, blank)
		// A delta frame after them cannot be decoded either.
		s.push(true, testAU(t, 2))
		rec.Close()

		blocks := readVideoBlocks(t, rec.GetFilePath())
		assert.Len(t, blocks, 2, "only the frames before the blank ones")

		doc := readContainer(t, rec.GetFilePath())
		assert.Equal(t, uint64(320), doc.Segment.Tracks.TrackEntry[0].Video.PixelWidth)
	})
}

func TestWebmRecorderH264NeverStartsAtABlankKeyFrame(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, _ := newH264Recorder(t, false)
		s := &h264Stream{rec: rec, seq: 1, ts: 0}

		s.push(false, [][]byte{h264Blank2x2SPS, h264Blank2x2PPS, h264Blank2x2IDR})
		assert.NoFileExists(t, rec.GetFilePath())
		rec.Close()
	})
}

func TestWebmRecorderH264WarnsOnceOfEachRefusedKeyFrame(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, _ := newH264Recorder(t, false)
		s := &h264Stream{rec: rec, seq: 1, ts: 0}
		au := testAU(t, 0)
		sps, pps, idr := au[1], au[2], au[3]
		oversized := (&testSPS{profile: 66, constraints: 0xc0, level: 30, widthMBsMinus1: 512, heightMUsMinus1: 511}).nal()
		deep := (&testSPS{profile: 100, level: 30, chromaFormat: 1, lumaMinus8: 8, widthMBsMinus1: 19, heightMUsMinus1: 14}).nal()

		s.push(false, au)
		logged := warnings(func() {
			// A layer switch: a frame cut short, then the new layer's key frame.
			sample := s.sample(true, testAU(t, 2)[:2])
			sample.Packets[len(sample.Packets)-1].Marker = false
			s.ts -= 6000 - 1
			sample.Packets = append(sample.Packets, s.sample(true, [][]byte{oversized, pps, idr}).Packets...)
			rec.PushVideoSample(sample)

			for range 2 {
				s.push(true, [][]byte{oversized, pps, idr})
				s.push(true, [][]byte{sps[:6], pps, idr})
				s.push(true, [][]byte{deep, pps, idr})
				s.push(true, [][]byte{h264Blank2x2SPS, h264Blank2x2PPS, h264Blank2x2IDR})
				s.push(true, au) // a valid key frame does not reset the limit
			}
		})
		rec.Close()

		assert.Equal(t, []string{
			"Discarding invalid video/h264 frame: IDR at timestamp 6001: invalid dimensions: 8208x8192",
			"Discarding invalid video/h264 frame: IDR at timestamp 18001: unknown dimensions: SPS: read past the end",
			"Discarding invalid video/h264 frame: IDR at timestamp 24001: parameter sets avcC cannot hold: SPS bit depths 16/8 do not fit avcC",
		}, logged, "blank key frames and the cut frame stay at Debug")
	})
}

func TestWebmRecorderH264SkipsOrphanParameterSets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, _ := newH264Recorder(t, false)

		// The simulcast start TestH264AccessUnitsOrphanParameterSets
		// describes, as the writer gets it.
		low := mustHex(t, h264TestSPS[0].nal)
		lowPPS := mustHex(t, h264TestSPS[0].pps)
		orphan := &h264Stream{rec: rec, seq: 1, ts: 1000}
		sample := orphan.sample(false, [][]byte{low, lowPPS})
		sample.Packets[len(sample.Packets)-1].Marker = false
		key := &h264Stream{rec: rec, seq: orphan.seq, ts: 1001}
		sample.Packets = append(sample.Packets, key.sample(true, testAU(t, 0)).Packets...)
		rec.PushVideoSample(sample)
		// The same parameter sets alone, once the file is open.
		key.push(true, [][]byte{low, lowPPS})
		key.push(true, testAU(t, 1))
		rec.Close()

		blocks := readVideoBlocks(t, rec.GetFilePath())
		require.Len(t, blocks, 2)
		assert.True(t, blocks[0].key)
		assert.False(t, blocks[1].key)

		doc := readContainer(t, rec.GetFilePath())
		assert.Equal(t, uint64(320), doc.Segment.Tracks.TrackEntry[0].Video.PixelWidth, "the key frame's own SPS")
	})
}

func TestWebmRecorderH264KeepsTheFirstConfigAcrossASizeChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, _ := newH264Recorder(t, false)
		s := &h264Stream{rec: rec, seq: 1, ts: 0}

		s.push(false, testAU(t, 0))
		// A layer switch: an IDR with another SPS, in band.
		big := testAU(t, 0)
		big[1], big[2] = mustHex(t, h264TestSPS[0].nal), mustHex(t, h264TestSPS[0].pps)
		s.push(true, big)
		rec.Close()

		blocks := readVideoBlocks(t, rec.GetFilePath())
		require.Len(t, blocks, 2)
		assert.True(t, blocks[1].key)
		assert.Contains(t, blocks[1].data, string(big[1]), "the new SPS stays in the block")

		entry := readContainer(t, rec.GetFilePath()).Segment.Tracks.TrackEntry[0]
		assert.Equal(t, uint64(320), entry.Video.PixelWidth)
		assert.Equal(t, uint64(180), entry.Video.PixelHeight)
	})
}

func TestWebmRecorderH264CloseLogNamesTheFormat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, _ := newH264Recorder(t, false)
		(&h264Stream{rec: rec, seq: 1}).push(false, testAU(t, 0))

		std := log.StandardLogger()
		defer std.ReplaceHooks(std.ReplaceHooks(make(log.LevelHooks)))
		hook := logtest.NewLocal(std)
		rec.Close()

		require.NotNil(t, hook.LastEntry())
		assert.Equal(t, "mkv writer closed: "+rec.GetFilePath(), hook.LastEntry().Message)
	})
}

func TestWebmRecorderH264HasNoIVFCopy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, _ := newH264Recorder(t, true)
		s := &h264Stream{rec: rec, seq: 1, ts: 0}
		s.push(false, testAU(t, 0))
		rec.Close()

		assert.FileExists(t, rec.GetFilePath())
		assert.NoFileExists(t, strings.TrimSuffix(rec.GetFilePath(), ".mkv")+".ivf")
	})
}

func TestCodecH264(t *testing.T) {
	c := CodecH264
	assert.Equal(t, "V_MPEG4/ISO/AVC", c.MatroskaID)
	assert.False(t, c.WebM)
	assert.Equal(t, uint32(90000), c.ClockRate)
	assert.True(t, c.TakesSamples())
	assert.False(t, CodecVP8.TakesSamples())

	webm := NewWebmRecorder(filepath.Join(t.TempDir(), "x.webm"), 0o600, 256, 32, true, false)
	assert.Error(t, webm.SetVideoCodec(CodecH264))

	// The packet key-frame hint the track buffer uses.
	key := &rtp.Packet{Payload: stapA(testAU(t, 0)[1], testAU(t, 0)[2])}
	assert.True(t, c.KeyFrameStart(key))
	assert.False(t, c.KeyFrameStart(&rtp.Packet{Payload: testAU(t, 1)[1]}))
}

func TestWebmRecorderH264DropsPacketsPushedOneAtATime(t *testing.T) {
	rec, _ := newH264Recorder(t, false)
	for _, p := range (&h264Stream{rec: rec}).sample(false, testAU(t, 0)).Packets {
		rec.PushVideoAt(p.Packet, p.At)
	}
	rec.Close()

	_, err := os.Stat(rec.GetFilePath())
	assert.True(t, os.IsNotExist(err))
}

// The layer switch h264AccessUnits describes, as the writer gets it.
func TestWebmRecorderH264ResumesAtAKeyFrameAfterATruncatedFrame(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, _ := newH264Recorder(t, false)
		s := &h264Stream{rec: rec, seq: 1, ts: 0}

		s.push(false, testAU(t, 0))
		s.push(true, testAU(t, 1))

		// The first fragment of a P slice, its frame cut there.
		cut := fuA(testAU(t, 2)[2], 100)[0]
		sample := VideoSample{Follows: true, Packets: []ArrivedPacket{{
			Packet: &rtp.Packet{Header: rtp.Header{SequenceNumber: s.seq, Timestamp: s.ts, SSRC: 1234}, Payload: cut},
			At:     time.Now(),
		}}}
		s.seq++
		s.ts++
		sample.Packets = append(sample.Packets, s.sample(true, testAU(t, 0)).Packets...)
		assert.Empty(t, warnings(func() { rec.PushVideoSample(sample) }), "a layer switch is no malformed input")
		s.push(true, testAU(t, 1))
		rec.Close()

		blocks := readVideoBlocks(t, rec.GetFilePath())
		require.Len(t, blocks, 4, "the new layer's key frame and the frame after it")
		assert.True(t, blocks[2].key)
		assert.False(t, blocks[3].key)
	})
}

func TestWebmRecorderH264ResumesAtAKeyFrameAfterAMalformedDeltaFrame(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, req := newH264Recorder(t, false)
		s := &h264Stream{rec: rec, seq: 1, ts: 0}

		s.push(false, testAU(t, 0))
		s.push(true, testAU(t, 1))
		// A delta frame of a reserved NAL unit type, with no packet lost.
		logged := warnings(func() { s.push(true, [][]byte{{0x1e, 1, 2, 3}}) })
		s.push(true, testAU(t, 2))
		rec.Close()

		assert.Equal(t, []writtenBlock{
			{true, 0, avccOf(testAU(t, 0))},
			{false, 66, avccOf(testAU(t, 1))},
		}, readVideoBlocks(t, rec.GetFilePath()), "the next delta frame may reference the malformed one")
		assert.Equal(t, 1, req.count())
		require.Len(t, logged, 1)
		assert.Contains(t, logged[0], "Discarding malformed video/h264 frame")
	})
}

func TestWebmRecorderH264DropsAFrameCutBetweenSlices(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, req := newH264Recorder(t, false)
		s := &h264Stream{rec: rec, seq: 1, ts: 0}

		s.push(false, testAU(t, 0))
		s.push(true, testAU(t, 1))

		// LiveKit switches layers after the AUD and first slice of a P
		// frame. The new layer's key frame follows one tick later.
		sample := s.sample(true, testAU(t, 2)[:2])
		sample.Packets[len(sample.Packets)-1].Marker = false
		s.ts -= 6000 - 1
		sample.Packets = append(sample.Packets, s.sample(true, testAU(t, 0)).Packets...)
		assert.Empty(t, warnings(func() { rec.PushVideoSample(sample) }))
		s.push(true, testAU(t, 1))
		rec.Close()

		assert.Equal(t, []writtenBlock{
			{true, 0, avccOf(testAU(t, 0))},
			{false, 66, avccOf(testAU(t, 1))},
			{true, 133, avccOf(testAU(t, 0))},
			{false, 200, avccOf(testAU(t, 1))},
		}, readVideoBlocks(t, rec.GetFilePath()))
		assert.Zero(t, req.count(), "the key frame came with the cut frame")
	})
}

// warnings returns the messages logged at Warn level or above while f runs.
func warnings(f func()) []string {
	std := log.StandardLogger()
	defer std.ReplaceHooks(std.ReplaceHooks(make(log.LevelHooks)))
	hook := logtest.NewLocal(std)

	f()

	var messages []string
	for _, e := range hook.AllEntries() {
		if e.Level <= log.WarnLevel {
			messages = append(messages, e.Message)
		}
	}

	return messages
}
