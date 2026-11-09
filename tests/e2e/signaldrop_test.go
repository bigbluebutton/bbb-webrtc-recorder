//go:build e2e && linux

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The signal-drop scenarios cut the recorder's signalling connection to
// livekit-server and pin what server-sdk-go v2.18.1 then does. It makes a
// single resume attempt: a cut whose redial is accepted resumes the session in
// place, and a refused redial makes the SDK fall back to a full reconnect that
// closes the peer connections, so each session stops as "closed" and the SFU's
// retry starts a second file. That split is today's behaviour, not a target.

const (
	metricReconnecting = "recorder_participant_reconnecting_events_total"
	metricReconnected  = "recorder_participant_reconnects_total"

	// bbb-webrtc-sfu's RECORDING_RETRY_DELAY: after an abrupt stop it starts
	// the same session again, with a new file name, this long after.
	sfuRetryDelay = 3 * time.Second

	signalCameraFPS = 15
	signalOpusFPS   = 50
	// As in the baseline: the subscription, and on video LiveKit's PLI and the
	// publisher's key frame, before a session's first frame.
	signalStartupAllowance = time.Second
	// Media recorded before the cut, and after the session recovers.
	signalBeforeCut = 6 * time.Second
	signalAfterCut  = 6 * time.Second
)

// TestSignalCutResumesInPlace resets the signalling connections and lets the
// redial through at once: the SDK's single resume attempt succeeds, so each
// session keeps its peer connection and its one file. Every frame must play
// where it was sent, on both sides of the resume.
func TestSignalCutResumesInPlace(t *testing.T) {
	const (
		// The SDK redials at once, and the server restarts ICE on the
		// subscriber PC as it resumes the session.
		resumeWithin = 3 * time.Second
		// Frames not written around the cut: whatever the ICE restart holds
		// up, plus slack for a slow runner.
		resumeAllowance = 1500 * time.Millisecond
	)

	r := newSignalRig(t)
	ctx := r.ctx

	time.Sleep(time.Until(r.started.Add(signalBeforeCut)))

	base, err := ScrapeMetrics(ctx, r.s.Recorder.MetricsURL)
	require.NoError(t, err)

	accepted := r.proxy.Accepted()
	cutAt := time.Now()
	require.Equal(t, len(r.tracks), r.proxy.Cut(), "one signalling connection per session")

	require.NoError(t, WaitFor(resumeWithin, nil, func() error {
		m, err := ScrapeMetrics(ctx, r.s.Recorder.MetricsURL)
		if err != nil {
			return err
		}

		if n := int(m.Delta(base).Sum(metricReconnected, nil)); n < len(r.tracks) {
			return fmt.Errorf("%d of %d sessions reconnected", n, len(r.tracks))
		}

		return nil
	}), "the SDK did not resume")
	t.Logf("resumed %s after the cut; proxy accepted %d new connections",
		time.Since(cutAt), r.proxy.Accepted()-accepted)

	time.Sleep(signalAfterCut)

	for _, tr := range r.tracks {
		for _, m := range r.s.Driver.Messages() {
			assert.False(t, stoppedMatch(tr.rec.SessionID)(m), "%s stopped during the outage: %s", tr.name, m.Raw)
		}
	}

	stopSent, stopped := r.stopAll()

	after, err := ScrapeMetrics(ctx, r.s.Recorder.MetricsURL)
	require.NoError(t, err)
	delta := after.Delta(base)

	// Pinned: one OnReconnecting and one OnReconnected per session.
	assert.Equal(t, len(r.tracks), int(delta.Sum(metricReconnecting, nil)), metricReconnecting)
	assert.Equal(t, len(r.tracks), int(delta.Sum(metricReconnected, nil)), metricReconnected)
	assert.GreaterOrEqual(t, r.proxy.Accepted()-accepted, len(r.tracks), "resume dials through the proxy")

	places := map[string]placement{}

	for _, tr := range r.tracks {
		t.Run(tr.name, func(t *testing.T) {
			assert.Equal(t, events.StopReasonNormal, stopped[tr.rec.SessionID].Reason)
			assert.Len(t, sessionFiles(t, r.s, r.room, tr), 1, "files for the session")

			out := ReadOutput(t, tr.rec, tr.track.SID)
			webm := out.WebM
			sinceCut := stopSent.Sub(cutAt)
			after := blocksInLast(webm, sinceCut)

			t.Logf("webm: blocks=%d maxGap=%dms after-cut=%d; trace: frames=%d rtx=%d plis=%v",
				webm.Blocks, webm.MaxGapMs, after, out.ReadFrames, out.RTXPackets, out.PLIs)

			assert.NoError(t, webm.CheckMonotonic())
			assert.NoError(t, webm.CheckNoDuplicatePTS())
			assert.GreaterOrEqual(t, webm.Blocks,
				int(tr.fps*(stopSent.Sub(tr.started)-signalStartupAllowance-resumeAllowance).Seconds()), "frames written")
			assert.GreaterOrEqual(t, after, int(tr.fps*(sinceCut-resumeAllowance).Seconds()),
				"frames written after the cut")
			assert.LessOrEqual(t, webm.MaxGapMs, resumeAllowance.Milliseconds(), "PTS gap at the resume")

			sends := frameSends(tr.track)
			p, _ := checkPlacement(ctx, t, r.s.Driver, tr.name, tr.rec, sends, stopped[tr.rec.SessionID], maxStepOf(tr.track.Kind))
			places[tr.name] = p
			logResume(t, tr.name, p, sends, cutAt, resumeWithin)
			t.Logf("%s: wakeup lag %v at most; ICE pairs, server side: %v", tr.name, maxWakeLag(tr.track),
				iceServerAddrs(t, r.s.LiveKit.Proc.LogPath, "bbb-webrtc-recorder-"+tr.rec.SessionID, "SUBSCRIBER"))
		})
	}

	assertLipSync(t, "lip sync", places["camera"], places["microphone"])
}

// logResume logs what the resume cost a track. It logs where the frames sent
// after the cut play, at worst. Of the frames sent within window of the cut,
// it logs how many are not in the file, and the largest PTS step into one.
func logResume(t *testing.T, name string, p placement, sends map[int]time.Time, cutAt time.Time,
	window time.Duration,
) {
	t.Helper()

	inWindow := func(at time.Time) bool { return !at.Before(cutAt) && at.Before(cutAt.Add(window)) }

	var worst, step time.Duration
	written := map[int]bool{}

	for i, b := range p.Blocks {
		written[b.Frame] = true
		sent := sends[b.Frame]

		if !sent.Before(cutAt) && i < len(p.Errs) && p.Errs[i].Abs() > worst.Abs() {
			worst = p.Errs[i]
		}

		if i > 0 && inWindow(sent) {
			step = max(step, time.Duration(b.PTS-p.Blocks[i-1].PTS)*time.Millisecond)
		}
	}

	sent, missing := 0, 0
	for frame, at := range sends {
		if inWindow(at) {
			sent++
			if !written[frame] {
				missing++
			}
		}
	}

	t.Logf("%s: after the cut, frames play %v from where they were sent at worst; in the %v after it, "+
		"the largest PTS step is %v and %d of %d frames sent are not in the file", name, worst, window, step, missing, sent)
}

// TestSignalRefusedSplitsFile resets the signalling connections and refuses
// new ones for refuseFor. The SDK's one resume attempt fails on the refused
// dial and it falls back to a full reconnect, which closes the peer
// connections: every track reads EOF and its session stops as "closed",
// whatever the outage's length. The test then retries as the SFU does, so
// each session ends with two files, the second written once the signal is
// back.
//
// Each file must play its frames where they were sent, and the second must
// hold only frames sent after the refusal.
func TestSignalRefusedSplitsFile(t *testing.T) {
	const (
		// Longer than sfuRetryDelay, so the first retry meets the refusal.
		refuseFor = 5 * time.Second
		// The SDK escalates within milliseconds of a refused dial (the tracks
		// read EOF about 2 ms after it); the rest is the recorder's stop.
		stopWithin = 2 * time.Second
		maxRetries = 10
	)

	r := newSignalRig(t)
	ctx := r.ctx

	time.Sleep(time.Until(r.started.Add(signalBeforeCut)))

	base, err := ScrapeMetrics(ctx, r.s.Recorder.MetricsURL)
	require.NoError(t, err)

	cutAt := time.Now()
	n, listening := r.proxy.Refuse(refuseFor)
	require.Equal(t, len(r.tracks), n, "one signalling connection per session")

	type retryResult struct {
		closed    events.RecordingStopped
		closedAt  int // the closed recordingStopped's message index
		stoppedIn time.Duration
		failures  []error
		restarted time.Time
		from      int
		err       error
	}

	results := make([]retryResult, len(r.tracks))

	var wg sync.WaitGroup

	for i, tr := range r.tracks {
		wg.Add(1)

		go func() {
			defer wg.Done()

			res := &results[i]

			m, at, err := r.s.Driver.WaitMessage(ctx, 0, stoppedMatch(tr.rec.SessionID))
			if err != nil {
				res.err = fmt.Errorf("%s: no recordingStopped: %w", tr.name, err)
				return
			}

			res.stoppedIn, res.closedAt = m.At.Sub(cutAt), at

			if err := json.Unmarshal(m.Raw, &res.closed); err != nil || res.closed.Reason != "closed" {
				res.err = fmt.Errorf("%s: recordingStopped reason %q (%v), want closed", tr.name, res.closed.Reason, err)
				return
			}

			// bbb-webrtc-sfu's _scheduleRecordingRetry: the same session, a new
			// file name, every sfuRetryDelay while the start fails.
			for attempt := 1; attempt <= maxRetries; attempt++ {
				time.Sleep(sfuRetryDelay)

				res.from = len(r.s.Driver.Messages())
				rec, err := r.s.Driver.StartRecordingWith(ctx, StartSpec{
					Room:      r.room,
					TrackIDs:  []string{tr.track.SID},
					SessionID: tr.rec.SessionID,
					FileName:  fmt.Sprintf("recordings/%s/%s-%s-retry%d.webm", r.room, tr.track.SID, tr.rec.SessionID, attempt),
				})
				if err == nil {
					tr.retry = rec
					res.restarted = time.Now()

					return
				}

				res.failures = append(res.failures, err)
			}

			res.err = fmt.Errorf("%s: %d retries failed: %v", tr.name, maxRetries, res.failures)
		}()
	}

	wg.Wait()

	require.NoError(t, <-listening)

	for i, tr := range r.tracks {
		res := results[i]
		require.NoError(t, res.err)

		_, _, err := r.s.Driver.WaitMessage(ctx, res.from, flowingMatch(tr.rec.SessionID))
		require.NoError(t, err, "%s: retried session not flowing", tr.name)

		t.Logf("%s: stopped %s after the cut, %d failed retries (%v), restarted %s after the cut",
			tr.name, res.stoppedIn, len(res.failures), res.failures, res.restarted.Sub(cutAt))

		// Pinned: the refused redial ends the session at once.
		assert.LessOrEqual(t, res.stoppedIn, stopWithin, "%s: recordingStopped after the cut", tr.name)
		// No retry can join while the signal is refused.
		assert.NotEmpty(t, res.failures, "%s: a retry during the outage", tr.name)
		assert.GreaterOrEqual(t, res.restarted.Sub(cutAt), refuseFor, "%s: restarted during the outage", tr.name)

		tr.restarted = res.restarted
	}

	time.Sleep(time.Until(lastRestart(r.tracks).Add(signalAfterCut)))

	stopSent, stopped := r.stopAll()

	after, err := ScrapeMetrics(ctx, r.s.Recorder.MetricsURL)
	require.NoError(t, err)
	delta := after.Delta(base)

	// Pinned: the fallback fires OnReconnecting once per session, and the
	// session closes the room before the SDK's restart can get through.
	assert.Equal(t, len(r.tracks), int(delta.Sum(metricReconnecting, nil)), metricReconnecting)
	assert.Zero(t, int(delta.Sum(metricReconnected, nil)), metricReconnected)

	firsts, seconds := map[string]placement{}, map[string]placement{}

	for i, tr := range r.tracks {
		t.Run(tr.name, func(t *testing.T) {
			assert.Equal(t, events.StopReasonNormal, stopped[tr.rec.SessionID].Reason)
			// Pinned: the split. One file up to the cut, one from the retry on.
			assert.Len(t, sessionFiles(t, r.s, r.room, tr), 2, "files for the session")

			first := ReadOutput(t, tr.rec, tr.track.SID)
			second := ReadOutput(t, tr.retry, tr.track.SID)

			t.Logf("first: blocks=%d maxGap=%dms eof=%v; second: blocks=%d maxGap=%dms plis=%v",
				first.WebM.Blocks, first.WebM.MaxGapMs, first.EOF != nil,
				second.WebM.Blocks, second.WebM.MaxGapMs, second.PLIs)

			for _, out := range []*Output{first, second} {
				assert.NoError(t, out.WebM.CheckMonotonic(), out.WebMPath)
				assert.NoError(t, out.WebM.CheckNoDuplicatePTS(), out.WebMPath)
				assert.NotNil(t, out.EOF, "%s: trace eof", out.TracePath)
			}

			assert.GreaterOrEqual(t, first.WebM.Blocks,
				int(tr.fps*(cutAt.Sub(tr.started)-signalStartupAllowance).Seconds()), "frames before the cut")
			assert.GreaterOrEqual(t, second.WebM.Blocks,
				int(tr.fps*(stopSent.Sub(tr.restarted)-signalStartupAllowance).Seconds()), "frames after the outage")
			assert.LessOrEqual(t, second.WebM.MaxGapMs, int64(2*1000/tr.fps), "PTS gap after the outage")

			sends := frameSends(tr.track)
			p1, ok := checkPlacement(ctx, t, r.s.Driver, tr.name+", first file", tr.rec, sends, &results[i].closed, maxStepOf(tr.track.Kind))
			// recordingMediaStarted names no file. Without this check, a start
			// missing for the first file would pass as the second file's.
			if ok {
				require.Less(t, p1.StartedAt, results[i].closedAt,
					"%s: the first file's recordingMediaStarted came after its recordingStopped", tr.name)
			}

			p2, _ := checkPlacement(ctx, t, r.s.Driver, tr.name+", second file", tr.retry, sends,
				stopped[tr.rec.SessionID], maxStepOf(tr.track.Kind))
			firsts[tr.name], seconds[tr.name] = p1, p2

			_, last1 := tagRange(p1.Blocks)
			first2, _ := tagRange(p2.Blocks)
			assert.Greater(t, first2, last1, "%s: the second file holds frames of the first", tr.name)
			assert.GreaterOrEqual(t, sends[p2.Blocks[0].Frame].Sub(cutAt), refuseFor,
				"%s: the second file's first frame was sent during the refusal", tr.name)
			t.Logf("%s: the first file's last frame was sent %v before the cut; wakeup lag %v at most",
				tr.name, cutAt.Sub(sends[last1]), maxWakeLag(tr.track))
		})
	}

	assertLipSync(t, "lip sync, first files", firsts["camera"], firsts["microphone"])
	assertLipSync(t, "lip sync, second files", seconds["camera"], seconds["microphone"])
}

func tagRange(blocks []taggedBlock) (int, int) {
	lo, hi := blocks[0].Frame, blocks[0].Frame
	for _, b := range blocks[1:] {
		lo, hi = min(lo, b.Frame), max(hi, b.Frame)
	}

	return lo, hi
}

// signalRig is a stack whose recorder signals through a proxy, recording one
// camera and one microphone session that are both flowing.
type signalRig struct {
	ctx     context.Context
	s       *Stack
	proxy   *TCPProxy
	room    string
	tracks  []*signalTrack
	started time.Time
}

type signalTrack struct {
	name    string
	fps     float64
	track   *PubTrack
	rec     *Recording
	started time.Time

	// The SFU-style retry after an abrupt stop, in the split case.
	retry     *Recording
	restarted time.Time
}

func newSignalRig(t *testing.T) *signalRig {
	t.Helper()

	r := &signalRig{room: "e2e-signal-" + uuid.NewString()[:8]}

	r.s = NewStack(t, StackOptions{
		// The selected ICE pairs, which TestSignalCutResumesInPlace logs, are
		// logged at debug.
		LiveKitConfig: func(cfg map[string]any) {
			lookup[map[string]any](t, cfg, "logging")["level"] = "debug"
		},
		RecorderConfig: func(s *Stack, cfg map[string]any) {
			r.proxy = StartTCPProxy(t, "127.0.0.1", fmt.Sprintf("127.0.0.1:%d", s.LiveKit.HTTPPort),
				filepath.Join(s.Artifacts, "signal-proxy.log"))
			lookup[map[string]any](t, cfg, "livekit")["host"] = r.proxy.URL()
		},
	})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	r.ctx = ctx

	pub := r.s.NewPublisher(r.room, "e2e-publisher")

	// Longer than any signal-drop run, so the schedule does not loop.
	video, err := corpus.GenerateVP8(corpus.VP8Options{FPS: signalCameraFPS, Duration: time.Minute, SpreadUs: 2000})
	require.NoError(t, err)
	audio, err := corpus.GenerateOpus(corpus.OpusOptions{Duration: time.Minute})
	require.NoError(t, err)

	cam := pub.PublishVP8(TrackOptions{Name: "camera", Schedule: video, TagFrames: true})
	mic := pub.PublishOpus(TrackOptions{Name: "microphone", Schedule: audio, TagFrames: true})
	require.NoError(t, pub.WaitPublished(ctx, cam, mic))

	r.tracks = []*signalTrack{
		{name: "camera", fps: signalCameraFPS, track: cam},
		{name: "microphone", fps: signalOpusFPS, track: mic},
	}

	for _, tr := range r.tracks {
		tr.rec, err = r.s.Driver.StartRecording(ctx, r.room, tr.track.SID)
		require.NoError(t, err)
		tr.started = time.Now()
	}

	r.started = time.Now()

	for _, tr := range r.tracks {
		_, err := r.s.Driver.WaitRtpStatus(ctx, tr.rec.From, tr.rec.SessionID, "flowing")
		require.NoError(t, err)
	}

	t.Logf("both flowing %s after start", time.Since(r.started))

	return r
}

// stopAll stops every session and waits for the recordingStopped that
// follows the stop, returning when the stops were sent.
func (r *signalRig) stopAll() (time.Time, map[string]*events.RecordingStopped) {
	t := r.s.T
	t.Helper()

	from := len(r.s.Driver.Messages())
	stopSent := time.Now()

	for _, tr := range r.tracks {
		require.NoError(t, r.s.Driver.Stop(tr.rec.SessionID))
	}

	stopped := map[string]*events.RecordingStopped{}

	for _, tr := range r.tracks {
		m, _, err := r.s.Driver.WaitMessage(r.ctx, from, stoppedMatch(tr.rec.SessionID))
		require.NoError(t, err, "%s: no recordingStopped after stopRecording", tr.name)

		var st events.RecordingStopped
		require.NoError(t, json.Unmarshal(m.Raw, &st))
		stopped[tr.rec.SessionID] = &st
	}

	return stopSent, stopped
}

func stoppedMatch(sessionID string) func(Message) bool {
	return func(m Message) bool {
		return m.ID == events.RecordingStoppedKey && m.SessionID == sessionID
	}
}

func flowingMatch(sessionID string) func(Message) bool {
	return func(m Message) bool {
		if m.ID != events.RecordingRtpStatusChangedKey || m.SessionID != sessionID {
			return false
		}

		var e events.RecordingRtpStatusChanged

		return json.Unmarshal(m.Raw, &e) == nil && e.Status == "flowing"
	}
}

func lastRestart(tracks []*signalTrack) time.Time {
	var last time.Time

	for _, tr := range tracks {
		if tr.restarted.After(last) {
			last = tr.restarted
		}
	}

	return last
}

// sessionFiles lists the WebM files the recorder wrote for tr's session.
func sessionFiles(t *testing.T, s *Stack, room string, tr *signalTrack) []string {
	t.Helper()

	files, err := filepath.Glob(filepath.Join(s.Recorder.Dir, "recordings", room,
		fmt.Sprintf("%s-%s*.webm", tr.track.SID, tr.rec.SessionID)))
	require.NoError(t, err)

	return files
}

// blocksInLast counts the blocks whose PTS lies within span of the last
// one: what the file gained over the span that ended with its last write.
func blocksInLast(webm *oracle.Track, span time.Duration) int {
	from := webm.LastPTS - span.Milliseconds()
	n := 0

	for _, pts := range webm.PTS {
		if pts >= from {
			n++
		}
	}

	return n
}
