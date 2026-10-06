package livekit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/utils"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVP8HeaderLen(t *testing.T) {
	media := []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66}
	key := []byte{0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x80, 0x02, 0xe0, 0x01}
	inter := []byte{0x11, 0x02, 0x00}
	cat := func(parts ...[]byte) []byte { return slices.Concat(parts...) }

	tests := []struct {
		name string
		pay  []byte
		want int
	}{
		{name: "empty", pay: nil, want: 0},
		{name: "no X, continuation", pay: cat([]byte{0x00}, media), want: 1},
		{name: "no X, interframe start", pay: cat([]byte{0x10}, inter, media), want: 4},
		{name: "no X, key frame start", pay: cat([]byte{0x10}, key, media), want: 11},
		{name: "7-bit picture ID, continuation", pay: cat([]byte{0x80, 0x80, 0x05}, media), want: 3},
		{name: "15-bit picture ID, key frame start", pay: cat([]byte{0x90, 0x80, 0x81, 0x23}, key, media), want: 14},
		{name: "15-bit picture ID, interframe start", pay: cat([]byte{0x90, 0x80, 0x81, 0x23}, inter, media), want: 7},
		{name: "I L T K, continuation", pay: cat([]byte{0x80, 0xf0, 0x81, 0x23, 0x07, 0x40}, media), want: 6},
		{name: "L only", pay: cat([]byte{0x80, 0x40, 0x07}, media), want: 3},
		{name: "T only", pay: cat([]byte{0x80, 0x20, 0x40}, media), want: 3},
		{name: "K only", pay: cat([]byte{0x80, 0x10, 0x03}, media), want: 3},
		{name: "T and K share a byte", pay: cat([]byte{0x80, 0x30, 0x43}, media), want: 3},
		{name: "N set, key frame start", pay: cat([]byte{0x30}, key, media), want: 11},
		{name: "start of partition 1", pay: cat([]byte{0x91, 0x80, 0x05}, key, media), want: 3},
		{name: "truncated after X", pay: []byte{0x80}, want: 1},
		{name: "truncated before the picture ID", pay: []byte{0x80, 0x80}, want: 2},
		{name: "truncated 15-bit picture ID", pay: []byte{0x80, 0x80, 0x81}, want: 3},
		{name: "descriptor only, start", pay: []byte{0x90, 0x80, 0x05}, want: 3},
		{name: "truncated key frame header", pay: []byte{0x10, 0x00, 0x00, 0x9d}, want: 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, vp8HeaderLen(tt.pay))
		})
	}
}

// The header prefix of a generated packet is exactly the prefix the
// generator wrote, so a capture of a replay can be compared to its input.
func TestVP8HeaderLenMatchesGenerator(t *testing.T) {
	tr, err := corpus.GenerateVP8(corpus.VP8Options{FPS: 30, Frames: 40, KeyInterval: 10, StartPictureID: 0x7ff0})
	require.NoError(t, err)

	for i, r := range tr.Records {
		assert.Equal(t, len(r.Pay), vp8HeaderLen(r.Packet().Payload), "record %d", i)
	}
}

// rtxSource hands out the packets in rtx with the attribute pion sets on
// a packet read from the RTX stream.
type rtxSource struct {
	trackSource
	rtx map[*rtp.Packet]bool
}

func (s rtxSource) ReadRTP() (*rtp.Packet, interceptor.Attributes, error) {
	p, attrs, err := s.trackSource.ReadRTP()
	if p != nil && s.rtx[p] {
		attrs = interceptor.Attributes{}
		attrs.Set(webrtc.AttributeRtxSsrc, uint32(4242))
	}

	return p, attrs, err
}

// captureRun is a replay with the trace capture configured as cfg.
type captureRun struct {
	r        replayResult
	dir      string
	files    []string // in dir once the replay ends
	paths    []string // each trace's capture file
	captured []*corpus.Trace
	sizes    []int64
	timeouts []int // each source's timed-out reads
	epoch    time.Time
}

func (c captureRun) read(t *testing.T, i int) *corpus.Trace {
	require.NotNil(t, c.captured[i], "%s: no capture", c.paths[i])

	return c.captured[i]
}

// collect reads what the replay left in its directory, which the bubble's
// t removes when it ends.
func (c *captureRun) collect(t *testing.T) {
	entries, err := os.ReadDir(c.dir)
	require.NoError(t, err)
	for _, e := range entries {
		c.files = append(c.files, e.Name())
	}

	for _, p := range c.paths {
		st, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			c.captured, c.sizes = append(c.captured, nil), append(c.sizes, 0)

			continue
		}
		require.NoError(t, err)

		tr, err := corpus.ReadFile(p)
		require.NoError(t, err)
		require.NoError(t, tr.Validate(), p)
		c.captured, c.sizes = append(c.captured, tr), append(c.sizes, st.Size())
	}
}

// replayCaptured is replayTraces with each track set up for readTrack as
// onTrackSubscribed does, the trace capture configured as cfg, rtx records
// read with pion's RTX attribute, and every capture closed as Close does
// once the tracks end. Run it inside a synctest bubble.
func replayCaptured(t *testing.T, cfg config.TraceCapture, traces ...*corpus.Trace) captureRun {
	infos := make([]trackInfo, 0, len(traces))
	clockRates := make([]uint32, 0, len(traces))
	for i, tr := range traces {
		info, clockRate := traceInfo(t, i, tr)
		infos = append(infos, info)
		clockRates = append(clockRates, clockRate)
	}

	h := newReplayHarness(t, infos...)
	h.w.cfg.TraceCapture = cfg
	run := captureRun{dir: filepath.Dir(h.file), epoch: h.w.startTs}
	for i, info := range infos {
		h.w.prepareTrack(info, clockRates[i])
		run.paths = append(run.paths, traceCapturePath(h.file, info.id))
	}

	turn := make(chan struct{}, 1)
	srcs := make([]trackSource, 0, len(traces))
	fakes := make([]*fakeTrack, 0, len(traces))
	for _, tr := range traces {
		reads, rtx := traceReads(tr)
		fake := newFakeTrack(reads)
		fakes = append(fakes, fake)
		srcs = append(srcs, settledSource{trackSource: rtxSource{trackSource: fake, rtx: rtx}, turn: turn})
	}

	run.r = h.run(nil, srcs...)
	h.w.closeTraceCaptures()

	for i, fake := range fakes {
		assert.False(t, fake.staleDeadline, "%s: a read without a deadline ahead", infos[i].id)
		run.timeouts = append(run.timeouts, fake.timeouts)
	}
	run.collect(t)

	return run
}

func loadSeed(t *testing.T, names ...string) []*corpus.Trace {
	traces := make([]*corpus.Trace, 0, len(names))
	for _, name := range names {
		tr, err := corpus.ReadFile(filepath.Join(corpusDir, name+".ndjson.gz"))
		require.NoError(t, err)
		traces = append(traces, tr)
	}

	return traces
}

// readView is what a capture keeps of an rtp record.
type readView struct {
	AtUs int64
	Seq  uint16
	TS   uint32
	M    bool
	PT   uint8
	PLen int
	RTX  bool
	Pad  bool
	Pay  []byte
}

func readViews(tr *corpus.Trace, pay func(corpus.Record) []byte) []readView {
	var views []readView
	for _, r := range tr.Records {
		if r.T != corpus.RecordRTP {
			continue
		}

		p := pay(r)
		if len(p) == 0 {
			p = nil
		}
		views = append(views, readView{AtUs: r.AtUs, Seq: r.Seq, TS: r.TS, M: r.M, PT: r.PT, PLen: r.PLen, RTX: r.RTX, Pad: r.Pad, Pay: p})
	}

	return views
}

func asCaptured(r corpus.Record) []byte  { return r.Pay }
func fullPayload(r corpus.Record) []byte { return r.Packet().Payload }

func recordsOf(tr *corpus.Trace, typ corpus.RecordType) []corpus.Record {
	var recs []corpus.Record
	for _, r := range tr.Records {
		if r.T == typ {
			recs = append(recs, r)
		}
	}

	return recs
}

func plisAt(tr *corpus.Trace, reason string) []int64 {
	var at []int64
	for _, r := range recordsOf(tr, corpus.RecordPLI) {
		if r.Reason == reason {
			at = append(at, r.AtUs)
		}
	}

	return at
}

func microsOf(ds []time.Duration) []int64 {
	var us []int64
	for _, d := range ds {
		us = append(us, d.Microseconds())
	}

	return us
}

var captureSeeds = []struct {
	name   string
	traces []string
}{
	{name: "requests of both origins", traces: []string{"c10-two-losses-1500ms"}},
	{name: "rtx copies", traces: []string{"c12-lkprobe-rtxdup"}},
	{name: "two tracks", traces: []string{"baseline-av.video", "baseline-av.audio"}},
}

func headerCapture() config.TraceCapture {
	return config.TraceCapture{Enabled: true, Payload: config.TraceCapturePayloadHeader, MaxBytesPerTrack: 16 << 20}
}

func TestTraceCaptureDisabled(t *testing.T) {
	for _, seed := range captureSeeds {
		t.Run(seed.name, func(t *testing.T) {
			traces := loadSeed(t, seed.traces...)
			var off, on captureRun
			synctest.Test(t, func(t *testing.T) {
				off = replayCaptured(t, (&config.Config{}).GetDefaults().LiveKit.TraceCapture, traces...)
			})
			synctest.Test(t, func(t *testing.T) {
				on = replayCaptured(t, headerCapture(), traces...)
			})

			assert.Equal(t, []string{"replay.webm"}, off.files, "no trace file")
			assert.Len(t, on.files, 1+len(traces))
			for i := range traces {
				on.read(t, i)
			}

			assert.Equal(t, off.r.String(), on.r.String(), "capturing leaves the recording as it is")
			assert.Equal(t, off.timeouts, on.timeouts)
		})
	}
}

func TestTraceCaptureReproducesReplay(t *testing.T) {
	for _, seed := range captureSeeds {
		t.Run(seed.name, func(t *testing.T) {
			traces := loadSeed(t, seed.traces...)
			var run captureRun
			synctest.Test(t, func(t *testing.T) {
				run = replayCaptured(t, headerCapture(), traces...)
			})

			for i, in := range traces {
				got := run.read(t, i)
				info, _ := traceInfo(t, i, in)

				assert.Equal(t, corpus.Header{
					V: corpus.Version, Kind: in.Header.Kind, Codec: in.Header.Codec, ClockRate: in.Header.ClockRate,
					SSRC: in.Header.SSRC, TrackID: in.Header.TrackID, EpochUnixUs: run.epoch.UnixMicro(),
					Provenance: corpus.ProvenanceCapture,
					Generator:  "bbb-webrtc-recorder devel traceCapture/header",
				}, got.Header)
				assert.Equal(t, filepath.Join(run.dir, "replay."+strings.ReplaceAll(info.id, "/", "_")+".trace.ndjson.gz"), run.paths[i])

				// Read times on the session's clock, which is the replay's.
				assert.Equal(t, readViews(in, asCaptured), readViews(got, asCaptured), "rtp records, header payload prefixes included")
				assert.Len(t, recordsOf(got, corpus.RecordTimeout), run.timeouts[i], "a timeout record per timed-out read")

				eof := got.Records[len(got.Records)-1]
				assert.Equal(t, corpus.Record{T: corpus.RecordEOF, AtUs: in.Records[len(in.Records)-1].AtUs}, eof, "the read loop's own eof")

				if info.kind == TrackKindVideo {
					assert.Equal(t, microsOf(run.r.bufferRequests), plisAt(got, "packet_drop"), "the track buffer's requests")
					assert.Equal(t, microsOf(run.r.writerRequests), plisAt(got, "direct_request"), "the writer's requests")
				} else {
					assert.Empty(t, recordsOf(got, corpus.RecordPLI))
				}
			}
		})
	}
}

func TestTraceCaptureRequestsAndRTXPresent(t *testing.T) {
	// Guards the seed choice: a reproduction that holds trivially proves nothing.
	var c10, c12 captureRun
	synctest.Test(t, func(t *testing.T) { c10 = replayCaptured(t, headerCapture(), loadSeed(t, "c10-two-losses-1500ms")...) })
	synctest.Test(t, func(t *testing.T) { c12 = replayCaptured(t, headerCapture(), loadSeed(t, "c12-lkprobe-rtxdup")...) })

	got := c10.read(t, 0)
	assert.Len(t, plisAt(got, "packet_drop"), 2)
	assert.Len(t, plisAt(got, "direct_request"), 1)

	rtx := 0
	for _, r := range recordsOf(c12.read(t, 0), corpus.RecordRTP) {
		if r.RTX {
			rtx++
		}
	}
	assert.Equal(t, 42, rtx)
}

// Header mode keeps an Opus payload of up to 3 bytes whole: DTX frames,
// which carry no media.
func TestTraceCaptureKeepsOpusDTXFrames(t *testing.T) {
	tr := loadSeed(t, "c02-opus-tail-blank")[0]
	// The seed's blank pair records 3 bytes of longer frames; cut to those
	// bytes, they stand in for DTX frames.
	blank := 0
	for i, r := range tr.Records {
		if bytes.Equal(r.Pay, []byte{0xf8, 0xff, 0xfe}) {
			tr.Records[i].PLen = len(r.Pay)
			blank++
		}
	}
	require.Equal(t, 2, blank)

	var run captureRun
	synctest.Test(t, func(t *testing.T) { run = replayCaptured(t, headerCapture(), tr) })

	assert.Equal(t, readViews(tr, asCaptured), readViews(run.read(t, 0), asCaptured), "byte for byte")
}

func TestTraceCaptureFullPayload(t *testing.T) {
	traces := loadSeed(t, "c12-lkprobe-rtxdup", "baseline-av.audio")
	var run captureRun
	synctest.Test(t, func(t *testing.T) {
		run = replayCaptured(t, config.TraceCapture{Enabled: true, Payload: config.TraceCapturePayloadFull}, traces...)
	})

	for i, in := range traces {
		got := run.read(t, i)
		assert.Equal(t, "bbb-webrtc-recorder devel traceCapture/full", got.Header.Generator)
		assert.Equal(t, readViews(in, fullPayload), readViews(got, asCaptured), "whole payloads")
	}
}

// captureLog collects the trace capture messages logged at level while f runs.
func captureLog(level log.Level, f func()) []string {
	std := log.StandardLogger()
	defer std.ReplaceHooks(std.ReplaceHooks(make(log.LevelHooks)))
	hook := logtest.NewLocal(std)

	f()

	var messages []string
	for _, e := range hook.AllEntries() {
		if e.Level == level && strings.Contains(strings.ToLower(e.Message), "trace capture") {
			messages = append(messages, e.Message)
		}
	}

	return messages
}

func captureWarnings(f func()) []string { return captureLog(log.WarnLevel, f) }

func TestTraceCaptureByteCap(t *testing.T) {
	const capBytes = 8 << 10
	traces := loadSeed(t, "baseline-av.video")
	var capped, uncapped captureRun
	var warnings []string
	synctest.Test(t, func(t *testing.T) {
		uncapped = replayCaptured(t, headerCapture(), traces...)
	})
	synctest.Test(t, func(t *testing.T) {
		warnings = captureWarnings(func() {
			capped = replayCaptured(t, config.TraceCapture{Enabled: true, Payload: config.TraceCapturePayloadHeader, MaxBytesPerTrack: capBytes}, traces...)
		})
	})

	assert.Equal(t, []string{fmt.Sprintf("Trace capture truncated at its %d-byte cap", capBytes)}, warnings, "one warning")
	assert.Equal(t, uncapped.r.String(), capped.r.String(), "the cap leaves the recording as it is")

	got := capped.read(t, 0)
	eof := got.Records[len(got.Records)-1]
	assert.Equal(t, corpus.RecordEOF, eof.T)
	assert.Equal(t, truncatedReason, eof.Reason)

	gotReads, allReads := readViews(got, asCaptured), readViews(uncapped.read(t, 0), asCaptured)
	require.NotEmpty(t, gotReads)
	require.Less(t, len(gotReads), len(allReads))
	assert.Equal(t, allReads[:len(gotReads)], gotReads, "a prefix of the uncapped capture")
	assert.Equal(t, gotReads[len(gotReads)-1].AtUs, eof.AtUs, "cut right after the read that passed the cap")

	assert.Greater(t, capped.sizes[0], int64(capBytes))
	assert.Less(t, capped.sizes[0], int64(capBytes+4<<10), "past the cap by less than a flush interval")
}

// failingWriter fails every write once it has taken limit bytes.
type failingWriter struct {
	mu     sync.Mutex
	limit  int
	n      int
	writes int // after the limit
	closed bool
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.n+len(p) > w.limit {
		w.writes++

		return 0, errors.New("disk full")
	}
	w.n += len(p)

	return len(p), nil
}

func (w *failingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true

	return nil
}

func TestTraceCaptureWriteErrorDisablesIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := newPushRecorder()
		h := newLoopHarness(rec, audioInfo.id)
		out := &failingWriter{limit: 64}
		h.w.cfg.TraceCapture = headerCapture()
		c, err := newTrackCapture(out, corpus.Header{V: corpus.Version, Kind: corpus.KindAudio, Codec: corpus.CodecOpus,
			ClockRate: 48000, SSRC: audioInfo.ssrc, Provenance: corpus.ProvenanceCapture}, false, 0, h.w.sinceStart,
			log.WithField("trackID", audioInfo.id))
		require.NoError(t, err)
		h.w.traceCaptures = map[string]*trackCapture{audioInfo.id: c}

		warnings := captureWarnings(func() { h.run(newFakeTrack(audioReads(0, 100, 300)), audioInfo) })

		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "Trace capture stopped on a write error: ")
		assert.Equal(t, 1, out.writes, "nothing written after the error")
		assert.True(t, out.closed)
		assert.Equal(t, seqRange(100, 300), rec.audio, "recording goes on")
		states, _ := h.reported()
		assert.Equal(t, []utils.ConnectionState{utils.ConnectionStateClosed}, states)
	})
}

// Close can come before the read loop ends: it ends the trace, and the
// loop's own close and a subscription landing after it do nothing.
func TestTraceCaptureClosedFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := newPushRecorder()
		rec.filePath = filepath.Join(t.TempDir(), "rec.webm")
		h := newLoopHarness(rec, audioInfo.id, videoInfo.id)
		h.w.cfg.TraceCapture = headerCapture()
		h.w.prepareTrack(audioInfo, 48000)

		done := make(chan struct{})
		go func() {
			defer close(done)
			h.run(newFakeTrack(audioReads(0, 100, 50)), audioInfo)
		}()

		time.Sleep(510 * time.Millisecond)
		h.w.Close()
		h.w.Close()
		<-done

		h.w.prepareTrack(videoInfo, 90000)
		assert.NoFileExists(t, traceCapturePath(rec.filePath, videoInfo.id), "no trace after Close")
		assert.Equal(t, seqRange(100, 50), rec.audio, "recording goes on")

		got, err := corpus.ReadFile(traceCapturePath(rec.filePath, audioInfo.id))
		require.NoError(t, err)
		require.NoError(t, got.Validate())

		reads := recordsOf(got, corpus.RecordRTP)
		require.Len(t, reads, 26, "the reads up to 500 ms")
		assert.Equal(t, uint16(125), reads[25].Seq)
		assert.Equal(t, corpus.Record{T: corpus.RecordEOF, AtUs: 510000}, got.Records[len(got.Records)-1])
	})
}

// A track subscribed later reads on the same clock as the first one, from
// the session's start, not from its own.
func TestTraceCaptureSharesEpoch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		second := audioInfo
		second.id, second.ssrc = "audio-2", 2222
		rec := newPushRecorder()
		rec.filePath = filepath.Join(t.TempDir(), "rec.webm")
		h := newLoopHarness(rec, audioInfo.id, second.id)
		h.w.cfg.TraceCapture = headerCapture()

		var wg sync.WaitGroup
		h.w.prepareTrack(audioInfo, 48000)
		first := newFakeTrack(audioReads(0, 100, 50))
		wg.Go(func() { h.run(first, audioInfo) })

		time.Sleep(300 * time.Millisecond)
		h.w.prepareTrack(second, 48000)
		later := newFakeTrack(audioReads(15*time.Millisecond, 500, 50))
		wg.Go(func() { h.run(later, second) })
		wg.Wait()

		atUs := func(info trackInfo) []int64 {
			got, err := corpus.ReadFile(traceCapturePath(rec.filePath, info.id))
			require.NoError(t, err)
			require.NoError(t, got.Validate())

			var at []int64
			for _, r := range recordsOf(got, corpus.RecordRTP) {
				at = append(at, r.AtUs)
			}

			return at
		}
		want := func(from time.Duration) []int64 {
			var at []int64
			for i := range 50 {
				at = append(at, (from + time.Duration(i)*20*time.Millisecond).Microseconds())
			}

			return at
		}

		assert.Equal(t, want(0), atUs(audioInfo))
		assert.Equal(t, want(315*time.Millisecond), atUs(second))
	})
}

// A track subscribed again starts its trace over, which the earlier
// capture does not corrupt.
func TestTraceCaptureReopen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := newPushRecorder()
		rec.filePath = filepath.Join(t.TempDir(), "rec.webm")
		h := newLoopHarness(rec, audioInfo.id)
		h.w.cfg.TraceCapture = headerCapture()

		h.w.prepareTrack(audioInfo, 48000)
		first := h.w.traceCaptures[audioInfo.id]
		// Past a flush, so that it has written more than the second one will.
		for seq := range uint16(200) {
			first.observe(opusPacket(seq), nil, nil)
		}
		h.w.prepareTrack(audioInfo, 48000)
		first.observe(opusPacket(200), nil, nil)
		h.run(newFakeTrack(audioReads(0, 100, 10)), audioInfo)

		got, err := corpus.ReadFile(traceCapturePath(rec.filePath, audioInfo.id))
		require.NoError(t, err)
		require.NoError(t, got.Validate())
		reads := recordsOf(got, corpus.RecordRTP)
		require.Len(t, reads, 10)
		assert.Equal(t, uint16(100), reads[0].Seq)
	})
}

// heldWriter lets its first write through, the trace header, and holds
// every later one until gate is closed.
type heldWriter struct {
	gate   chan struct{}
	writes int
	buf    bytes.Buffer
}

func (w *heldWriter) Write(p []byte) (int, error) {
	if w.writes++; w.writes > 1 {
		<-w.gate
	}

	return w.buf.Write(p)
}

func (w *heldWriter) Close() error { return nil }

func audioCaptureHeader() corpus.Header {
	return corpus.Header{V: corpus.Version, Kind: corpus.KindAudio, Codec: corpus.CodecOpus,
		ClockRate: 48000, SSRC: audioInfo.ssrc, Provenance: corpus.ProvenanceCapture}
}

// A writer held up on its file drops what the queue cannot hold rather than
// hold up the caller, and the trace says so.
func TestTraceCaptureDropsWhenBehind(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &heldWriter{gate: make(chan struct{})}
		start := time.Now()
		c, err := newTrackCapture(out, audioCaptureHeader(), false, 0,
			func() int64 { return time.Since(start).Microseconds() }, log.WithField("trackID", audioInfo.id))
		require.NoError(t, err)

		// The first flush interval reaches the file, which holds the writer.
		const flushed = 64
		for seq := range uint16(flushed) {
			c.observe(opusPacket(seq), nil, nil)
		}
		synctest.Wait()

		const dropped = 10
		for seq := range uint16(captureQueueLen + dropped) {
			c.observe(opusPacket(flushed+seq), nil, nil)
		}
		c.keyframeRequest("packet_drop")

		time.Sleep(time.Second)
		warnings := captureWarnings(func() {
			close(out.gate)
			c.close()
		})

		assert.Equal(t, []string{fmt.Sprintf("Trace capture dropped %d records, its writer falling behind", dropped+1)}, warnings)

		got, err := corpus.Read(&out.buf)
		require.NoError(t, err)
		require.NoError(t, got.Validate())
		reads := recordsOf(got, corpus.RecordRTP)
		require.Len(t, reads, flushed+captureQueueLen)
		assert.Equal(t, uint16(flushed+captureQueueLen-1), reads[len(reads)-1].Seq, "the queued records, in order")
		assert.Empty(t, recordsOf(got, corpus.RecordPLI), "dropped with the queue full")
		assert.Equal(t, corpus.Record{T: corpus.RecordEOF, AtUs: time.Second.Microseconds(), Reason: droppedReason},
			got.Records[len(got.Records)-1])
	})
}

// Records queued out of time order keep the queue's order, each at no time
// before its predecessor's.
func TestTraceCaptureClampsQueueOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &heldWriter{gate: make(chan struct{})}
		close(out.gate)
		stamps := []int64{100, 40, 120, 130}
		now := func() int64 {
			at := stamps[0]
			stamps = stamps[1:]

			return at
		}
		c, err := newTrackCapture(out, audioCaptureHeader(), false, 0, now, log.WithField("trackID", audioInfo.id))
		require.NoError(t, err)

		c.keyframeRequest("direct_request")
		c.observe(opusPacket(1), nil, nil)
		c.observe(opusPacket(2), nil, nil)
		c.close()

		got, err := corpus.Read(&out.buf)
		require.NoError(t, err)
		var at []int64
		for _, r := range got.Records {
			at = append(at, r.AtUs)
		}
		assert.Equal(t, []int64{100, 100, 120, 130}, at)
	})
}

func TestTraceCaptureConfigFallbacks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := newPushRecorder()
		rec.filePath = filepath.Join(t.TempDir(), "rec.webm")
		h := newLoopHarness(rec, audioInfo.id)
		h.w.cfg.TraceCapture = config.TraceCapture{Enabled: true, Payload: "FULL", MaxBytesPerTrack: -1}

		warnings := captureWarnings(func() { h.w.prepareTrack(audioInfo, 48000) })

		assert.Equal(t, []string{
			`Unknown trace capture payload "FULL", capturing "header"`,
			fmt.Sprintf("Negative trace capture maxBytesPerTrack -1, capping at %d", config.DefaultTraceCaptureMaxBytesPerTrack),
		}, warnings)
		c := h.w.traceCaptures[audioInfo.id]
		require.NotNil(t, c)
		assert.False(t, c.full)
		assert.Equal(t, int64(config.DefaultTraceCaptureMaxBytesPerTrack), c.maxBytes)
		h.w.Close()
	})
}

// A recording written to /dev/null gets no traces, which would land in /dev.
func TestTraceCaptureSkipsDevNull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := newPushRecorder()
		rec.filePath = os.DevNull
		h := newLoopHarness(rec, audioInfo.id)
		h.w.cfg.TraceCapture = headerCapture()

		infos := captureLog(log.InfoLevel, func() { h.w.prepareTrack(audioInfo, 48000) })

		assert.Equal(t, []string{"Trace capture skipped for track audio: the recording goes to /dev/null"}, infos)
		assert.Empty(t, h.w.traceCaptures)
		assert.NoFileExists(t, traceCapturePath(os.DevNull, audioInfo.id))
	})
}

// The queue holds copies: the read loop may reuse a packet once observed.
func TestTraceCaptureCopiesPayload(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%v", full), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				out := &heldWriter{gate: make(chan struct{})}
				c, err := newTrackCapture(out, audioCaptureHeader(), full, 0,
					func() int64 { return 0 }, log.WithField("trackID", audioInfo.id))
				require.NoError(t, err)

				// The first flush interval reaches the file, which holds the writer.
				for seq := range uint16(64) {
					c.observe(opusPacket(seq), nil, nil)
				}
				synctest.Wait()

				pkt := opusPacket(64)
				pkt.Payload = []byte{0xFC, 0x01, 0x02, 0x03, 0x04}
				want := slices.Clone(pkt.Payload)
				if !full {
					want = want[:1]
				}
				c.observe(pkt, nil, nil)
				clear(pkt.Payload)

				close(out.gate)
				c.close()

				got, err := corpus.Read(&out.buf)
				require.NoError(t, err)
				reads := recordsOf(got, corpus.RecordRTP)
				require.Len(t, reads, 65)
				assert.Equal(t, want, reads[64].Pay)
			})
		})
	}
}

// A stalled trace file holds up neither the reads nor the track's report.
func TestTraceCaptureStallDoesNotHoldReport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &heldWriter{gate: make(chan struct{})}
		h := newLoopHarness(newPushRecorder(), audioInfo.id)
		c, err := newTrackCapture(out, audioCaptureHeader(), false, 0,
			func() int64 { return 0 }, log.WithField("trackID", audioInfo.id))
		require.NoError(t, err)
		h.w.traceCaptures = map[string]*trackCapture{audioInfo.id: c}

		src := newFakeTrack(audioReads(0, 100, 200))
		done := make(chan struct{})
		go func() {
			h.run(src, audioInfo)
			close(done)
		}()

		time.Sleep(time.Minute)
		states, _ := h.reported()
		assert.Equal(t, []utils.ConnectionState{utils.ConnectionStateClosed}, states)
		assert.Empty(t, src.reads)

		close(out.gate)
		<-done
	})
}

// LiveKit sends padding probes ahead of a track's first key frame. The jitter
// buffer drops them before it starts; replay must not take them for a frame
// that never completes.
func TestTraceCapturePaddingProbes(t *testing.T) {
	in := loadSeed(t, "baseline-av.video")[0]
	first := in.Records[0]
	probes := make([]corpus.Record, 0, 3)
	for i := range 3 {
		probes = append(probes, corpus.Record{T: corpus.RecordRTP, AtUs: first.AtUs,
			Seq: first.Seq - uint16(3-i), TS: first.TS - 3000, PT: first.PT, Pad: true, PLen: 255, Pay: []byte{0}})
	}
	in.Records = append(probes, in.Records...)
	require.NoError(t, in.Validate())

	var live, again captureRun
	synctest.Test(t, func(t *testing.T) {
		live = replayCaptured(t, headerCapture(), in)
	})
	got := live.read(t, 0)
	synctest.Test(t, func(t *testing.T) {
		again = replayCaptured(t, headerCapture(), got)
	})

	assert.Empty(t, live.r.requests(), "the probes are no loss")
	assert.Equal(t, readViews(in, asCaptured)[:3], readViews(got, asCaptured)[:3], "the probes keep their padding bit")
	assert.Equal(t, live.r.String(), again.r.String(), "a capture replays as the session it records")
}

type discardCloser struct{ io.Writer }

func (discardCloser) Close() error { return nil }

// The loss handler also records its request in the track's capture, still
// without the adapter lock.
func TestJitterLossHandlerWithCaptureDoesNotTakeAdapterLock(t *testing.T) {
	lk, _ := setupMockLK()
	defer lk.Close()

	lk.cfg.TraceCapture.Enabled = true
	c, err := newTrackCapture(discardCloser{io.Discard}, corpus.Header{V: corpus.Version, Kind: corpus.KindVideo,
		Codec: corpus.CodecVP8, ClockRate: 90000, SSRC: 1234, Provenance: corpus.ProvenanceCapture},
		false, 0, lk.sinceStart, log.WithField("trackID", "test-track"))
	require.NoError(t, err)
	lk.traceCaptures = map[string]*trackCapture{"test-track": c}

	buffer := lk.addJitterBuffer("test-track", &codecs.VP8Packet{}, jitterLatency, 1234, true)
	defer buffer.close()

	lk.m.Lock()
	lostAfterGap(buffer)

	lost := make(chan struct{})

	go func() {
		defer close(lost)

		for buffer.stats().PacketsLost == 0 {
			time.Sleep(time.Millisecond)
		}
	}()

	assert.Eventually(t, closed(lost), jitterLatency+time.Second, time.Millisecond,
		"a lost packet blocked on the adapter lock")
	lk.m.Unlock()
	<-lost
}
