package livekit

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const corpusDir = "testdata/corpus"

// bound is an inclusive count range; a missing max leaves it open.
type bound struct {
	Min int  `json:"min"`
	Max *int `json:"max,omitempty"`
}

func (b *bound) violation(name string, got int) string {
	if b == nil || (got >= b.Min && (b.Max == nil || got <= *b.Max)) {
		return ""
	}

	upper := "inf"
	if b.Max != nil {
		upper = fmt.Sprint(*b.Max)
	}

	return fmt.Sprintf("%s = %d, want [%d, %s]", name, got, b.Min, upper)
}

type webmExpect struct {
	NoDupPTS       bool `json:"noDupPTS"`
	Monotonic      bool `json:"monotonic"`
	MinVideoFrames int  `json:"minVideoFrames,omitempty"`
	MinAudioFrames int  `json:"minAudioFrames,omitempty"`
	MinKeyframes   int  `json:"minKeyframes,omitempty"` // by the VP8 payload bit
}

// window is an inclusive time range, in ms on the trace's atUs timeline.
type window struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// checks are the bounds of an expect v1 sidecar; see testdata/corpus/README.md.
type checks struct {
	BufferRequests    *bound      `json:"bufferRequests,omitempty"`
	BufferRequestsMs  []window    `json:"bufferRequestsMs,omitempty"`
	WriterRequests    *bound      `json:"writerRequests,omitempty"`
	FirstRequestMinMs *float64    `json:"firstRequestMinMs,omitempty"`
	FirstRequestMaxMs *float64    `json:"firstRequestMaxMs,omitempty"`
	Written           *bound      `json:"written,omitempty"`
	PacketsWritten    *bound      `json:"packetsWritten,omitempty"`
	PacketsLostMax    *uint64     `json:"packetsLostMax,omitempty"`
	Webm              *webmExpect `json:"webm,omitempty"`
}

type expectation struct {
	V      int      `json:"v"`
	Class  string   `json:"class"`
	Intent string   `json:"intent"`
	Traces []string `json:"traces"`
	checks
	// Residuals explain the checks pinned away from their ideal value.
	Residuals []string `json:"residuals,omitempty"`
}

func msOf(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// violations lists every check of e that r fails.
func (e *checks) violations(r replayResult) []string {
	var v []string
	add := func(s string) {
		if s != "" {
			v = append(v, s)
		}
	}

	add(e.BufferRequests.violation("bufferRequests", len(r.bufferRequests)))
	add(e.WriterRequests.violation("writerRequests", len(r.writerRequests)))
	add(e.Written.violation("written", r.videoFrames+r.audioFrames))
	add(e.PacketsWritten.violation("packetsWritten", r.packets))

	if e.FirstRequestMinMs != nil || e.FirstRequestMaxMs != nil {
		if reqs := r.requests(); len(reqs) == 0 {
			add("no request, want a first one")
		} else {
			first := msOf(slices.Min(reqs))
			if (e.FirstRequestMinMs != nil && first < *e.FirstRequestMinMs) ||
				(e.FirstRequestMaxMs != nil && first > *e.FirstRequestMaxMs) {
				add(fmt.Sprintf("first request at %.3f ms, want [%v, %v]", first, deref(e.FirstRequestMinMs), deref(e.FirstRequestMaxMs)))
			}
		}
	}

	if len(e.BufferRequestsMs) > 0 {
		if len(r.bufferRequests) != len(e.BufferRequestsMs) {
			add(fmt.Sprintf("%d buffer requests, want one in each of %d windows", len(r.bufferRequests), len(e.BufferRequestsMs)))
		} else {
			for i, w := range e.BufferRequestsMs {
				if at := msOf(r.bufferRequests[i]); at < w.Min || at > w.Max {
					add(fmt.Sprintf("buffer request %d at %.3f ms, want [%v, %v]", i+1, at, w.Min, w.Max))
				}
			}
		}
	}

	if e.PacketsLostMax != nil && r.packetsLost > *e.PacketsLostMax {
		add(fmt.Sprintf("packetsLost = %d, want <= %d", r.packetsLost, *e.PacketsLostMax))
	}

	if e.Webm != nil {
		v = append(v, e.Webm.violations(r)...)
	}

	return v
}

func deref(f *float64) any {
	if f == nil {
		return "-"
	}

	return *f
}

func (e *webmExpect) violations(r replayResult) []string {
	// Nothing written leaves no file, which fails every WebM check.
	if r.webmErr != nil {
		return []string{fmt.Sprintf("webm: %v", r.webmErr)}
	}

	var v []string
	codecs := make([]string, 0, len(r.webm.Tracks))
	for c := range r.webm.Tracks {
		codecs = append(codecs, c)
	}
	slices.Sort(codecs)

	for _, c := range codecs {
		tr := r.webm.Tracks[c]
		if err := tr.CheckNoDuplicatePTS(); e.NoDupPTS && err != nil {
			v = append(v, fmt.Sprintf("webm %s: %v", c, err))
		}
		if err := tr.CheckMonotonic(); e.Monotonic && err != nil {
			v = append(v, fmt.Sprintf("webm %s: %v", c, err))
		}
	}

	blocks := func(codec string) (n, keys int) {
		if tr := r.webm.Track(codec); tr != nil {
			return tr.Blocks, tr.VP8Keyframes
		}

		return 0, 0
	}
	video, keys := blocks(oracle.CodecVP8)
	audio, _ := blocks(oracle.CodecOpus)

	if video < e.MinVideoFrames {
		v = append(v, fmt.Sprintf("webm: %d video frames, want >= %d", video, e.MinVideoFrames))
	}
	if keys < e.MinKeyframes {
		v = append(v, fmt.Sprintf("webm: %d key frames, want >= %d", keys, e.MinKeyframes))
	}
	if audio < e.MinAudioFrames {
		v = append(v, fmt.Sprintf("webm: %d audio frames, want >= %d", audio, e.MinAudioFrames))
	}

	return v
}

func loadExpectation(t *testing.T, path string) *expectation {
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	e := &expectation{}
	require.NoError(t, dec.Decode(e), path)
	require.Equal(t, 1, e.V, path)
	require.NotEmpty(t, e.Traces, path)

	return e
}

// replayTraces plays traces together through the read loop into one WebM.
// Run it inside a synctest bubble: the traces' atUs are offsets from now.
func replayTraces(t *testing.T, traces []*corpus.Trace) replayResult {
	infos := make([]trackInfo, 0, len(traces))
	for i, tr := range traces {
		info := trackInfo{id: tr.Header.TrackID, ssrc: tr.Header.SSRC}
		if info.id == "" {
			info.id = fmt.Sprintf("track-%d", i)
		}

		switch tr.Header.Codec {
		case corpus.CodecVP8:
			info.kind, info.mime, info.source, info.pubMime = TrackKindVideo, MimeTypeVP8, "camera", "video/VP8"
		case corpus.CodecOpus:
			info.kind, info.mime, info.source, info.pubMime = TrackKindAudio, MimeTypeOpus, "microphone", "audio/opus"
		default:
			require.Failf(t, "unsupported codec", "%q", tr.Header.Codec)
		}
		infos = append(infos, info)
	}

	h := newReplayHarness(t, infos...)
	turn := make(chan struct{}, 1)
	srcs := make([]trackSource, 0, len(traces))
	fakes := make([]*fakeTrack, 0, len(traces))
	for _, tr := range traces {
		reads := make([]trackRead, 0, len(tr.Records))
		for _, rec := range tr.Records {
			read := trackRead{at: time.Duration(rec.AtUs) * time.Microsecond}
			if rec.T == corpus.RecordEOF {
				read.err = io.EOF
			} else {
				read.pkt = rec.Packet()
				read.pkt.SSRC = tr.Header.SSRC
			}
			reads = append(reads, read)
		}
		fake := newFakeTrack(reads)
		fakes = append(fakes, fake)
		srcs = append(srcs, settledSource{trackSource: fake, turn: turn})
	}

	r := h.run(nil, srcs...)
	for i, fake := range fakes {
		assert.False(t, fake.staleDeadline, "%s: a read without a deadline ahead", infos[i].id)
	}

	return r
}

func (r replayResult) String() string {
	reqs := func(ds []time.Duration) string {
		s := make([]string, 0, len(ds))
		for _, d := range ds {
			s = append(s, fmt.Sprintf("%.3f", msOf(d)))
		}

		return "[" + strings.Join(s, " ") + "]"
	}

	webm := "none"
	if r.webm != nil {
		var parts []string
		for _, c := range []string{oracle.CodecVP8, oracle.CodecOpus} {
			if tr := r.webm.Track(c); tr != nil {
				parts = append(parts, fmt.Sprintf("%s{blocks=%d vp8key=%d keyBlocks=%d dup=%v mono=%v maxGap=%d}",
					c, tr.Blocks, tr.VP8Keyframes, tr.KeyBlocks, tr.DuplicatePTS, tr.Monotonic, tr.MaxGapMs))
			}
		}
		webm = strings.Join(parts, " ")
	}

	return fmt.Sprintf("buffer=%d%s writer=%d%s frames v=%d a=%d packets=%d lost=%d dropped=%d webm=%s",
		len(r.bufferRequests), reqs(r.bufferRequests), len(r.writerRequests), reqs(r.writerRequests),
		r.videoFrames, r.audioFrames, r.packets, r.packetsLost, r.packetsDropped, webm)
}

// TestCorpusReplay plays each seed of testdata/corpus through the read loop,
// the track buffer and the WebM writer, and checks the run against the
// seed's expect file.
func TestCorpusReplay(t *testing.T) {
	expects, err := filepath.Glob(filepath.Join(corpusDir, "*.expect.json"))
	require.NoError(t, err)
	require.NotEmpty(t, expects)

	traceFiles, err := filepath.Glob(filepath.Join(corpusDir, "*.ndjson.gz"))
	require.NoError(t, err)
	replayed := map[string]int{}

	for _, path := range expects {
		name := strings.TrimSuffix(filepath.Base(path), ".expect.json")
		e := loadExpectation(t, path)

		traces := make([]*corpus.Trace, 0, len(e.Traces))
		for _, f := range e.Traces {
			tr, err := corpus.ReadFile(filepath.Join(corpusDir, f))
			require.NoError(t, err)
			traces = append(traces, tr)
			replayed[filepath.Join(corpusDir, f)]++
		}

		t.Run(name, func(t *testing.T) {
			var r replayResult
			synctest.Test(t, func(t *testing.T) {
				r = replayTraces(t, traces)
			})
			t.Logf("measured: %s", r)

			for _, v := range e.violations(r) {
				assert.Fail(t, v)
			}

			// The writer's count and the file must agree, or a frame the
			// writer counts can go missing from the recording unseen.
			if r.webm != nil {
				for codec, n := range map[string]int{oracle.CodecVP8: r.videoFrames, oracle.CodecOpus: r.audioFrames} {
					blocks := 0
					if tr := r.webm.Track(codec); tr != nil {
						blocks = tr.Blocks
					}
					assert.Equal(t, n, blocks, "%s: frames written against blocks in the file", codec)
				}
			}
		})
	}

	for _, f := range traceFiles {
		assert.Equal(t, 1, replayed[f], "%s: replayed by exactly one expect file", f)
	}
}

var replayTracesFlag = flag.String("corpus.replay", "",
	"comma-separated trace files that TestReplayTraces replays together, such as a live capture")

// TestReplayTraces replays the traces named by -corpus.replay with no expect
// file and logs what it measured, to look at a capture before it is a seed.
func TestReplayTraces(t *testing.T) {
	if *replayTracesFlag == "" {
		t.Skip("set -corpus.replay=<trace>[,<trace>...]")
	}

	var traces []*corpus.Trace
	for _, path := range strings.Split(*replayTracesFlag, ",") {
		tr, err := corpus.ReadFile(path)
		require.NoError(t, err)
		traces = append(traces, tr)
	}

	var r replayResult
	synctest.Test(t, func(t *testing.T) {
		r = replayTraces(t, traces)
	})
	t.Logf("measured: %s", r)
}

// TestKeyframeLoopDoesNotPersist closes the loop the corpus cannot: the
// publisher answers each request with a key frame its pacer spreads over
// hundreds of ms. A key frame that expires before its last packet is a loss
// that requests the next one, so with too short a video latency one
// unrepaired loss never recovers.
func TestKeyframeLoopDoesNotPersist(t *testing.T) {
	// lostFrame is about 3 s in. At videoJitterLatency, the one request it
	// causes brings a key frame that completes, so only the frames up to it
	// are lost.
	tests := []struct {
		name         string
		fps          float64
		deltaPackets int
		keyPackets   int
		keySpread    time.Duration
		lostFrame    int
		minFrames    int // of int(20*fps)
	}{
		{name: "3.4fps", fps: 3.4, deltaPackets: 6, keyPackets: 16, keySpread: 664 * time.Millisecond, lostFrame: 10, minFrames: 65},
		{name: "15fps", fps: 15, deltaPackets: 3, keyPackets: 16, keySpread: 480 * time.Millisecond, lostFrame: 45, minFrames: 295},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				info := trackInfo{id: "camera", kind: TrackKindVideo, mime: MimeTypeVP8, source: "camera", ssrc: 4321, pubMime: "video/VP8"}
				h := newReplayHarness(t, info)
				cam := &pacedCamera{
					ssrc: info.ssrc, fps: tt.fps, frames: int(20 * tt.fps),
					deltaPackets: tt.deltaPackets, keyPackets: tt.keyPackets, keySpread: tt.keySpread,
					respond: 120 * time.Millisecond, lostFrame: tt.lostFrame, start: h.start,
				}

				r := h.run(cam.request, cam)
				t.Logf("measured: %s keyFramesSent=%d", r, cam.keys)
				assert.False(t, cam.staleDeadline, "a read without a deadline ahead")

				lossAt := time.Duration(float64(tt.lostFrame) * float64(time.Second) / tt.fps)
				reqs := r.requests()
				// A loop asks again once lossKeyframeInterval has passed.
				require.Len(t, reqs, 1, "requests")
				assert.GreaterOrEqual(t, reqs[0], lossAt, "the request")
				assert.Less(t, reqs[0], lossAt+lossKeyframeInterval, "the request")
				assert.Equal(t, 1, cam.keys, "key frames sent after the first")
				assert.GreaterOrEqual(t, r.videoFrames, tt.minFrames, "frames written")
			})
		})
	}
}
