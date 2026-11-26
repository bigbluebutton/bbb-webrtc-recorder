package recorder

import (
	"encoding/json"
	"flag"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var updateFence = flag.Bool("update", false, "pin in testdata/fence.json the values this run measures")

// fenceCase is one input the recorder fence pins the output of.
type fenceCase struct {
	name   string
	custom bool // recorder.useCustomSampler
	ivf    bool // recorder.writeIVFCopy
	video  []corpus.Record
	audio  []corpus.Record
	// Video packets not pushed; NotifySkippedPacket reports each instead.
	skipped []uint16
}

// fenceEntry is what the fence pins for a case.
type fenceEntry struct {
	File       string  `json:"file"`
	IVF        string  `json:"ivf,omitempty"`
	Stats      string  `json:"stats"`
	RequestsUs []int64 `json:"requestsUs"`
}

// fenceRequester records the time of the writer's keyframe requests.
type fenceRequester struct {
	start time.Time

	mu  sync.Mutex
	got []int64
}

func (r *fenceRequester) RequestKeyframe() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, time.Since(r.start).Microseconds())
}

func (r *fenceRequester) RequestKeyframeForSSRC(uint32) { r.RequestKeyframe() }

func fenceVP8(t *testing.T, o corpus.VP8Options, ops ...corpus.Op) []corpus.Record {
	tr, err := corpus.GenerateVP8(o)
	require.NoError(t, err)
	recs, err := corpus.Apply(tr.Records, ops...)
	require.NoError(t, err)

	return recs
}

func fenceOpus(t *testing.T, o corpus.OpusOptions, ops ...corpus.Op) []corpus.Record {
	tr, err := corpus.GenerateOpus(o)
	require.NoError(t, err)
	recs, err := corpus.Apply(tr.Records, ops...)
	require.NoError(t, err)

	return recs
}

// fenceCases covers both VP8 assemblers, Opus and two-track files. The
// streams wrap their sequence numbers and timestamps.
func fenceCases(t *testing.T) []fenceCase {
	camera := corpus.VP8Options{
		FPS: 30, Frames: 90, KeyInterval: 30, KeyPackets: 4, DeltaPackets: 2,
		StartSeq: 65500, StartTS: 0xFFFF0000, StartPictureID: 0x7FF0, SpreadUs: 6000,
	}
	// The first packet of frame f, in the camera's layout.
	seqOf := func(f int) uint16 {
		keys := f/30 + 1
		if f%30 == 0 {
			keys--
		}

		return camera.StartSeq + uint16(keys*4+(f-keys)*2)
	}

	clean := fenceVP8(t, camera)
	loss := fenceVP8(t, camera, corpus.DropSeqs(seqOf(9)+1, seqOf(19), seqOf(19)+1, seqOf(30)+2))
	reorder := fenceVP8(t, camera, corpus.Reorder(3, 0.08, rand.New(rand.NewPCG(1, 2))))
	pause := fenceVP8(t, camera, corpus.Pause(1_000_000, 1_800_000))

	resize := fenceVP8(t, camera)
	for i, r := range resize {
		if r.Seq == seqOf(60) && i > 0 {
			pic, _ := corpus.VP8PictureID(r.Pay)
			resize[i].Pay = corpus.VP8Payload(pic, true, true, 320, 240)
		}
	}

	mic := corpus.OpusOptions{Frames: 150, StartSeq: 65450, StartTS: 0xFFFFF000}
	audio := fenceOpus(t, mic,
		corpus.DropSeqs(65460, 65461, 65535, 3),
		corpus.Duplicate(65470, 400_000),
		corpus.Reorder(2, 0.05, rand.New(rand.NewPCG(3, 4))))
	// A camera that starts 100 ms after the microphone. When video starts
	// first, the writer is opened again on each video frame until audio is
	// written. The earlier writers' goroutines never end, which synctest
	// reports.
	lateCamera := func(recs []corpus.Record) []corpus.Record {
		recs = slices.Clone(recs)
		for i := range recs {
			recs[i].AtUs += 100_000
		}

		return recs
	}
	mic0 := fenceOpus(t, mic)

	return []fenceCase{
		{name: "vp8-custom", custom: true, video: clean},
		{name: "vp8-builtin", video: clean},
		{name: "vp8-custom-loss", custom: true, video: loss},
		{name: "vp8-builtin-loss", video: loss},
		{name: "vp8-custom-reorder", custom: true, video: reorder},
		{name: "vp8-builtin-reorder", video: reorder},
		{name: "vp8-custom-skip", custom: true, video: clean, skipped: []uint16{seqOf(40) + 1}},
		{name: "vp8-custom-pause", custom: true, video: pause},
		{name: "vp8-custom-resize", custom: true, video: resize},
		{name: "vp8-builtin-resize", video: resize},
		{name: "vp8-custom-ivf", custom: true, ivf: true, video: clean},
		{name: "vp8-builtin-ivf", ivf: true, video: clean},
		{name: "opus", custom: true, audio: audio},
		{name: "av-custom", custom: true, video: lateCamera(clean), audio: mic0},
		// No builtin two-track case: that path never marks its video valid, so
		// it opens a new writer on every key frame.
		{name: "av-custom-loss", custom: true, video: lateCamera(loss), audio: audio},
	}
}

type fencePush struct {
	at    int64
	video bool
	rec   corpus.Record
}

// runFenceCase pushes c's packets at their read times, closes the recorder
// and returns what the fence pins. Run it inside a synctest bubble.
func runFenceCase(t *testing.T, c fenceCase) fenceEntry {
	dir := t.TempDir()
	file := filepath.Join(dir, "fence.webm")
	rec := NewWebmRecorder(file, 0o600, 256, 32, c.custom, c.ivf)
	rec.WithContext(testSessionContext())
	rec.SetHasVideo(len(c.video) > 0)
	rec.SetHasAudio(len(c.audio) > 0)

	req := &fenceRequester{start: time.Now()}
	rec.SetKeyframeRequester(req)

	var pushes []fencePush
	for _, r := range c.video {
		pushes = append(pushes, fencePush{at: r.AtUs, video: true, rec: r})
	}
	for _, r := range c.audio {
		pushes = append(pushes, fencePush{at: r.AtUs, rec: r})
	}
	slices.SortStableFunc(pushes, func(a, b fencePush) int {
		switch {
		case a.at < b.at:
			return -1
		case a.at > b.at:
			return 1
		}

		return 0
	})

	for _, p := range pushes {
		if d := time.Until(req.start.Add(time.Duration(p.at) * time.Microsecond)); d > 0 {
			time.Sleep(d)
		}

		pkt := p.rec.Packet()
		if pkt == nil {
			continue
		}

		switch {
		case p.video && slices.Contains(c.skipped, pkt.SequenceNumber):
			rec.NotifySkippedPacket(pkt.SequenceNumber)
		case p.video:
			rec.PushVideo(pkt)
		default:
			rec.PushAudio(pkt)
		}
	}

	rec.Close()

	stats, err := json.Marshal(rec.GetStats())
	require.NoError(t, err)
	data, err := os.ReadFile(file)
	require.NoError(t, err)

	e := fenceEntry{File: oracle.SHA256(data), Stats: oracle.SHA256(stats)}
	if c.ivf {
		ivf, err := os.ReadFile(strings.TrimSuffix(file, ".webm") + ".ivf")
		require.NoError(t, err)
		e.IVF = oracle.SHA256(ivf)
	}

	req.mu.Lock()
	e.RequestsUs = append([]int64{}, req.got...)
	req.mu.Unlock()

	t.Logf("stats: %s", stats)

	return e
}

// TestWebmRecorderFence pins the bytes, stats and keyframe requests of the
// recorder's VP8 and Opus output. A refactor of the writer cannot change
// them unseen.
func TestWebmRecorderFence(t *testing.T) {
	fence, err := oracle.NewFence("testdata/fence.json", *updateFence)
	require.NoError(t, err)

	cases := fenceCases(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var e fenceEntry
			synctest.Test(t, func(t *testing.T) {
				e = runFenceCase(t, c)
			})

			diff, err := fence.Check(c.name, e)
			require.NoError(t, err)
			assert.Empty(t, diff, "the recording, its stats or its keyframe requests changed; if intended, run with -update")
		})
	}

	unmeasured, err := fence.Close()
	require.NoError(t, err)
	if flag.Lookup("test.run").Value.String() == "" && flag.Lookup("test.skip").Value.String() == "" {
		assert.Empty(t, unmeasured, "cases pinned in testdata/fence.json that no run measured; -update keeps the pin of a removed case, so delete it by hand")
	}
}
