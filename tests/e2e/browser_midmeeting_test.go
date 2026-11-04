//go:build e2e && linux

package e2e

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// srSpread is how far apart the sender reports of c place the media: the
// spread of each report's NTP time less its RTP time.
func srSpread(c senderClock) time.Duration {
	first := c.reports[0]
	var lo, hi time.Duration
	for i, r := range c.reports {
		off := ntpTime(r.NTP).Sub(ntpTime(first.NTP)) -
			time.Duration(int32(r.TS-first.TS))*time.Second/time.Duration(c.rate)
		if i == 0 || off < lo {
			lo = off
		}
		if i == 0 || off > hi {
			hi = off
		}
	}

	return hi - lo
}

// readGap returns the longest time between two media reads of tr, among
// the reads before until, and when it starts and ends.
func readGap(tr *corpus.Trace, until time.Time) (gap time.Duration, start, end time.Time) {
	var prev time.Time
	for _, r := range tr.Records {
		if r.T != corpus.RecordRTP || r.Pad {
			continue
		}
		at := traceTime(tr, r.AtUs)
		if !at.Before(until) {
			break
		}
		if !prev.IsZero() && at.Sub(prev) > gap {
			gap, start, end = at.Sub(prev), prev, at
		}
		prev = at
	}

	return gap, start, end
}

// TestBrowserMidMeetingFilesKeepLipSync records Chromium's microphone and
// camera through what BBB's client does to them in a meeting. Each new file
// starts while a file of the other kind runs. Two files that start
// together can share an error in how they start, which then cancels out.
//
// The page publishes both as BBB's client does (PublishBBBMic,
// PublishBBBCameraCapturing), and the test records each new track as
// bbb-webrtc-sfu does on track_published:
//   - A 3 s mute keeps the microphone's file. LiveKit sends 1 s of silence,
//     then nothing until the unmute.
//   - A mute held past 5 s makes the page unpublish the microphone. The
//     unmute publishes it again, as a new track and file.
//   - The camera is stopped and shared again after a preview, as a new
//     track and file.
//
// Every track of the browser shares its capture clock. So the four files
// must play their media equally late, within lip sync. Each block must
// play as late as its file's first.
func TestBrowserMidMeetingFilesKeepLipSync(t *testing.T) {
	RequireBrowser(t)

	const (
		// Gives each file a sender report before the next step.
		warmUp       = srSegment
		afterReshare = srSegment
		// The extra forwarding interval lets mic1 read a report built after
		// the unmute, before the long mute.
		afterShortMute = srSegment + lkReportInterval
		// Under BBB's 5 s unpublish timer, over LiveKit's 1 s of silence.
		shortMute = 3 * time.Second
		// Mute signalling on loopback takes ms; this ends inside the mute.
		muteSeen = 2 * time.Second
		// BBB's timer, 5 s, may fire late in a headless page.
		unpublishDelayMin = 5 * time.Second
		unpublishedBy     = 8 * time.Second
		// A user's pauses; they set how far apart the files start.
		unmuteDelay = time.Second
		micAlone    = 6 * time.Second
		cameraOff   = 2 * time.Second
		preview     = time.Second
		// cam1's first block can be a later key frame, after a key frame
		// request. cam2AfterMic2 needs none: record waits for mic2's media,
		// and cam2's first frame is captured after its preview, at the
		// publish.
		startSlack = time.Second
		// How far apart the schedule starts the files' first frames, at least.
		mic2AfterCam1 = warmUp + shortMute + afterShortMute + unpublishDelayMin + unmuteDelay - startSlack
		cam2AfterMic2 = micAlone + cameraOff + preview
	)

	s := NewStack(t, StackOptions{RecorderConfig: fullTracePayloads(t), LiveKitConfig: bbbLiveKitRoom})
	room := "e2e-browser-midmeeting-" + uuid.NewString()[:8]
	b := s.NewBrowser()
	peer := b.JoinAsBBB(room, "e2e-browser")

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	// The recorder subscribes at publish, so dynacast never pauses the
	// camera. cam1 has low-u30, the hidden 30 kbps profile. Chromium
	// sends its first frames late, so cam1's mediaStart must come from the
	// frames that caught up.
	mic1 := peer.PublishBBBMic("microphone")
	recMic1 := record(ctx, t, s, room, mic1.SID)
	cam1 := peer.PublishBBBCameraCapturing("camera", "low-u30", CameraCapture{})
	recCam1 := record(ctx, t, s, room, cam1.SID)

	time.Sleep(warmUp)

	muteAt := time.Now()
	peer.MuteMic()
	for {
		listed, err := s.LiveKit.listedTracks(ctx, room, "")
		require.NoError(t, err)
		ti := listed[mic1.SID]
		require.NotNil(t, ti, "mic1: unlisted in the short mute")
		if ti.Muted {
			break
		}
		require.True(t, time.Now().Before(muteAt.Add(muteSeen)), "mic1: LiveKit does not list it muted")
		time.Sleep(100 * time.Millisecond)
	}

	time.Sleep(time.Until(muteAt.Add(shortMute)))
	unmuted, republished := peer.UnmuteMic()
	unmuteDone := time.Now()
	require.False(t, republished, "mic1: the short mute's unmute published a new track")
	require.Equal(t, mic1.SID, unmuted.SID, "mic1: the short mute's unmute")
	listed, err := s.LiveKit.listedTracks(ctx, room, "")
	require.NoError(t, err)
	require.NotNil(t, listed[mic1.SID], "mic1: unlisted after the short mute")

	time.Sleep(afterShortMute)

	longMuteAt := time.Now()
	peer.MuteMic()
	unlistCtx, unlistCancel := context.WithDeadline(ctx, longMuteAt.Add(unpublishedBy))
	err = waitTracksUnlisted(unlistCtx, s.LiveKit, room, peer.Identity, mic1.SID)
	unlistCancel()
	require.NoError(t, err, "mic1: BBB's timer did not unpublish it")
	stopMic1 := unrecord(ctx, t, s, "mic1", recMic1)

	time.Sleep(unmuteDelay)
	mic2, republished := peer.UnmuteMic()
	require.True(t, republished, "mic2: the unmute after the unpublish did not publish a track")
	require.NotEqual(t, mic1.SID, mic2.SID, "mic2: the republished microphone's SID")
	recMic2 := record(ctx, t, s, room, mic2.SID)

	time.Sleep(micAlone)
	peer.UnpublishCamera(cam1.SID)
	stopCam1 := unrecord(ctx, t, s, "cam1", recCam1)

	time.Sleep(cameraOff)
	cam2 := peer.PublishBBBCameraCapturing("camera", "low", CameraCapture{Preview: preview})
	require.NotEqual(t, cam1.SID, cam2.SID, "cam2: the re-shared camera's SID")
	recCam2 := record(ctx, t, s, room, cam2.SID)

	time.Sleep(afterReshare)
	stopMic2, err := s.Driver.StopAndWait(ctx, recMic2.SessionID)
	require.NoError(t, err)
	stopCam2, err := s.Driver.StopAndWait(ctx, recCam2.SessionID)
	require.NoError(t, err)

	var files []capturePlacement
	for _, f := range []struct {
		name string
		rec  *Recording
		sid  string
		stop *events.RecordingStopped
	}{
		{"cam1", recCam1, cam1.SID, stopCam1},
		{"mic1", recMic1, mic1.SID, stopMic1},
		{"mic2", recMic2, mic2.SID, stopMic2},
		{"cam2", recCam2, cam2.SID, stopCam2},
	} {
		p := placeOnCaptureClock(ctx, t, s, f.name, f.rec, f.sid)

		assertMediaEnd(t, f.name, p.Started, f.stop, p.Out.WebM.PTS, maxStepOf(p.kind()))

		t.Logf("%s: the first block was read %v after mediaStart; the sender reports spread %v",
			f.name, p.Read.Sub(p.Start), srSpread(p.Clock))

		files = append(files, p)
	}

	assertFilesAgree(t, files, browserLipSyncBound)

	first := func(f capturePlacement) time.Time { return f.captured(0) }
	last := func(f capturePlacement) time.Time { return f.captured(len(f.Blocks) - 1) }
	cam1File, mic2File, cam2File := files[0], files[2], files[3]
	t.Logf("first frames captured: mic2 %v after cam1 (at least %v), cam2 %v after mic2 (at least %v)",
		first(mic2File).Sub(first(cam1File)), mic2AfterCam1, first(cam2File).Sub(first(mic2File)), cam2AfterMic2)
	assert.GreaterOrEqual(t, first(mic2File).Sub(first(cam1File)), mic2AfterCam1, "mic2 starts after cam1")
	assert.GreaterOrEqual(t, first(cam2File).Sub(first(mic2File)), cam2AfterMic2, "cam2 starts after mic2")
	assert.True(t, first(mic2File).Before(last(cam1File)), "mic2 starts while cam1 runs")
	assert.True(t, first(cam2File).Before(last(mic2File)), "cam2 starts while mic2 runs")

	// LiveKit's mute path shows in mic1's reads: it forwards until the mute
	// reaches it, sends its silence, then nothing until the unmute. The long
	// mute leaves a longer gap later in the file.
	mic1Trace := files[1].Out.Trace
	gap, gapStart, gapEnd := readGap(mic1Trace, longMuteAt)
	silence := 0
	for _, r := range mic1Trace.Records {
		at := traceTime(mic1Trace, r.AtUs)
		if r.T == corpus.RecordRTP && !r.Pad && !at.Before(muteAt) && !at.After(gapStart) && isOpusSilence(r.Pay) {
			silence++
		}
	}
	t.Logf("mic1: the short mute's read gap is %v, from %v to %v after the mute; %d reads of LiveKit's silence before it",
		gap, gapStart.Sub(muteAt), gapEnd.Sub(muteAt), silence)
	require.Greater(t, gap, time.Second, "mic1: the short mute left no read gap in the file")

	// The drift check covers the frames after the unmute only through a
	// sender report built after it. Until Chromium's next report, LiveKit
	// keeps extrapolating the reports from before the mute.
	assert.True(t, slices.ContainsFunc(mic1Trace.Records, func(r corpus.Record) bool {
		at := traceTime(mic1Trace, r.AtUs)
		return r.T == corpus.RecordSR && !at.Before(unmuteDone.Add(lkReportAge)) && at.Before(longMuteAt)
	}), "mic1: no sender report %v after the unmute", lkReportAge)
}
