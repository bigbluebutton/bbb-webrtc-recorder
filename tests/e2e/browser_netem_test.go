//go:build e2e && linux

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// In the browser netem scenario, Chromium has a network namespace of its
// own. Its one link goes to the servers' namespace, and netem shapes both
// ends of it. No other link is shaped.

const (
	brwNetemIdentity = "e2e-browser"

	// The impaired start's file: a segment and a margin. It also gives
	// thousands of packets each way, so netem's 3% loss drops some.
	brwNetemImpairedRecord = 15 * time.Second

	// not_flowing comes PacketReadTimeout after the read loop sets its
	// deadline. This allows for the loop's work before that, the flow
	// dispatch and Redis, under -race too.
	brwNetemNotFlowingSlack = 100 * time.Millisecond
	// Chromium sends a microphone packet every 20 ms, and can send one late.
	// So its last packet comes within 50 ms before a blackout. LiveKit's
	// timers count from that packet.
	brwNetemLastPacket = 50 * time.Millisecond
	// LiveKit's RED receiver recovers up to two earlier Opus frames from the
	// first packet after a gap. One more frame allows for the drop window's
	// edges.
	brwNetemMicCutSlack = 3 * 20 * time.Millisecond
	// On loopback, the microphone's PTS steps one 20 ms Opus frame across a
	// blackout that changes nothing. Its PTS may drift from capture by
	// browserDriftBound.
	brwNetemNoCutMicStep = 20*time.Millisecond + browserDriftBound
	// Blocks after a blackout span this much media at least, so the segment
	// after it is checked.
	brwNetemPostCut = 10 * time.Second

	// livekit-server closes a participant 20 s after its last packet: ICE
	// fails at 15 s, then disconnectCleanupDuration (5 s) runs. Each PC's
	// failure restarts that timer, so the close follows the publisher PC's,
	// which fails last.
	brwNetemParticipantTimeout = 20*time.Second - brwNetemLastPacket
	// From the old tracks' unlisting to their recordingStopped: the
	// follower's poll, Close's 2 s wait for the track readers, and margin.
	brwNetemStopWithin = 5 * time.Second
	// From the blackout's end until LiveKit lists both new tracks. An
	// attempt stuck at the blackout's end waits for livekit-client's 15 s
	// websocket timeout. The next retry waits up to 8 s, and joining and
	// republishing take about 3 s.
	brwNetemReconnectWithin = 30 * time.Second

	// On loopback, a microphone frame is read at most 3 ms after its
	// capture. It can seem to be read up to about 15 ms before it (see
	// browserReportError). So the median is at most 3 ms, and under
	// netem's 40 ms delay at least 25 ms.
	//
	// The first second's 50-frame median has a standard error of about
	// 3 ms, so 15 ms is over three of them under 25 ms.
	// The spread holds for a uniform or normal delay.
	brwNetemMinDelay    = 15 * time.Millisecond
	brwNetemDelaySpread = 20 * time.Millisecond

	// pion/ice in livekit-server 1.13.7 checks every 2 s, goes disconnected
	// after 10 s with no packet and failed after 15 s.
	brwNetemICEDisconnected = 10 * time.Second
)

type browserNetemCase struct {
	name    string
	base    []string // netem's arguments on the browser's link, from setup
	profile string   // the camera's
	// blackout, if set, drops everything on the browser's link for this
	// long, srSegment after both tracks flow.
	blackout time.Duration
	// reconnect: the blackout outlasts LiveKit's participant timeout, so
	// livekit-client reconnects fully, with new tracks.
	reconnect bool
	// resumeWithin bounds the microphone's flowing again after a blackout it
	// survives, from the blackout's end.
	resumeWithin time.Duration
	// iceDisconnects: the blackout outlasts livekit-server's ICE
	// disconnected timeout, not its failed one.
	iceDisconnects bool
}

var browserNetemCases = []browserNetemCase{
	{
		// 3% is the NACK scenario's loss. The delay takes the kernel's default
		// distribution; the bounds hold for uniform and normal. low sends one
		// layer, so a misplaced block can only be the recorder's.
		name: "loss and jitter from publish", base: []string{"loss", "3%", "delay", "40ms", "20ms"}, profile: "low",
	},
	{
		// Shorter than ICE disconnected (10 s) and livekit-client's ping
		// timeout (over 10 s). LiveKit's connection quality still drops.
		// Much longer than PacketReadTimeout, so the microphone goes
		// not_flowing inside it.
		name: "3 s blackout", base: []string{"loss", "0%"}, profile: bbbDefaultCameraProfile,
		blackout: 3 * time.Second, resumeWithin: 5 * time.Second,
	},
	{
		// The publisher PC's last packet comes at the blackout. Its pion/ice
		// reports disconnected at 10-12 s and failed at 15-17 s: 1.5 s of
		// margin each way. A signal resume may follow livekit-client's 15 s
		// websocket timeout and an ICE restart.
		name: "blackout past ICE disconnected", base: []string{"loss", "0%"}, profile: bbbDefaultCameraProfile,
		blackout: 13500 * time.Millisecond, resumeWithin: 30 * time.Second, iceDisconnects: true,
	},
	{
		// LiveKit closes the participant at 20-22 s, before the link returns,
		// so the resume gets a leave and livekit-client reconnects fully.
		name: "blackout past the participant timeout", base: []string{"loss", "0%"}, profile: "low",
		blackout: 25 * time.Second, reconnect: true,
	},
}

// TestBrowserNetemKeepsPlacement records Chromium's microphone and camera,
// published as BBB's client does, while the browser's network is bad:
//   - loss and jitter from before publish, so the start is impaired too;
//   - a 3 s blackout mid-file;
//   - a 13.5 s blackout, past livekit-server's ICE disconnected, which
//     LiveKit resumes with the same tracks;
//   - a 25 s blackout, past LiveKit's participant timeout. livekit-client
//     reconnects with new tracks, which the test records as bbb-webrtc-sfu
//     does, in new files.
//
// record-and-playback plays each file from its mediaStart, and never
// stretches its media. So every file must play its media equally late,
// within lip sync, and each block as late as its file's first. Across a
// blackout, the PTS must step as far as the capture clock does.
//
// It needs root, E2E_NETEM=1 and browser mode.
func TestBrowserNetemKeepsPlacement(t *testing.T) {
	requireNetem(t)
	RequireBrowser(t)

	for _, c := range browserNetemCases {
		t.Run(c.name, func(t *testing.T) {
			plan := newBrowserNetemPlan(os.Getpid(), c.base)
			setupNetem(t, plan)
			runBrowserNetemCase(t, plan, c)
		})
	}
}

// TestBrowserNetemControl runs the 3 s blackout case on loopback, with a
// blackout that changes nothing. The fault detectors must stay quiet, and
// the placement checks, the split at the blackout included, must pass.
// checkCut asserts the opposite of its read and capture-step checks here,
// so the control shows those two can tell a clean path from a blackout.
func TestBrowserNetemControl(t *testing.T) {
	RequireBrowser(t)

	i := slices.IndexFunc(browserNetemCases, func(c browserNetemCase) bool { return c.name == "3 s blackout" })
	require.NotEqual(t, -1, i, "no 3 s blackout case")

	plan := dryNetemPlan()
	plan.BrowserNetem = []string{"loss", "0%"}
	runBrowserNetemCase(t, plan, browserNetemCases[i])
}

type netemFile struct {
	name  string
	track followedTrack
	stop  *events.RecordingStopped
	// cutAfter is the blackout's end, if the file lasts through it.
	cutAfter time.Time
}

// netemBlackout is when the browser's link went dark and came back.
type netemBlackout struct {
	On, Off netemEdge
}

// dmin and dmax bound how long the browser's end dropped everything.
func (b netemBlackout) dmin() time.Duration { return b.Off.UpStart.Sub(b.On.UpEnd) }
func (b netemBlackout) dmax() time.Duration { return b.Off.UpEnd.Sub(b.On.UpStart) }

func runBrowserNetemCase(t *testing.T, plan netemPlan, c browserNetemCase) {
	s := newNetemStack(t, plan, StackOptions{RecorderConfig: fullTracePayloads(t), Browser: plan.browser(t)})
	room := "e2e-browser-netem-" + uuid.NewString()[:8]

	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()

	b := s.NewBrowser()
	peer := b.JoinAsBBB(room, brwNetemIdentity)
	mic := peer.PublishBBBMic("microphone")
	cam := peer.PublishBBBCamera("camera", c.profile)

	f := followTracks(t, s, room, brwNetemIdentity)
	micTrack := f.waitRecorded(ctx, t, "microphone", func(tr followedTrack) bool { return tr.SID == mic.SID })
	camTrack := f.waitRecorded(ctx, t, "camera", func(tr followedTrack) bool { return tr.SID == cam.SID })

	var flowingAt time.Time
	for _, tr := range []followedTrack{micTrack, camTrack} {
		_, err := s.Driver.WaitRtpStatus(ctx, tr.Rec.From, tr.Rec.SessionID, "flowing")
		require.NoError(t, err)
		flowingAt = maxTime(flowingAt, firstFlowing(s.Driver, tr.Rec.From, tr.Rec.SessionID))
	}

	checkICEPaths(t, s, plan, micTrack, camTrack)
	saveQdiscs(t, s, plan, "start")

	var bo *netemBlackout
	var resumedAt, micNotFlowing time.Time

	if c.blackout == 0 {
		time.Sleep(time.Until(flowingAt.Add(brwNetemImpairedRecord)))
	} else {
		time.Sleep(time.Until(flowingAt.Add(srSegment)))
		before := saveQdiscs(t, s, plan, "before-blackout")
		from := len(s.Driver.Messages())
		on := plan.blackout(t, true)
		time.Sleep(time.Until(on.UpStart.Add(c.blackout)))
		bo = &netemBlackout{On: on, Off: plan.blackout(t, false)}
		after := saveQdiscs(t, s, plan, "after-blackout")
		// The checks bound the blackout by the execs' start and end times.
		// So a slow exec widens their windows rather than failing them.
		t.Logf("blackout: %v to %v long; tc execs on %v, %v, off %v, %v", bo.dmin(), bo.dmax(),
			on.UpEnd.Sub(on.UpStart), on.DownEnd.Sub(on.DownStart), bo.Off.UpEnd.Sub(bo.Off.UpStart), bo.Off.DownEnd.Sub(bo.Off.DownStart))
		if !plan.Dry {
			for dev, n := range before {
				t.Logf("netem: %s dropped %d in the blackout", dev, after[dev]-n)
				assert.Positive(t, after[dev]-n, "netem: %s dropped nothing in the blackout", dev)
			}
		}

		if c.reconnect {
			resumedAt = waitReconnected(ctx, t, s, f, bo, micTrack, camTrack)
		} else {
			resumedAt = bo.Off.UpEnd
			if !plan.Dry {
				resumedAt, micNotFlowing = waitResumed(ctx, t, s, micTrack, camTrack, from, bo, c.resumeWithin)
			}
		}

		time.Sleep(time.Until(resumedAt.Add(srSegment)))
	}

	var senderStats map[string]SenderStat
	if c.blackout == 0 {
		senderStats = peer.SenderStats(cam.SID)
	}
	roomEvents := peer.RoomEvents()
	localTracks := peer.LocalTracks()
	end := saveQdiscs(t, s, plan, "end")

	// The follower stops before the driver does, so it sends no stop of its own.
	f.stop()
	stopFrom := len(s.Driver.Messages())
	tracks := f.snapshot()
	stops := map[string]*events.RecordingStopped{}
	for _, tr := range tracks {
		if tr.Rec == nil || !tr.VanishedAt.IsZero() {
			continue
		}
		st, err := s.Driver.StopAndWait(ctx, tr.Rec.SessionID)
		require.NoError(t, err, "%s: recordingStopped", tr.SID)
		stops[tr.SID] = st
	}

	for _, e := range roomEvents {
		t.Logf("page: %s %s %s %s %s at %v", e.Event, e.SID, e.Source, e.Quality, e.Reason, e.At.Sub(flowingAt))
	}
	for _, tr := range tracks {
		t.Logf("follower: %s %s listed %v, unlisted %v, stop sent %v", tr.Source, tr.SID,
			tr.ListedAt.Sub(flowingAt), sinceOrZero(tr.VanishedAt, flowingAt), sinceOrZero(tr.StopSentAt, flowingAt))
	}
	for _, e := range iceStates(t, s.LiveKit.Proc.LogPath, brwNetemIdentity) {
		t.Logf("livekit: %s ICE %s at %v", e.Transport, e.State, e.at().Sub(flowingAt))
	}
	assert.Empty(t, f.errors(), "the follower's calls")

	var recorded []followedTrack
	for _, tr := range tracks {
		if tr.Rec != nil {
			recorded = append(recorded, tr)
		}
	}
	checkICEPaths(t, s, plan, recorded...)

	var files []netemFile
	if c.reconnect {
		files = checkReconnect(ctx, t, s, bo, tracks, stops, roomEvents, localTracks, micTrack, camTrack)
	} else {
		files = checkSameTracks(t, s, plan, bo, tracks, stops, roomEvents, localTracks, micTrack, camTrack, stopFrom)
	}

	placed := map[string]capturePlacement{}
	var agree []capturePlacement
	pathDelay := netemLeastDelay(t, plan.BrowserNetem)
	for _, nf := range files {
		p := checkNetemFile(ctx, t, s, room, nf, c.profile, pathDelay)
		placed[nf.name] = p
		agree = append(agree, p)
	}
	assertFilesAgree(t, agree, browserLipSyncBound)

	switch {
	case c.blackout == 0 && !plan.Dry:
		for _, name := range []string{"microphone", "camera"} {
			loss := readTraceLoss(placed[name].Out.Trace)
			t.Logf("%s: the trace has %d sequence gaps, %d packets read later, %d gaps never repaired",
				name, len(loss.Gaps), loss.RepairedPackets(), len(loss.Unrepaired()))
		}
		checkImpairedStart(t, placed["microphone"], senderStats, end)
	case bo != nil && !c.reconnect:
		checkCut(t, s, plan, bo, micTrack, placed["microphone"], placed["camera"], stopFrom, micNotFlowing)
	case c.reconnect:
		checkOldFilesEnd(t, bo, placed["old microphone"], placed["old camera"])
	}

	if c.iceDisconnects && !plan.Dry {
		checkICEDisconnected(t, s, bo)
	}
}

// checkICEPaths checks that the browser's media takes the browser's link,
// and each recorder session's media the recorder's link. So the fault
// reaches only what the browser sends and receives.
func checkICEPaths(t *testing.T, s *Stack, plan netemPlan, tracks ...followedTrack) {
	t.Helper()

	if plan.Dry {
		return
	}

	brw := selectedICEAddrs(t, s.LiveKit.Proc.LogPath, brwNetemIdentity, "PUBLISHER")
	assert.Equal(t, plan.SrvBrwIP, brw[len(brw)-1], "the browser's ICE pair, server side (all: %v)", brw)

	for _, tr := range tracks {
		rec := selectedICEAddrs(t, s.LiveKit.Proc.LogPath, "bbb-webrtc-recorder-"+tr.Rec.SessionID, "SUBSCRIBER")
		assert.Equal(t, plan.SrvRecIP, rec[len(rec)-1], "the recorder's ICE pair for %s, server side (all: %v)", tr.SID, rec)
	}

	// Nothing but the browser's link has a netem qdisc.
	for _, c := range [][]string{
		{"tc", "-n", plan.ServerNS, "qdisc", "show", "dev", plan.SrvRecIf},
		{"tc", "-n", plan.ServerNS, "qdisc", "show", "dev", plan.SrvHostIf},
		{"tc", "-n", plan.RecorderNS, "qdisc", "show", "dev", plan.RecSrvIf},
		{"tc", "-n", plan.RecorderNS, "qdisc", "show", "dev", plan.RecHostIf},
		{"tc", "qdisc", "show", "dev", plan.HostSrvIf},
		{"tc", "qdisc", "show", "dev", plan.HostRecIf},
	} {
		out, err := exec.Command(c[0], c[1:]...).CombinedOutput()
		require.NoError(t, err, "%s: %s", strings.Join(c, " "), out)
		assert.NotContains(t, string(out), "netem", "%s", strings.Join(c, " "))
	}
}

// saveQdiscs saves both ends' qdisc statistics as artifacts named after
// stage, and returns their drop counts by device. A dry plan has none.
func saveQdiscs(t *testing.T, s *Stack, plan netemPlan, stage string) map[string]int {
	t.Helper()

	if plan.Dry {
		return nil
	}

	drops := map[string]int{}
	for _, end := range [][2]string{{plan.BrowserNS, plan.BrwSrvIf}, {plan.ServerNS, plan.SrvBrwIf}} {
		stats, n := readNetemStats(t, end[0], end[1], s.Artifacts, fmt.Sprintf("netem-qdisc-%s-%s.txt", stage, end[1]))
		drops[end[1]] = n
		if stage == "start" || stage == "end" {
			t.Logf("netem %s, %s: %s", stage, end[1], strings.Join(strings.Fields(stats), " "))
		}
	}

	return drops
}

// waitResumed waits until both tracks flow again after the blackout, and
// returns when the later did, and when the microphone reported not_flowing
// in the blackout.
func waitResumed(ctx context.Context, t *testing.T, s *Stack, mic, cam followedTrack, from int, bo *netemBlackout,
	within time.Duration,
) (resumed, micNotFlowing time.Time) {
	t.Helper()

	waitCtx, cancel := context.WithDeadline(ctx, bo.Off.UpEnd.Add(within))
	defer cancel()

	m, i, err := s.Driver.WaitMessage(waitCtx, from, rtpStatusMatch(mic.Rec.SessionID, "not_flowing"))
	require.NoError(t, err, "microphone: no not_flowing in the blackout")
	t.Logf("microphone: not_flowing %v after the link went dark", m.At.Sub(bo.On.UpStart))
	assert.True(t, m.At.After(bo.On.UpStart) && m.At.Before(bo.Off.UpStart), "microphone: not_flowing outside the blackout")
	micNotFlowing = m.At

	m, _, err = s.Driver.WaitMessage(waitCtx, i+1, rtpStatusMatch(mic.Rec.SessionID, "flowing"))
	require.NoError(t, err, "microphone: not flowing within %v of the blackout's end", within)
	t.Logf("microphone: flowing again %v after the link came back", m.At.Sub(bo.Off.UpEnd))
	resumed = m.At

	notFlowing := slices.IndexFunc(s.Driver.Messages()[from:], rtpStatusMatch(cam.Rec.SessionID, "not_flowing"))
	if notFlowing < 0 {
		t.Logf("camera: flowing through the blackout")
		return resumed, micNotFlowing
	}

	m, _, err = s.Driver.WaitMessage(waitCtx, from+notFlowing+1, rtpStatusMatch(cam.Rec.SessionID, "flowing"))
	require.NoError(t, err, "camera: not flowing within %v of the blackout's end", within)
	t.Logf("camera: flowing again %v after the link came back", m.At.Sub(bo.Off.UpEnd))

	return maxTime(resumed, m.At), micNotFlowing
}

// waitReconnected waits for livekit-client's new tracks after a full
// reconnect, and for both new sessions to flow, and returns when they did.
func waitReconnected(ctx context.Context, t *testing.T, s *Stack, f *trackFollower, bo *netemBlackout,
	oldMic, oldCam followedTrack,
) time.Time {
	t.Helper()

	waitCtx, cancel := context.WithDeadline(ctx, bo.Off.UpEnd.Add(brwNetemReconnectWithin))
	defer cancel()

	var flowing time.Time
	for _, old := range []followedTrack{oldMic, oldCam} {
		tr := f.waitRecorded(waitCtx, t, "new "+old.Source, func(tr followedTrack) bool {
			return tr.Source == old.Source && tr.SID != old.SID && tr.ListedAt.After(bo.On.UpStart)
		})
		_, err := s.Driver.WaitRtpStatus(ctx, tr.Rec.From, tr.Rec.SessionID, "flowing")
		require.NoError(t, err)
		flowing = maxTime(flowing, firstFlowing(s.Driver, tr.Rec.From, tr.Rec.SessionID))
	}
	t.Logf("reconnect: both new tracks flow %v after the link came back", flowing.Sub(bo.Off.UpEnd))

	return flowing
}

// checkSameTracks checks a case that keeps the original tracks: one file
// each, which the driver alone stops. It returns the files.
func checkSameTracks(t *testing.T, s *Stack, plan netemPlan, bo *netemBlackout, tracks []followedTrack,
	stops map[string]*events.RecordingStopped, roomEvents []RoomEvent, localTracks map[string]string,
	mic, cam followedTrack, stopFrom int,
) []netemFile {
	t.Helper()

	assert.Len(t, tracks, 2, "the follower saw a track besides the first two")
	assert.Equal(t, map[string]string{"microphone": mic.SID, "camera": cam.SID}, localTracks, "the page's tracks")

	for _, e := range roomEvents {
		switch e.Event {
		case "reconnecting", "disconnected", "localTrackUnpublished":
			assert.Fail(t, "a reconnect or an unpublish", "page: %s %s at %v", e.Event, e.SID, e.At)
		case "signalReconnecting":
			if bo == nil || plan.Dry || bo.dmin() < brwNetemICEDisconnected {
				assert.Fail(t, "a signal resume", "page: %s at %v", e.Event, e.At)
			}
		}
	}

	var cut time.Time
	if bo != nil {
		cut = bo.Off.UpStart
	}

	var files []netemFile
	for _, tr := range []followedTrack{mic, cam} {
		name := "microphone"
		if tr.SID == cam.SID {
			name = "camera"
		}

		assert.False(t, slices.ContainsFunc(s.Driver.Messages()[tr.Rec.From:stopFrom], stoppedMatch(tr.Rec.SessionID)),
			"%s: recordingStopped before the driver's stop", name)

		st := stops[tr.SID]
		require.NotNil(t, st, "%s: no recordingStopped", name)
		assert.Equal(t, events.StopReasonNormal, st.Reason, "%s: stop reason", name)

		files = append(files, netemFile{name: name, track: tr, stop: st, cutAfter: cut})
	}

	return files
}

// checkReconnect checks the full reconnect: the old tracks end when LiveKit
// closes the participant, not sooner, and new tracks follow. It returns
// the four files.
func checkReconnect(ctx context.Context, t *testing.T, s *Stack, bo *netemBlackout, tracks []followedTrack,
	stops map[string]*events.RecordingStopped, roomEvents []RoomEvent, localTracks map[string]string,
	oldMic, oldCam followedTrack,
) []netemFile {
	t.Helper()

	// livekit-client tries a resume first, and again after the link comes
	// back, so a full reconnect can be reported more than once before it
	// succeeds.
	var reconnects []string
	for _, e := range roomEvents {
		if e.Event == "reconnecting" || e.Event == "reconnected" || e.Event == "disconnected" {
			reconnects = append(reconnects, e.Event)
		}
	}
	assert.Contains(t, reconnects, "reconnecting", "the page's reconnect")
	assert.NotContains(t, reconnects, "disconnected", "the page's reconnect")
	if assert.NotEmpty(t, reconnects, "the page's reconnect") {
		assert.Equal(t, "reconnected", reconnects[len(reconnects)-1], "the page's reconnect: %v", reconnects)
	}

	closeFrom := bo.On.UpStart.Add(brwNetemParticipantTimeout)
	var closings []string
	closed := false
	for _, e := range participantClosings(t, s.LiveKit.Proc.LogPath, brwNetemIdentity) {
		closings = append(closings, fmt.Sprintf("%s at %v", e.Reason, e.at().Sub(bo.On.UpStart)))
		closed = closed || e.Reason == "PEER_CONNECTION_DISCONNECTED" && !e.at().Before(closeFrom) && e.at().Before(bo.Off.UpStart)
	}
	t.Logf("livekit: participant closings after the link went dark: %v", closings)
	assert.True(t, closed, "livekit did not close the browser's participant in the blackout, after %v: %v",
		brwNetemParticipantTimeout, closings)

	byOld := map[string]followedTrack{}
	var files []netemFile
	for _, tr := range tracks {
		if tr.SID == oldMic.SID || tr.SID == oldCam.SID {
			byOld[tr.SID] = tr
		}
	}

	for _, old := range []followedTrack{oldMic, oldCam} {
		name := "old " + strings.ToLower(old.Source)
		tr := byOld[old.SID]
		require.False(t, tr.VanishedAt.IsZero(), "%s: LiveKit still lists it", name)
		assert.WithinRange(t, tr.VanishedAt, closeFrom, bo.Off.UpStart, "%s: unlisted", name)

		m, _, err := s.Driver.WaitMessage(ctx, tr.Rec.From, stoppedMatch(tr.Rec.SessionID))
		require.NoError(t, err, "%s: recordingStopped", name)
		var st events.RecordingStopped
		require.NoError(t, json.Unmarshal(m.Raw, &st))

		first := "the recorder stopped it"
		if !tr.StopSentAt.IsZero() {
			first = fmt.Sprintf("the follower's stop came first, %v after the unlisting", tr.StopSentAt.Sub(tr.VanishedAt))
			assert.False(t, tr.StopSentAt.Before(closeFrom), "%s: the driver stopped it in the blackout", name)
		}
		t.Logf("%s: stopped (%s) %v after the link went dark; %s", name, st.Reason, m.At.Sub(bo.On.UpStart), first)
		assert.False(t, m.At.Before(closeFrom), "%s: recordingStopped %v after the link went dark, before LiveKit's timeout",
			name, m.At.Sub(bo.On.UpStart))
		assert.LessOrEqual(t, m.At.Sub(tr.VanishedAt), brwNetemStopWithin, "%s: recordingStopped after the unlisting", name)

		files = append(files, netemFile{name: name, track: tr, stop: &st})
	}

	var newSIDs []string
	for _, tr := range tracks {
		if _, old := byOld[tr.SID]; old {
			continue
		}

		name := "new microphone"
		if tr.Source == oldCam.Source {
			name = "new camera"
		}
		require.NotNil(t, tr.Rec, "%s: not recorded", name)
		st := stops[tr.SID]
		require.NotNil(t, st, "%s: no recordingStopped", name)
		assert.Equal(t, events.StopReasonNormal, st.Reason, "%s: stop reason", name)
		newSIDs = append(newSIDs, tr.SID)
		files = append(files, netemFile{name: name, track: tr, stop: st})
	}
	require.Len(t, newSIDs, 2, "new tracks after the reconnect")

	assert.ElementsMatch(t, newSIDs, []string{localTracks["microphone"], localTracks["camera"]}, "the page's tracks after the reconnect")

	return files
}

// checkNetemFile places one file on the browser's capture clock and checks
// it alone, without comparing it to other files. pathDelay is as in
// placeOnCaptureClockCut.
func checkNetemFile(ctx context.Context, t *testing.T, s *Stack, room string, nf netemFile, profile string,
	pathDelay time.Duration,
) capturePlacement {
	t.Helper()

	assert.Len(t, sessionFiles(t, s, room, nf.track.SID, nf.track.Rec.SessionID), 1, "%s: files of its session", nf.name)

	p := placeOnCaptureClockCut(ctx, t, s, nf.name, nf.track.Rec, nf.track.SID, nf.cutAfter, pathDelay)
	assert.NoError(t, p.Out.WebM.CheckMonotonic(), "%s", nf.name)

	if p.kind() == corpus.KindVideo && profile == bbbDefaultCameraProfile {
		t.Logf("%s: %d blocks repeat an earlier block's PTS", nf.name, assertRepeatsHoldOneFrame(t, nf.name, p))
		for _, kf := range writtenKeyFrames(p) {
			t.Logf("%s: %s key frame placed %v after mediaStart", nf.name, kf.size, kf.at.Sub(p.Start))
		}
	} else {
		assert.NoError(t, p.Out.WebM.CheckNoDuplicatePTS(), "%s", nf.name)
	}

	assertMediaEnd(t, nf.name, p.Started, nf.stop, p.Out.WebM.PTS, maxStepOf(p.kind()))
	assertTraceEnded(t, nf.name, p.Out)

	return p
}

// netemLeastDelay is the least delay netem's arguments args set: the delay
// less its jitter. With loss and jitter, a camera's Late comes up to about
// 11 ms over it; browserLateMax's margin covers that.
func netemLeastDelay(t *testing.T, args []string) time.Duration {
	t.Helper()

	i := slices.Index(args, "delay")
	if i < 0 {
		return 0
	}
	require.Less(t, i+1, len(args), "netem %v: a delay with no value", args)
	d, err := time.ParseDuration(args[i+1])
	require.NoError(t, err, "netem %v: the delay", args)
	if i+2 < len(args) {
		if jitter, err := time.ParseDuration(args[i+2]); err == nil {
			d -= jitter
		}
	}

	return d
}

// checkCut checks both files across a blackout they last through. The PTS
// step must equal the capture step; the rest shows the fault reached the
// recorder. micNotFlowing is when the microphone's not_flowing came.
func checkCut(t *testing.T, s *Stack, plan netemPlan, bo *netemBlackout, mic followedTrack, micP, camP capturePlacement,
	stopFrom int, micNotFlowing time.Time,
) {
	t.Helper()

	for _, f := range []struct {
		name string
		p    capturePlacement
	}{{"microphone", micP}, {"camera", camP}} {
		st, ok := f.p.stepAcrossCut()
		require.True(t, ok, "%s: no block on one side of the blackout", f.name)

		inside := 0
		for i := range f.p.Blocks {
			if c := f.p.captured(i); c.After(bo.On.UpEnd) && c.Before(bo.Off.UpStart) {
				inside++
			}
		}
		t.Logf("%s: across the blackout, PTS step %v, capture step %v (%v over the shortest blackout), "+
			"sequence jump %d, timestamp jump %v; %d blocks captured inside it",
			f.name, st.PTS, st.Capture, st.Capture-bo.dmin(), st.SeqJump, st.TSJump, inside)

		assert.LessOrEqual(t, (st.PTS - st.Capture).Abs(), browserDriftBound,
			"%s: the PTS step across the blackout against the capture step", f.name)
		last := f.p.Blocks[len(f.p.Blocks)-1].PTS
		assert.GreaterOrEqual(t, time.Duration(last-f.p.Blocks[st.After].PTS)*time.Millisecond, brwNetemPostCut,
			"%s: media after the blackout", f.name)

		// Nothing the browser sent while its link was dark reached the
		// recorder. The check goes by capture time, not read time. A packet
		// in flight at the blackout's start, or read late by a busy read
		// loop, was captured before it.
		tr := f.p.Out.Trace
		var reads []string
		for _, r := range tr.Records {
			at := tr.Header.Time(r.AtUs)
			if r.T != corpus.RecordRTP || r.Pad || !at.After(bo.On.UpEnd) || !at.Before(bo.Off.UpStart) {
				continue
			}
			if c, ok := f.p.Clock.atIn(r.TS, f.p.Clock.segment(r.AtUs)); ok && c.After(bo.On.UpEnd.Add(reportError(tr.Header.Kind))) {
				reads = append(reads, fmt.Sprintf("read at %v, captured at %v", at.Sub(bo.On.UpEnd), c.Sub(bo.On.UpEnd)))
			}
		}

		if plan.Dry {
			assert.NotEmpty(t, reads, "%s: no media captured in a blackout that changed nothing was read in it", f.name)
			if f.name == "microphone" {
				assert.LessOrEqual(t, st.PTS, brwNetemNoCutMicStep, "microphone: a PTS step with no blackout")
				assert.Less(t, st.Capture, bo.dmin()-brwNetemMicCutSlack, "microphone: a capture step with no blackout")
			}

			continue
		}

		assert.Empty(t, reads, "%s: media captured while the browser's link was dark was read in the blackout", f.name)
		if f.name == "microphone" {
			assert.GreaterOrEqual(t, st.Capture, bo.dmin()-brwNetemMicCutSlack,
				"microphone: capture step across the blackout (%d blocks captured inside it)", inside)
		}
	}

	if plan.Dry {
		for _, m := range s.Driver.Messages()[mic.Rec.From:stopFrom] {
			assert.False(t, rtpStatusMatch(mic.Rec.SessionID, "not_flowing")(m), "microphone: not_flowing with no blackout")
		}

		return
	}

	var lastRead time.Time
	for _, r := range micP.Out.Trace.Records {
		if at := micP.Out.Trace.Header.Time(r.AtUs); r.T == corpus.RecordRTP && !r.Pad && at.Before(micNotFlowing) {
			lastRead = at
		}
	}
	t.Logf("microphone: not_flowing %v after its last read, which came %v after the link went dark",
		micNotFlowing.Sub(lastRead), lastRead.Sub(bo.On.UpStart))
	within := (&config.Config{}).GetDefaults().LiveKit.PacketReadTimeout + brwNetemNotFlowingSlack
	assert.LessOrEqual(t, micNotFlowing.Sub(lastRead), within,
		"microphone: not_flowing after its last read in the blackout, at most %v", within)
}

// checkImpairedStart checks that the loss and the delay reached the media
// path from the start.
func checkImpairedStart(t *testing.T, mic capturePlacement, cam map[string]SenderStat, drops map[string]int) {
	t.Helper()

	for dev, n := range drops {
		assert.Positive(t, n, "netem: %s dropped nothing", dev)
	}

	var nacks, rtx int
	for rid, st := range cam {
		t.Logf("camera %q: %+v", rid, st)
		nacks += st.NACKCount
		rtx += st.RetransmittedPacketsSent
	}
	assert.Positive(t, nacks, "camera: Chromium got no NACK")
	assert.Positive(t, rtx, "camera: Chromium retransmitted nothing")

	firstRead := mic.Blocks[0].Frame.Read
	var all, early []time.Duration
	for i, b := range mic.Blocks {
		d := mic.Out.Trace.Header.Time(b.Frame.Read).Sub(mic.captured(i))
		all = append(all, d)
		if b.Frame.Read-firstRead < time.Second.Microseconds() {
			early = append(early, d)
		}
	}

	for _, set := range []struct {
		name string
		d    []time.Duration
	}{{"the file", all}, {"its first second", early}} {
		ds := slices.Clone(set.d)
		slices.Sort(ds)
		median, p10, p90 := ds[len(ds)/2], ds[len(ds)/10], ds[len(ds)*9/10]
		t.Logf("microphone, %s: read %v after capture (median), p10 %v, p90 %v, %d frames", set.name, median, p10, p90, len(ds))
		assert.GreaterOrEqual(t, median, brwNetemMinDelay, "microphone, %s: median delay from capture to read", set.name)
		assert.GreaterOrEqual(t, p90-p10, brwNetemDelaySpread, "microphone, %s: spread of the delay", set.name)
	}
}

// checkOldFilesEnd checks that the old files of a full reconnect hold no
// media the browser sent after its link went dark.
//
// When LiveKit closes the participant, it sends the old microphone's
// downtrack a few Opus silence frames, timestamped by the wall clock since
// the last frame. The recorder writes them, so the file ends at the close,
// with the blackout as a gap before them. They are left out here.
func checkOldFilesEnd(t *testing.T, bo *netemBlackout, mic, cam capturePlacement) {
	t.Helper()

	for _, f := range []struct {
		name string
		p    capturePlacement
	}{{"old microphone", mic}, {"old camera", cam}} {
		last := len(f.p.Blocks) - 1
		if f.name == "old microphone" {
			for last > 0 && isOpusSilence(f.p.Blocks[last].Frame.Data) {
				last--
			}
			if n := len(f.p.Blocks) - 1 - last; n > 0 {
				t.Logf("%s: ends with %d of LiveKit's silence frames, the first placed %v after the link went dark",
					f.name, n, f.p.placed(f.p.Blocks[last+1]).Sub(bo.On.UpEnd))
			} else {
				t.Logf("%s: ends with none of LiveKit's silence frames", f.name)
			}
		}

		captured := f.p.captured(last)
		t.Logf("%s: the last media block was captured %v after the link went dark", f.name, captured.Sub(bo.On.UpEnd))
		assert.False(t, captured.After(bo.On.UpEnd.Add(reportError(f.p.Out.Trace.Header.Kind))),
			"%s: a block captured in the blackout", f.name)
	}
}

// checkICEDisconnected checks that livekit-server's ICE went disconnected
// for the browser's publisher PC in the blackout, and did not fail. The
// subscriber PC's last packet can come seconds before the blackout, so its
// states are only logged.
func checkICEDisconnected(t *testing.T, s *Stack, bo *netemBlackout) {
	t.Helper()

	disconnected := false
	for _, e := range iceStates(t, s.LiveKit.Proc.LogPath, brwNetemIdentity) {
		if e.Transport != "PUBLISHER" {
			t.Logf("livekit: %s ICE %s at %v", e.Transport, e.State, e.at().Sub(bo.On.UpStart))

			continue
		}

		assert.NotEqual(t, "failed", e.State, "livekit: publisher ICE failed at %v", e.at().Sub(bo.On.UpStart))
		disconnected = disconnected || e.State == "disconnected" &&
			!e.at().Before(bo.On.UpStart.Add(brwNetemICEDisconnected-brwNetemLastPacket)) &&
			!e.at().After(bo.Off.UpEnd.Add(2*time.Second))
	}
	assert.True(t, disconnected, "livekit: the browser's publisher ICE did not go disconnected in the blackout")
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}

	return a
}

func sinceOrZero(at, from time.Time) time.Duration {
	if at.IsZero() {
		return 0
	}

	return at.Sub(from)
}

type followedTrack struct {
	SID, Source string // Source as LiveKit lists it, e.g. "CAMERA"
	ListedAt    time.Time
	VanishedAt  time.Time // zero while listed
	Rec         *Recording
	// StopSentAt is when the follower sent a stop on the unlisting; zero if
	// the recorder had stopped the session already.
	StopSentAt time.Time
}

// trackFollower records each track identity publishes in room, as
// bbb-webrtc-sfu does on track_published. It stops the session when
// LiveKit no longer lists the track, as on track_unpublished. It polls
// LiveKit's listing over the test's link, which no fault reaches.
type trackFollower struct {
	s              *Stack
	room, identity string
	cancel         context.CancelFunc
	done           chan struct{}
	once           sync.Once

	mu     sync.Mutex
	tracks []*followedTrack
	errs   []string
}

func followTracks(t *testing.T, s *Stack, room, identity string) *trackFollower {
	ctx, cancel := context.WithCancel(t.Context())
	f := &trackFollower{s: s, room: room, identity: identity, cancel: cancel, done: make(chan struct{})}
	go f.run(ctx)
	t.Cleanup(f.stop)

	return f
}

func (f *trackFollower) run(ctx context.Context) {
	defer close(f.done)

	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()

	for {
		f.poll(ctx)

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (f *trackFollower) poll(ctx context.Context) {
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	listed, err := f.s.LiveKit.listedTracks(callCtx, f.room, f.identity)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			f.fail("list participants: %v", err)
		}

		return
	}

	now := time.Now()

	f.mu.Lock()
	known := slices.Clone(f.tracks)
	f.mu.Unlock()

	for _, tr := range known {
		if _, ok := listed[tr.SID]; ok || !tr.VanishedAt.IsZero() {
			continue
		}

		f.mu.Lock()
		tr.VanishedAt = now
		rec := tr.Rec
		f.mu.Unlock()

		if rec == nil || slices.ContainsFunc(f.s.Driver.Messages()[rec.From:], stoppedMatch(rec.SessionID)) {
			continue
		}
		if err := f.s.Driver.Stop(rec.SessionID); err != nil {
			f.fail("stop %s: %v", tr.SID, err)
		}

		f.mu.Lock()
		tr.StopSentAt = time.Now()
		f.mu.Unlock()
	}

	sids := make([]string, 0, len(listed))
	for sid := range listed {
		if !slices.ContainsFunc(known, func(tr *followedTrack) bool { return tr.SID == sid }) {
			sids = append(sids, sid)
		}
	}
	slices.Sort(sids)

	for _, sid := range sids {
		tr := &followedTrack{SID: sid, Source: listed[sid].Source.String(), ListedAt: now}
		f.mu.Lock()
		f.tracks = append(f.tracks, tr)
		f.mu.Unlock()

		rec, err := f.s.Driver.StartRecording(ctx, f.room, sid)
		if err != nil {
			if ctx.Err() == nil {
				f.fail("record %s: %v", sid, err)
			}

			continue
		}

		f.mu.Lock()
		tr.Rec = rec
		f.mu.Unlock()
	}
}

func (f *trackFollower) fail(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.errs = append(f.errs, fmt.Sprintf("%s: %s", time.Now().Format(time.RFC3339Nano), fmt.Sprintf(format, args...)))
}

func (f *trackFollower) stop() {
	f.once.Do(func() {
		f.cancel()
		<-f.done
	})
}

func (f *trackFollower) snapshot() []followedTrack {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]followedTrack, 0, len(f.tracks))
	for _, tr := range f.tracks {
		out = append(out, *tr)
	}

	return out
}

func (f *trackFollower) errors() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.errs)
}

func (f *trackFollower) waitRecorded(ctx context.Context, t *testing.T, what string, match func(followedTrack) bool) followedTrack {
	t.Helper()

	for {
		for _, tr := range f.snapshot() {
			if tr.Rec != nil && match(tr) {
				return tr
			}
		}

		select {
		case <-ctx.Done():
			require.FailNow(t, "the follower did not record the "+what, "tracks: %+v; errors: %v", f.snapshot(), f.errors())
		case <-time.After(50 * time.Millisecond):
		}
	}
}
