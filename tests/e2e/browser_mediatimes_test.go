//go:build e2e && linux

package e2e

import (
	"cmp"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
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

// at returns when the sender captured the media at ts, by the report
// nearest to it.
func (c senderClock) at(ts uint32) time.Time {
	sr := slices.MinFunc(c.reports, func(a, b corpus.Record) int {
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
// their payload descriptors, in sequence order.
func readFrames(t *testing.T, tr *corpus.Trace) []*readFrame {
	t.Helper()

	type packet struct {
		seq uint16
		pay []byte
	}

	var frames []*readFrame
	index := map[uint32]*readFrame{}
	packets := map[uint32][]packet{}
	for _, r := range tr.Records {
		if r.T == corpus.RecordEOF {
			require.NotContains(t, []string{"truncated", "dropped"}, r.Reason, "the trace misses reads")
		}
		if r.T != corpus.RecordRTP || r.Pad {
			continue
		}
		require.Len(t, r.Pay, r.PLen, "the trace must hold whole payloads")

		f := index[r.TS]
		if f == nil {
			f = &readFrame{TS: r.TS, Read: r.AtUs}
			index[r.TS] = f
			frames = append(frames, f)
		}
		f.Read = min(f.Read, r.AtUs)

		// Retransmitted and repeated reads carry a packet already read.
		if !slices.ContainsFunc(packets[r.TS], func(p packet) bool { return p.seq == r.Seq }) {
			packets[r.TS] = append(packets[r.TS], packet{seq: r.Seq, pay: r.Pay})
		}
	}

	for _, f := range frames {
		ps := packets[f.TS]
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
	// The read loop stamps a packet's arrival for the recorder after the
	// trace takes its read time. Preemption on a loaded runner, under -race
	// too, can put a few ms between the two.
	readToArrival = 5 * time.Millisecond
)

type capturePlacement struct {
	Late    time.Duration // how late the first block plays after its frame's capture
	Worst   time.Duration // the blocks' worst drift from Late
	Start   time.Time     // mediaStart
	Started events.RecordingMediaStarted
	Blocks  []pairedBlock
	Clock   senderClock
	Out     *Output
}

func (p capturePlacement) placed(b pairedBlock) time.Time {
	return p.Start.Add(time.Duration(b.PTS-p.Blocks[0].PTS) * time.Millisecond)
}

// placeOnCaptureClock places each block of rec, and finds the capture time
// of the frame it holds through the sender reports of the browser track
// sid. The stack must keep whole payloads in its traces (fullTracePayloads).
// mediaStart must be at most readToArrival after the first block's frame
// was read, and at most 1 s and 1 ms before it. The recorder moves it back
// up to 1 s, and the event drops its sub-ms part.
func placeOnCaptureClock(ctx context.Context, t *testing.T, s *Stack, name string, rec *Recording, sid string) capturePlacement {
	t.Helper()

	out := ReadOutput(t, rec, sid)
	clock := newSenderClock(out.Trace)
	require.NotEmpty(t, clock.reports, "%s: no sender report", name)

	// The recorder sends recordingMediaStarted before recordingStopped,
	// which has already arrived, so a short wait is enough.
	waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
	var started events.RecordingMediaStarted
	_, err := s.Driver.waitDecode(waitCtx, rec.From, events.RecordingMediaStartedKey, rec.SessionID, &started)
	waitCancel()
	require.NoError(t, err, "%s: recordingMediaStarted", name)

	p := capturePlacement{
		Start:   time.UnixMilli(started.MediaStartUTC),
		Started: started,
		Blocks:  pairBlocks(t, out.WebMPath, readFrames(t, out.Trace), out.Trace.Header.ClockRate),
		Clock:   clock,
		Out:     out,
	}

	first := p.Blocks[0].Frame
	read := time.UnixMicro(out.Trace.Header.EpochUnixUs + first.Read)
	assert.WithinRange(t, p.Start, read.Add(-time.Second-time.Millisecond), read.Add(readToArrival),
		"%s: mediaStart against the first frame's read", name)

	captured := clock.at(first.TS)
	p.Late = p.Start.Sub(captured)
	for _, b := range p.Blocks {
		drift := p.placed(b).Sub(clock.at(b.Frame.TS)) - p.Late
		if drift.Abs() > p.Worst.Abs() {
			p.Worst = drift
		}
	}

	flowingAt := firstFlowing(s.Driver, rec.From, rec.SessionID)
	t.Logf("%s: %d blocks, %d sender reports; mediaStart plays the media %v after its capture, "+
		"drifting %v at worst; the first block was read %v after its capture; "+
		"a start stamped on flowing would play it %v after",
		name, len(p.Blocks), len(clock.reports), p.Late, p.Worst, read.Sub(captured), flowingAt.Sub(captured))

	return p
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
			mic := peer.PublishMic(MicOptions{})
			cam := peer.PublishBBBCameraCapturing("camera", c.profile, c.capture)

			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()

			if c.check != nil {
				c.check(ctx, t, s, room, cam)
			}

			recCam, err := s.Driver.StartRecording(ctx, room, cam.SID)
			require.NoError(t, err)
			recMic, err := s.Driver.StartRecording(ctx, room, mic.SID)
			require.NoError(t, err)

			for _, rec := range []*Recording{recCam, recMic} {
				_, err = s.Driver.WaitRtpStatus(ctx, rec.From, rec.SessionID, "flowing")
				require.NoError(t, err)
			}

			time.Sleep(recordFor)

			for _, rec := range []*Recording{recCam, recMic} {
				_, err = s.Driver.StopAndWait(ctx, rec.SessionID)
				require.NoError(t, err)
			}

			late := map[string]time.Duration{}
			for _, c := range []struct {
				name string
				rec  *Recording
				sid  string
			}{{"camera", recCam, cam.SID}, {"microphone", recMic, mic.SID}} {
				p := placeOnCaptureClock(ctx, t, s, c.name, c.rec, c.sid)
				late[c.name] = p.Late
				assert.LessOrEqual(t, p.Worst.Abs(), browserDriftBound, "%s: drift against the capture clock", c.name)
			}

			lipSync := late["camera"] - late["microphone"]
			t.Logf("lip sync: the camera plays %v later than the microphone", lipSync)
			assert.LessOrEqual(t, lipSync.Abs(), browserLipSyncBound, "lip sync")
		})
	}
}
