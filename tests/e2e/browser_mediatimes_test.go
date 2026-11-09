//go:build e2e && linux

package e2e

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/google/uuid"
	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ntpEpoch is the NTP era's start, 1900-01-01, in Unix seconds.
const ntpEpoch = -2208988800

func ntpTime(ntp uint64) time.Time {
	return time.Unix(int64(ntp>>32)+ntpEpoch, int64((ntp&0xFFFFFFFF)*uint64(time.Second)>>32))
}

// senderClock maps a track's RTP timestamps to its sender's capture clock
// through the sender reports in its trace. LiveKit forwards them with the
// publisher's NTP time and the downtrack's RTP time.
type senderClock struct {
	rate    uint32
	reports []corpus.Record
	// cut, if not 0, splits the trace in two by read time, µs since its
	// epoch: segment 1 is what was read from cut on. After a blackout
	// LiveKit can re-base the downtrack's timestamps, and builds its next
	// reports from the publisher's new ones. A report from one side of the
	// cut then maps the other side's timestamps wrongly.
	cut int64
}

func newSenderClock(tr *corpus.Trace) senderClock {
	c := senderClock{rate: tr.Header.ClockRate}
	for _, r := range tr.Records {
		if r.T == corpus.RecordSR {
			c.reports = append(c.reports, r)
		}
	}

	return c
}

func (c senderClock) segment(atUs int64) int {
	if c.cut > 0 && atUs >= c.cut {
		return 1
	}

	return 0
}

// atIn returns when the sender captured the media at ts, by the report
// read in segment seg nearest to it. It returns false if that segment has
// none.
func (c senderClock) atIn(ts uint32, seg int) (time.Time, bool) {
	var in []corpus.Record
	for _, r := range c.reports {
		if c.segment(r.AtUs) == seg {
			in = append(in, r)
		}
	}

	if len(in) == 0 {
		return time.Time{}, false
	}

	return c.by(ts, in), true
}

func (c senderClock) by(ts uint32, reports []corpus.Record) time.Time {
	sr := slices.MinFunc(reports, func(a, b corpus.Record) int {
		return cmp.Compare(abs64(int64(int32(ts-a.TS))), abs64(int64(int32(ts-b.TS))))
	})

	return ntpTime(sr.NTP).Add(time.Duration(int32(ts-sr.TS)) * time.Second / time.Duration(c.rate))
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}

	return v
}

func fullTracePayloads(t *testing.T) func(*Stack, map[string]any) {
	return func(_ *Stack, cfg map[string]any) {
		lookup[map[string]any](t, cfg, "livekit", "traceCapture")["payload"] = config.TraceCapturePayloadFull
	}
}

type readFrame struct {
	TS   uint32
	Read int64  // earliest read, µs since the trace's epoch
	Data []byte // as the recorder writes it
}

// readFrames returns the frames of tr, a trace with whole payloads, in the
// order of their first reads. A VP8 frame is its packets' payloads, less
// their payload descriptors, in sequence order. An Opus frame is a packet.
func readFrames(t *testing.T, tr *corpus.Trace) []*readFrame {
	t.Helper()

	type packet struct {
		seq uint16
		pay []byte
	}
	type frameKey struct {
		ts  uint32
		seq uint16 // Opus only
	}

	var frames []*readFrame
	var keys []frameKey
	index := map[frameKey]*readFrame{}
	packets := map[frameKey][]packet{}
	for _, r := range tr.Records {
		if r.T == corpus.RecordEOF {
			require.NotContains(t, []string{"truncated", "dropped"}, r.Reason, "the trace misses reads")
		}
		if r.T != corpus.RecordRTP || r.Pad {
			continue
		}
		require.Len(t, r.Pay, r.PLen, "the trace must hold whole payloads")

		k := frameKey{ts: r.TS}
		// LiveKit's first blank frame on a mute can reuse the last
		// timestamp. The recorder skips it.
		if tr.Header.Codec != corpus.CodecVP8 {
			k.seq = r.Seq
		}
		f := index[k]
		if f == nil {
			f = &readFrame{TS: r.TS, Read: r.AtUs}
			index[k] = f
			frames = append(frames, f)
			keys = append(keys, k)
		}
		f.Read = min(f.Read, r.AtUs)

		// Retransmitted and repeated reads carry a packet already read.
		if !slices.ContainsFunc(packets[k], func(p packet) bool { return p.seq == r.Seq }) {
			packets[k] = append(packets[k], packet{seq: r.Seq, pay: r.Pay})
		}
	}

	for i, f := range frames {
		ps := packets[keys[i]]
		first := ps[0].seq
		slices.SortFunc(ps, func(a, b packet) int { return cmp.Compare(int16(a.seq-first), int16(b.seq-first)) })

		for _, p := range ps {
			if tr.Header.Codec != corpus.CodecVP8 {
				f.Data = append(f.Data, p.pay...)
				continue
			}

			var vp8 codecs.VP8Packet
			_, err := vp8.Unmarshal(p.pay)
			require.NoError(t, err)
			f.Data = append(f.Data, vp8.Payload...)
		}
	}

	return frames
}

// pairedBlock is a WebM block and the frame it holds.
type pairedBlock struct {
	PTS   int64 // ms
	Frame *readFrame
}

// pairBlocks pairs each block of the WebM at path with the frame that holds
// the same bytes. Frames can share bytes, e.g. Opus silence, and the
// recorder can skip one of them. A block then takes the one its PTS step
// points at.
func pairBlocks(t *testing.T, path string, frames []*readFrame, rate uint32) []pairedBlock {
	t.Helper()

	byData := map[string][]int{}
	for i, f := range frames {
		byData[string(f.Data)] = append(byData[string(f.Data)], i)
	}

	paired := make([]bool, len(frames))
	var out []pairedBlock
	for _, b := range readBlocks(t, path) {
		off := func(i int) int64 {
			prev := out[len(out)-1]
			return abs64(int64(int32(frames[i].TS-prev.Frame.TS)) - (b.PTS-prev.PTS)*int64(rate)/1000)
		}

		pick := -1
		for _, i := range byData[string(b.Data)] {
			if !paired[i] && (pick < 0 || len(out) > 0 && off(i) < off(pick)) {
				pick = i
			}
		}
		require.GreaterOrEqual(t, pick, 0, "block at %d ms holds no frame read, or one already written", b.PTS)

		paired[pick] = true
		out = append(out, pairedBlock{PTS: b.PTS, Frame: frames[pick]})
	}

	return out
}

const (
	// Viewers notice audio that leads video by 45 ms (ITU-R BT.1359). The
	// bound includes the encoders' own delays, which differ between audio
	// and video and are part of any WebRTC call.
	browserLipSyncBound = 45 * time.Millisecond
	// How far a block's lateness may drift from the first block's (Late).
	// Chromium's microphone sender reports can disagree with each other by
	// over 10 ms.
	browserDriftBound = 30 * time.Millisecond
	// Chromium's sender reports can place a microphone's capture up to about
	// 15 ms late. So on loopback its frames can seem to be read, and its
	// file to play, up to 15 ms before their capture. 25 ms leaves a margin.
	browserReportError = 25 * time.Millisecond
	// A camera's reports place its capture closely: on loopback its file
	// plays its media 0.2 ms or more after its capture. 5 ms allows for the
	// event's truncation to the ms and a report a few ms off.
	browserCameraReportError = 5 * time.Millisecond
	// How late a file may play its media after its capture on loopback.
	// There the least delayed frame is read at most 3 ms after its capture,
	// through Chromium's encoder and pacer and the read path. 30 ms is a
	// chosen margin of about 10 times that for a slower runner, not a
	// derived bound. startByReads checks the recorder's own delay.
	browserLateMax = 30 * time.Millisecond
	// The read loop stamps a packet's arrival for the recorder after the
	// trace takes its read time. Preemption on a loaded runner, under -race
	// too, can put a few ms between the two.
	readToArrival = 5 * time.Millisecond
)

// reportError is how late the sender reports of a browser track of kind can
// place a capture.
func reportError(kind corpus.Kind) time.Duration {
	if kind == corpus.KindVideo {
		return browserCameraReportError
	}

	return browserReportError
}

type capturePlacement struct {
	Name     string
	Late     time.Duration // how late the first block plays after its frame's capture
	Worst    time.Duration // the blocks' worst drift from Late
	Start    time.Time     // mediaStart
	Read     time.Time     // when the first block's frame was read
	Started  events.RecordingMediaStarted
	Blocks   []pairedBlock
	Segments []int // each block's segment of Clock
	Clock    senderClock
	Out      *Output
}

func (p capturePlacement) kind() corpus.Kind { return p.Out.Trace.Header.Kind }

func (p capturePlacement) placed(b pairedBlock) time.Time {
	return p.Start.Add(time.Duration(b.PTS-p.Blocks[0].PTS) * time.Millisecond)
}

// captured returns when the sender captured the frame of block i, by the
// reports of its segment.
func (p capturePlacement) captured(i int) time.Time {
	at, _ := p.Clock.atIn(p.Blocks[i].Frame.TS, p.Segments[i])
	return at
}

// startByReads returns where the reads of the start window place the first
// block. Each block's read, less its PTS after the first block, gives a
// start; it takes the earliest, at most 1 s before the first read.
//
// It stops at or before the frame where the recorder settles, on purpose.
// The recorder stamps the same frames' arrivals at most readToArrival
// after their reads, so its start is at most readToArrival after this one.
// It can be earlier, so the check against it is one-sided.
func (p capturePlacement) startByReads() time.Time {
	// 120 ms and the 1 s floor below are the recorder's unexported
	// mediaSpanAudioJumpStep and mediaSpanMaxBacklog.
	window, jumpStep := recorder.MediaSpanWindow, 120*time.Millisecond
	if p.Out.Trace.Header.Kind == corpus.KindVideo {
		jumpStep = 0
	}

	start := p.Read
	for i := 1; i < len(p.Blocks); i++ {
		after := time.Duration(p.Blocks[i].PTS-p.Blocks[0].PTS) * time.Millisecond
		step := time.Duration(p.Blocks[i].PTS-p.Blocks[i-1].PTS) * time.Millisecond
		if after >= window || jumpStep > 0 && step >= jumpStep {
			break
		}
		if at := traceTime(p.Out.Trace, p.Blocks[i].Frame.Read).Add(-after); at.Before(start) {
			start = at
		}
	}

	if floor := p.Read.Add(-time.Second); start.Before(floor) {
		return floor
	}

	return start
}

// placeOnCaptureClock places each block of rec, and finds the capture time
// of the frame it holds through the sender reports of the browser track
// sid. The stack must keep whole payloads in its traces (fullTracePayloads).
//
// mediaStart may be up to 1 s and 1 ms before the first block's frame was
// read. The recorder moves it back up to 1 s, and the event drops its sub-ms
// part. The other bounds are the constants above.
func placeOnCaptureClock(ctx context.Context, t *testing.T, s *Stack, name string, rec *Recording, sid string) capturePlacement {
	t.Helper()

	return placeOnCaptureClockCut(ctx, t, s, name, rec, sid, time.Time{}, 0)
}

// placeOnCaptureClockCut is placeOnCaptureClock for a file cut by a
// blackout. cutAfter is the blackout's end: the first media read at or after
// it starts a second segment, and a zero cutAfter cuts nothing. Each block's
// frame gets its capture time from the reports read in its own segment, which
// must have one. browserLateMax grows by pathDelay, a shaped link's least
// delay.
func placeOnCaptureClockCut(ctx context.Context, t *testing.T, s *Stack, name string, rec *Recording, sid string,
	cutAfter time.Time, pathDelay time.Duration,
) capturePlacement {
	t.Helper()

	out := ReadOutput(t, rec, sid)
	clock := newSenderClock(out.Trace)
	require.NotEmpty(t, clock.reports, "%s: no sender report", name)

	if !cutAfter.IsZero() {
		for _, r := range out.Trace.Records {
			if r.T == corpus.RecordRTP && !r.Pad && !traceTime(out.Trace, r.AtUs).Before(cutAfter) {
				clock.cut = r.AtUs
				break
			}
		}
	}

	started, _, err := s.Driver.WaitMediaStarted(ctx, rec)
	require.NoError(t, err, "%s: recordingMediaStarted", name)

	p := capturePlacement{
		Name:    name,
		Start:   time.UnixMilli(started.MediaStartUTC),
		Started: started,
		Blocks:  pairBlocks(t, out.WebMPath, readFrames(t, out.Trace), out.Trace.Header.ClockRate),
		Clock:   clock,
		Out:     out,
	}

	for i, b := range p.Blocks {
		p.Segments = append(p.Segments, clock.segment(b.Frame.Read))
		_, ok := clock.atIn(b.Frame.TS, p.Segments[i])
		require.True(t, ok, "%s: no sender report in segment %d, which holds the block at %d ms", name, p.Segments[i], b.PTS)
	}

	p.Read = traceTime(out.Trace, p.Blocks[0].Frame.Read)
	byReads := p.startByReads()
	assert.WithinRange(t, p.Start, p.Read.Add(-time.Second-time.Millisecond), byReads.Add(readToArrival),
		"%s: mediaStart against the reads of its start window", name)

	captured := p.captured(0)
	p.Late = p.Start.Sub(captured)
	lateMin, lateMax := -reportError(out.Trace.Header.Kind), browserLateMax+pathDelay
	assert.GreaterOrEqual(t, p.Late, lateMin, "%s: mediaStart plays the media before its capture", name)
	assert.LessOrEqual(t, p.Late, lateMax, "%s: how late mediaStart plays the media", name)
	for i, b := range p.Blocks {
		drift := p.placed(b).Sub(p.captured(i)) - p.Late
		if drift.Abs() > p.Worst.Abs() {
			p.Worst = drift
		}
	}
	assert.LessOrEqual(t, p.Worst.Abs(), browserDriftBound, "%s: drift against the capture clock", name)

	segments := 1
	if clock.cut > 0 {
		segments = 2
	}
	flowingAt := firstFlowing(s.Driver, rec.From, rec.SessionID)
	t.Logf("%s: %d blocks, %d sender reports, %d segments; mediaStart plays the media %v after its capture "+
		"(%v under the bound), drifting %v at worst; mediaStart is %v after the start its window's reads give; "+
		"the first block was read %v after its capture; a start stamped on flowing would play it %v after",
		name, len(p.Blocks), len(clock.reports), segments, p.Late, lateMax-p.Late, p.Worst, p.Start.Sub(byReads),
		p.Read.Sub(captured), flowingAt.Sub(captured))

	return p
}

// cutStep is how a file steps across the cut of its clock: from its last
// block before the cut to its first after it.
type cutStep struct {
	After   int           // the index of the first block after the cut
	PTS     time.Duration // the two blocks' PTS step
	Capture time.Duration // their capture step, each by its own segment's reports
	// The trace's jump from the last media read before the cut to the first
	// at it.
	SeqJump int
	TSJump  time.Duration
}

// stepAcrossCut returns p's step across its cut, or false if p has no
// block on one side of it.
func (p capturePlacement) stepAcrossCut() (cutStep, bool) {
	i := slices.IndexFunc(p.Segments, func(seg int) bool { return seg > 0 })
	if i <= 0 {
		return cutStep{}, false
	}

	st := cutStep{
		After:   i,
		PTS:     time.Duration(p.Blocks[i].PTS-p.Blocks[i-1].PTS) * time.Millisecond,
		Capture: p.captured(i).Sub(p.captured(i - 1)),
	}

	tr := p.Out.Trace
	var last *corpus.Record
	for j, r := range tr.Records {
		if r.T != corpus.RecordRTP || r.Pad {
			continue
		}
		if r.AtUs < p.Clock.cut {
			last = &tr.Records[j]
			continue
		}
		if last != nil {
			st.SeqJump = int(int16(r.Seq - last.Seq))
			st.TSJump = time.Duration(int32(r.TS-last.TS)) * time.Second / time.Duration(tr.Header.ClockRate)
		}
		break
	}

	return st, true
}

// TestBrowserMediaTimestampsKeepLipSync records Chromium's camera and
// microphone, one session each, published as BBB's client publishes them.
// Each camera is one VP8 stream without simulcast, whose first frames
// Chromium sends late before it catches up:
//   - low: BBB's low profile. Its frames are on time from 600 ms in.
//   - low-u30: the hidden low-u30 profile, one layer at 30 kbps. Its first
//     on-time frame follows a 700 ms gap, about 1 s in.
//   - 5fps: the low profile captured at 5 fps, as a webcam in low light.
//     Its frames are 200 ms apart.
//
// So the camera's mediaStart must come from the frames that caught up, not
// from the late first ones.
//
// The sender reports map RTP timestamps to Chromium's capture clock, which
// on this host is the recorder's wall clock. A file's mediaStart, less its
// first frame's capture time, is how late it plays its media. The two
// files must agree, for lip sync.
//
// Each block must play as late as its file's first block.
func TestBrowserMediaTimestampsKeepLipSync(t *testing.T) {
	RequireBrowser(t)

	for _, c := range []struct {
		name    string
		profile string
		capture CameraCapture
		check   func(ctx context.Context, t *testing.T, s *Stack, room string, cam *BBBCamera)
	}{
		{name: "low", profile: "low"},
		{name: "low-u30", profile: "low-u30", check: func(ctx context.Context, t *testing.T, s *Stack, room string, cam *BBBCamera) {
			assert.Len(t, listedVP8Layers(ctx, t, s.LiveKit, room, cam.SID), 1, "camera: the layers LiveKit lists")
			require.Len(t, cam.Encodings, 1, "camera: the sender's encodings")
			assert.Equal(t, 30000, cam.Encodings[0].MaxBitrate, "camera: the sender's bitrate")
		}},
		{name: "5fps", profile: "low", capture: CameraCapture{FrameRate: 5}, check: func(ctx context.Context, t *testing.T, s *Stack, room string, cam *BBBCamera) {
			require.InDelta(t, 5, cam.Capture.FrameRate, 0.5, "camera: the capture frame rate")
			assert.Len(t, listedVP8Layers(ctx, t, s.LiveKit, room, cam.SID), 1, "camera: the layers LiveKit lists")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			const recordFor = 10 * time.Second

			s := NewStack(t, StackOptions{RecorderConfig: fullTracePayloads(t)})
			room := "e2e-browser-mediatimes-" + uuid.NewString()[:8]
			b := s.NewBrowser()
			peer := b.JoinAsBBB(room, "e2e-browser")
			mic := peer.PublishBBBMic("microphone")
			cam := peer.PublishBBBCameraCapturing("camera", c.profile, c.capture)

			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()

			if c.check != nil {
				c.check(ctx, t, s, room, cam)
			}

			recCam := record(ctx, t, s, room, cam.SID)
			recMic := record(ctx, t, s, room, mic.SID)

			time.Sleep(recordFor)

			for _, rec := range []*Recording{recCam, recMic} {
				_, err := s.Driver.StopAndWait(ctx, rec.SessionID)
				require.NoError(t, err)
			}

			var files []capturePlacement
			for _, c := range []struct {
				name string
				rec  *Recording
				sid  string
			}{{"camera", recCam, cam.SID}, {"microphone", recMic, mic.SID}} {
				files = append(files, placeOnCaptureClock(ctx, t, s, c.name, c.rec, c.sid))
			}

			assertFilesAgree(t, files, browserLipSyncBound)
		})
	}
}

const (
	// Chromium sends a sender report every 2.5 to 7.5 s, and LiveKit
	// forwards the latest every 3 s.
	chromiumReportIntervalMax = 7500 * time.Millisecond
	lkReportInterval          = 3 * time.Second
	// A report LiveKit forwards can be this old.
	lkReportAge = chromiumReportIntervalMax + lkReportInterval
	// A file, or a file's segment, that records this long reads a sender
	// report.
	srSegment = lkReportAge + 1500*time.Millisecond
)

// assertFilesAgree checks that the files play their media equally late,
// within bound. Every track of one browser shares its capture clock, so
// this holds whenever each file started.
func assertFilesAgree(t *testing.T, files []capturePlacement, bound time.Duration) {
	t.Helper()

	// A wall-clock step between two starts would shift the later files'
	// lateness by the step.
	var offsets []int64
	for _, f := range files {
		offsets = append(offsets, f.Started.MediaStartHR-f.Started.MediaStartUTC)
	}
	require.LessOrEqual(t, slices.Max(offsets)-slices.Min(offsets), hostClockStep.Milliseconds(),
		"the host's wall clock stepped between the files' starts: mediaStartHR - mediaStartUTC %v", offsets)

	var lines []string
	for i, a := range files {
		for _, b := range files[i+1:] {
			kinds := "cross-kind"
			if a.kind() == b.kind() {
				kinds = "same-kind"
			}
			d := a.Late - b.Late
			lines = append(lines, fmt.Sprintf("%s - %s (%s): %v, margin %v", a.Name, b.Name, kinds, d, bound-d.Abs()))
		}
	}
	t.Logf("lateness across files:\n  %s", strings.Join(lines, "\n  "))

	lates := make([]time.Duration, 0, len(files))
	for _, f := range files {
		lates = append(lates, f.Late)
	}
	assert.LessOrEqual(t, slices.Max(lates)-slices.Min(lates), bound, "lip sync across files")
}

func traceTime(tr *corpus.Trace, atUs int64) time.Time {
	return time.UnixMicro(tr.Header.EpochUnixUs + atUs)
}

// isOpusSilence reports whether pay is LiveKit's 80-byte Opus silence
// frame (OpusSilenceFrame in pkg/sfu/downtrack.go). An encoder's own
// silence frame can start with the same three bytes, but is shorter.
func isOpusSilence(pay []byte) bool {
	return len(pay) == 80 && bytes.HasPrefix(pay, []byte{0xF8, 0xFF, 0xFE}) && bytes.Count(pay[3:], []byte{0}) == len(pay)-3
}

// TestSenderClockSegments checks the split of a trace's sender reports at a
// blackout. After a 3 s read gap, the downtrack's timestamps move 500 ms
// ahead, as a LiveKit re-base on a resume can move them. The reports after
// the gap follow the new base. A report from before the gap then maps a
// frame after it 500 ms off; one from its own segment maps it right.
//
// A segment with no report maps nothing.
func TestSenderClockSegments(t *testing.T) {
	const (
		rate   = 48000
		frame  = 20 * time.Millisecond
		rebase = 500 * time.Millisecond
		delay  = 10 * time.Millisecond // capture to read
	)

	epoch := time.Unix(1_800_000_000, 0)
	ntp := func(at time.Time) uint64 {
		return uint64(at.Unix()-ntpEpoch)<<32 | uint64(at.Nanosecond())<<32/uint64(time.Second)
	}
	ticks := func(d time.Duration) uint32 { return uint32(d * rate / time.Second) }

	tr := &corpus.Trace{Header: corpus.NewSyntheticHeader(corpus.KindAudio, 1, "TestSenderClockSegments")}
	tr.Header.EpochUnixUs = epoch.UnixMicro()
	seq := uint16(0)
	add := func(from, to time.Duration, shift time.Duration, reportsAt ...time.Duration) {
		for c := from; c < to; c += frame {
			ts := ticks(c + shift)
			if slices.Contains(reportsAt, c) {
				tr.Records = append(tr.Records, corpus.Record{T: corpus.RecordSR, AtUs: (c + delay).Microseconds(), TS: ts, NTP: ntp(epoch.Add(c))})
			}
			tr.Records = append(tr.Records, corpus.Record{T: corpus.RecordRTP, AtUs: (c + delay).Microseconds(), Seq: seq, TS: ts, PLen: 1})
			seq++
		}
	}
	add(0, 5*time.Second, 0, time.Second, 4*time.Second)
	add(8*time.Second, 16*time.Second, rebase, 13*time.Second)

	clock := newSenderClock(tr)
	clock.cut = (8*time.Second + delay).Microseconds()
	require.Equal(t, 0, clock.segment((4*time.Second + delay).Microseconds()))
	require.Equal(t, 1, clock.segment((13*time.Second + delay).Microseconds()))

	captured := epoch.Add(8 * time.Second)
	ts := ticks(8*time.Second + rebase)
	at, ok := clock.atIn(ts, 1)
	require.True(t, ok)
	assert.WithinDuration(t, captured, at, time.Millisecond, "the segment's own report")
	assert.WithinDuration(t, captured.Add(rebase), clock.by(ts, clock.reports), time.Millisecond,
		"the nearest report, from before the gap, maps the frame by the old base")

	clock.cut = (14*time.Second + delay).Microseconds()
	_, ok = clock.atIn(ts, 1)
	assert.False(t, ok, "a segment with no report")
}
