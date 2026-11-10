//go:build e2e && linux

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/stretchr/testify/require"
)

// Browser mode: see "Browser mode" in README.md.

// browserDir holds the runner and its npm package, relative to the package
// directory that go test runs in.
const browserDir = "browser"

// browserCallTimeout bounds one runner call. Launching the browser and
// joining a room take the longest, a few seconds.
const browserCallTimeout = 30 * time.Second

// browserSkipReason says why the browser scenarios cannot run here, or
// returns "".
func browserSkipReason() string {
	const setup = "Browser scenarios need E2E_BROWSER=1, Node 22.22 or later, and " +
		"`npm ci && npx playwright install --no-shell chromium` in tests/e2e/browser."

	if os.Getenv("E2E_BROWSER") != "1" {
		return setup
	}

	if _, err := exec.LookPath("node"); err != nil {
		return "node is not on PATH. " + setup
	}

	if _, err := os.Stat(filepath.Join(browserDir, "node_modules", "playwright")); err != nil {
		return "tests/e2e/browser/node_modules has no playwright. " + setup
	}

	return ""
}

// RequireBrowser skips t unless the browser scenarios can run here. With
// E2E_BROWSER_REQUIRED=1, as CI sets, it fails t instead, so that a broken
// setup does not pass as skipped.
func RequireBrowser(t *testing.T) {
	t.Helper()

	if reason := browserSkipReason(); reason != "" {
		if os.Getenv("E2E_BROWSER_REQUIRED") == "1" {
			require.FailNow(t, reason)
		}

		t.Skip(reason)
	}
}

// Browser is one browser process, driven through the runner.
type Browser struct {
	// Version is the browser's version, as Playwright reports it.
	Version string

	t     *testing.T
	lk    *LiveKit
	url   string // where the page dials LiveKit
	proc  *Proc
	stdin io.WriteCloser
	// rpcLog keeps every request and reply, for a failed test's artifacts.
	rpcLog *os.File

	mu      sync.Mutex
	nextID  int
	pending map[int]chan browserReply
}

type browserReply struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

// NewBrowser starts a browser for the stack's LiveKit and closes it when the
// test ends. It runs Playwright's Chromium, or the Chrome binary that
// E2E_BROWSER_BIN names.
func (s *Stack) NewBrowser() *Browser {
	t := s.T
	t.Helper()

	if reason := browserSkipReason(); reason != "" {
		require.FailNow(t, "NewBrowser without RequireBrowser: "+reason)
	}

	s.browsers++
	name := fmt.Sprintf("browser-%d", s.browsers)

	runner, err := filepath.Abs(filepath.Join(browserDir, "runner.mjs"))
	require.NoError(t, err)

	rpcLog, err := os.Create(filepath.Join(s.Artifacts, name+"-rpc.ndjson"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rpcLog.Close() })

	// A pipe of our own rather than StdoutPipe: Wait closes that one as the
	// runner exits, which can lose the last replies.
	replies, w, err := os.Pipe()
	require.NoError(t, err)

	// An absolute path, for a wrapper that runs node in another namespace.
	node, err := exec.LookPath("node")
	require.NoError(t, err)
	node, err = filepath.Abs(node)
	require.NoError(t, err)

	cmd := exec.Command(s.browserNet.command(s.Artifacts, name, node), runner)
	cmd.Dir = s.Artifacts
	cmd.Env = childEnv()
	cmd.Stdout = w
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)

	url := s.LiveKit.URL
	if s.browserNet != nil && s.browserNet.Host != "" {
		url = fmt.Sprintf("ws://%s:%d", s.browserNet.Host, s.LiveKit.HTTPPort)
	}

	b := &Browser{t: t, lk: s.LiveKit, url: url, stdin: stdin, rpcLog: rpcLog, pending: map[int]chan browserReply{}}
	b.proc = startCmd(t, name, filepath.Join(s.Artifacts, name+".log"), cmd)
	_ = w.Close()

	go b.readReplies(replies)

	// Registered after startCmd's cleanup, so it runs first: the runner
	// closes the browser before the process group is stopped.
	t.Cleanup(b.close)

	var launched struct{ Version string }
	b.call("launch", map[string]any{"executablePath": os.Getenv("E2E_BROWSER_BIN")}, &launched)
	b.Version = launched.Version
	t.Logf("%s: %s", name, b.Version)

	return b
}

func (b *Browser) readReplies(r io.ReadCloser) {
	defer func() { _ = r.Close() }()

	lines := bufio.NewScanner(r)
	lines.Buffer(make([]byte, 64*1024), 16*1024*1024)

	for lines.Scan() {
		b.logRPC("<", lines.Bytes())

		var rep browserReply
		if err := json.Unmarshal(lines.Bytes(), &rep); err != nil {
			continue
		}

		b.mu.Lock()
		ch := b.pending[rep.ID]
		delete(b.pending, rep.ID)
		b.mu.Unlock()

		if ch != nil {
			ch <- rep
		}
	}
}

func (b *Browser) logRPC(dir string, line []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()

	_, _ = fmt.Fprintf(b.rpcLog, "%s %s %s\n", time.Now().Format(time.RFC3339Nano), dir, line)
}

// call runs method in the runner and decodes its result into result, which
// may be nil. It fails the test on an error.
func (b *Browser) call(method string, params, result any) {
	b.t.Helper()

	ctx, cancel := context.WithTimeout(b.t.Context(), browserCallTimeout)
	defer cancel()

	require.NoError(b.t, b.do(ctx, method, params, result), "browser %s; see %s", method, b.proc.LogPath)
}

func (b *Browser) do(ctx context.Context, method string, params, result any) error {
	ch := make(chan browserReply, 1)

	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.pending[id] = ch
	b.mu.Unlock()

	req, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return err
	}

	b.logRPC(">", req)

	if _, err := b.stdin.Write(append(req, '\n')); err != nil {
		return fmt.Errorf("write to the runner: %w", err)
	}

	select {
	case rep := <-ch:
		if rep.Error != "" {
			return errors.New(rep.Error)
		}

		if result == nil {
			return nil
		}

		return json.Unmarshal(rep.Result, result)
	case <-b.proc.Done():
		_, err := b.proc.Exited()

		return fmt.Errorf("the runner exited (%v)", err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// close asks the runner to close the browser. The test's context is done by
// the time cleanups run, so the call has its own.
func (b *Browser) close() {
	ctx, cancel := context.WithTimeout(context.Background(), browserCallTimeout)
	defer cancel()

	if err := b.do(ctx, "close", nil, nil); err != nil {
		b.t.Logf("browser close: %v", err)
	}
}

// BrowserPeer is one participant: a page of its own in the browser, joined
// to a room.
type BrowserPeer struct {
	Identity string

	b    *Browser
	room string
}

// Join opens a page for identity and connects it to room, with
// livekit-client's default room options.
func (b *Browser) Join(room, identity string) *BrowserPeer {
	b.t.Helper()

	return b.join(room, identity, nil)
}

// bbbRoomOptions are the room options BBB's client connects with
// (settings.yml, public.media.livekit.roomOptions).
var bbbRoomOptions = map[string]any{
	"adaptiveStream":            true,
	"dynacast":                  true,
	"stopLocalTrackOnUnpublish": false,
	"singlePeerConnection":      false,
}

// JoinAsBBB is Join with the room options of BBB's client. With dynacast,
// the page sends only the camera layers LiveKit asks for.
func (b *Browser) JoinAsBBB(room, identity string) *BrowserPeer {
	b.t.Helper()

	return b.join(room, identity, bbbRoomOptions)
}

func (b *Browser) join(room, identity string, roomOptions map[string]any) *BrowserPeer {
	b.t.Helper()

	b.call("join", map[string]any{
		"peer": identity, "url": b.url, "token": b.lk.Token(b.t, room, identity, nil), "roomOptions": roomOptions,
	}, nil)

	return &BrowserPeer{Identity: identity, b: b, room: room}
}

// BrowserTrack is a track a BrowserPeer published.
type BrowserTrack struct {
	SID string
}

// PublishBBBMic publishes the browser's fake microphone as BBB's client does
// (imports/api/audio/client/bridge/livekit.ts), and waits until LiveKit
// lists the track. It has no DTX, RED on and 48 kbps.
//
// The page publishes a track of its own capture, which livekit-client
// treats as user-provided: a mute only disables it. The page also runs
// BBB's timer, which unpublishes the microphone after 5 s muted.
func (p *BrowserPeer) PublishBBBMic(name string) *BrowserTrack {
	p.b.t.Helper()

	var published struct{ TrackSid string }
	p.b.call("publishBBBMic", map[string]any{"peer": p.Identity, "name": name}, &published)
	p.waitListed(published.TrackSid, true)

	return &BrowserTrack{SID: published.TrackSid}
}

// MuteMic mutes the microphone of PublishBBBMic as BBB's client does, with
// setMicrophoneEnabled(false). It starts BBB's unpublish timer.
func (p *BrowserPeer) MuteMic() {
	p.b.t.Helper()

	p.b.call("muteBBBMic", map[string]any{"peer": p.Identity}, nil)
}

// bbbLiveKitRoom turns on LiveKit's remote unmute, as BBB's livekit.yaml
// does.
func bbbLiveKitRoom(cfg map[string]any) {
	cfg["room"] = map[string]any{"enable_remote_unmute": true}
}

// UnmuteMic unmutes the microphone of PublishBBBMic as BBB does with its
// default settings (useLiveKitAudioState off). bbb-webrtc-sfu unmutes a
// published microphone with LiveKit's MutePublishedTrack, and the page
// unmutes it in place whatever the state of its capture. With none
// published, BBB's LiveKit audio bridge publishes the capture again, or a
// new one if it has ended. UnmuteMic then waits until LiveKit lists the
// new track.
//
// MutePublishedTrack can unmute only with LiveKit's remote unmute on
// (room.enable_remote_unmute).
func (p *BrowserPeer) UnmuteMic() (tr *BrowserTrack, republished bool) {
	p.b.t.Helper()

	ctx, cancel := context.WithTimeout(p.b.t.Context(), browserCallTimeout)
	defer cancel()

	// As bbb-webrtc-sfu does on MuteUserInVoiceConfSysMsg.
	rs := lksdk.NewRoomServiceClient(p.b.lk.HTTPURL(), p.b.lk.APIKey, p.b.lk.APISecret)
	info, err := rs.GetParticipant(ctx, &livekit.RoomParticipantIdentity{Room: p.room, Identity: p.Identity})
	require.NoError(p.b.t, err)
	remoteUnmuted := []string{}
	for _, ti := range info.Tracks {
		if ti.Source != livekit.TrackSource_MICROPHONE || !ti.Muted {
			continue
		}
		_, err := rs.MutePublishedTrack(ctx, &livekit.MuteRoomTrackRequest{
			Room: p.room, Identity: p.Identity, TrackSid: ti.Sid, Muted: false,
		})
		require.NoError(p.b.t, err)
		remoteUnmuted = append(remoteUnmuted, ti.Sid)
	}

	var unmuted struct {
		TrackSid    string
		Republished bool
	}
	p.b.call("unmuteBBBMic", map[string]any{"peer": p.Identity, "remoteUnmuted": remoteUnmuted}, &unmuted)

	if unmuted.Republished {
		p.waitListed(unmuted.TrackSid, true)
	}

	return &BrowserTrack{SID: unmuted.TrackSid}, unmuted.Republished
}

// waitListed waits until LiveKit lists the track sid as the peer's, or with
// listed false, until it does not.
func (p *BrowserPeer) waitListed(sid string, listed bool) {
	p.b.t.Helper()

	ctx, cancel := context.WithTimeout(p.b.t.Context(), browserCallTimeout)
	defer cancel()

	require.NoError(p.b.t, waitTracks(ctx, p.b.lk, p.room, p.Identity, listed, []string{sid}))
}

// CameraProfile is one of BBB's camera profiles (settings.yml,
// public.kurento.cameraProfiles).
type CameraProfile struct {
	ID          string         `json:"id"`
	Bitrate     int            `json:"bitrate"` // kbps
	Hidden      bool           `json:"hidden,omitempty"`
	Constraints map[string]any `json:"constraints,omitempty"`
}

// bbbCameraProfiles are profiles of settings.yml. The camera preview does
// not list a hidden one (low-u30).
var bbbCameraProfiles = []CameraProfile{
	{ID: "low-u30", Bitrate: 30, Hidden: true},
	{ID: "low", Bitrate: 100},
	{ID: "medium", Bitrate: 200},
	{ID: "high", Bitrate: 500, Constraints: map[string]any{"width": 1280, "height": 720, "frameRate": 15}},
	{ID: "hd", Bitrate: 800, Constraints: map[string]any{"width": 1280, "height": 720, "frameRate": 30}},
}

// bbbDefaultCameraProfile is the profile settings.yml marks as the default.
const bbbDefaultCameraProfile = "medium"

// BBBCamera is a camera published as BBB's client publishes it.
type BBBCamera struct {
	BrowserTrack
	// Capture is the size and frame rate Chromium captures at.
	Capture struct {
		Width, Height int
		FrameRate     float64
	}
	// Encodings are the sender's, one per layer.
	Encodings []struct {
		RID          string
		MaxBitrate   int // bps
		MaxFramerate float64
	}
}

// CameraCapture overrides the camera's capture. The zero value keeps the
// profile's.
type CameraCapture struct {
	// FrameRate, if set, is asked of getUserMedia only, as a webcam in low
	// light captures fewer frames than its profile asks.
	FrameRate float64
	// Preview, if set, holds the capture this long before publishing, as
	// BBB's preview modal does. It captures when it opens
	// (video-preview/service.js), and the camera bridge publishes on 'Start
	// sharing'.
	Preview time.Duration
}

// PublishBBBCamera is PublishBBBCameraCapturing with the profile's capture.
func (p *BrowserPeer) PublishBBBCamera(name, profile string) *BBBCamera {
	p.b.t.Helper()

	return p.PublishBBBCameraCapturing(name, profile, CameraCapture{})
}

// PublishBBBCameraCapturing publishes the browser's fake camera in VP8 as
// BBB's client does with profile, one of bbbCameraProfiles, and capture.
// Profile and up to two visible profiles below it are its simulcast
// layers, duplicates dropped; a hidden profile is one layer. It waits
// until LiveKit lists the track.
func (p *BrowserPeer) PublishBBBCameraCapturing(name, profile string, capture CameraCapture) *BBBCamera {
	p.b.t.Helper()

	var cam struct {
		TrackSid string
		Capture  struct {
			Width, Height int
			FrameRate     float64
		}
		Encodings []struct {
			RID          string
			MaxBitrate   int
			MaxFramerate float64
		}
	}
	args := map[string]any{"peer": p.Identity, "name": name, "profiles": bbbCameraProfiles, "profileId": profile}
	if capture.FrameRate > 0 {
		args["captureFrameRate"] = capture.FrameRate
	}
	if capture.Preview > 0 {
		args["previewMs"] = capture.Preview.Milliseconds()
	}
	p.b.call("publishBBBCamera", args, &cam)
	p.waitListed(cam.TrackSid, true)

	return &BBBCamera{BrowserTrack: BrowserTrack{SID: cam.TrackSid}, Capture: cam.Capture, Encodings: cam.Encodings}
}

// UnpublishCamera stops the camera sid of PublishBBBCameraCapturing as BBB's
// client does (destroyStream in
// video-provider/livekit-camera-bridge/component.tsx). It stops the
// capture, then unpublishes the track with the room's
// stopLocalTrackOnUnpublish. It waits until LiveKit no longer lists it.
func (p *BrowserPeer) UnpublishCamera(sid string) {
	p.b.t.Helper()

	p.b.call("unpublishBBBCamera", map[string]any{"peer": p.Identity, "trackSid": sid}, nil)
	p.waitListed(sid, false)
}

// SetCameraLayer starts or stops sending the simulcast layer rid of the
// camera sid, through its active flag. To LiveKit, a stopped layer looks
// the same as one Chromium drops on a low bandwidth estimate.
func (p *BrowserPeer) SetCameraLayer(sid, rid string, active bool) {
	p.b.t.Helper()

	p.b.call("setCameraLayer", map[string]any{"peer": p.Identity, "trackSid": sid, "rid": rid, "active": active}, nil)
}

// CameraLayers returns the active flag of each simulcast layer of the
// camera sid, by rid.
func (p *BrowserPeer) CameraLayers(sid string) map[string]bool {
	p.b.t.Helper()

	var layers map[string]bool
	p.b.call("cameraLayers", map[string]any{"peer": p.Identity, "trackSid": sid}, &layers)

	return layers
}

// RoomEvent is a livekit-client event the page saw on its room.
type RoomEvent struct {
	Event   string // livekit-client's RoomEvent value, e.g. "reconnecting"
	At      time.Time
	SID     string // the track's, for a local track event
	Source  string // the track's
	Quality string // for connectionQualityChanged
	Reason  string // livekit-client's DisconnectReason, for disconnected
}

// RoomEvents returns the events the page saw on its room since it joined,
// oldest first.
func (p *BrowserPeer) RoomEvents() []RoomEvent {
	p.b.t.Helper()

	var got struct {
		Events []struct {
			Event, Sid, Source, Quality, Reason string
			AtUnixMs                            int64
		}
	}
	p.b.call("roomEvents", map[string]any{"peer": p.Identity}, &got)

	out := make([]RoomEvent, 0, len(got.Events))
	for _, e := range got.Events {
		out = append(out, RoomEvent{
			Event: e.Event, At: time.UnixMilli(e.AtUnixMs), SID: e.Sid, Source: e.Source, Quality: e.Quality, Reason: e.Reason,
		})
	}

	return out
}

// LocalTracks returns the SID of each track the page publishes, by source
// ("camera", "microphone").
func (p *BrowserPeer) LocalTracks() map[string]string {
	p.b.t.Helper()

	var tracks map[string]string
	p.b.call("localTracks", map[string]any{"peer": p.Identity}, &tracks)

	return tracks
}

// SenderStat is one layer's counters from Chromium's outbound-rtp stats.
type SenderStat struct {
	PacketsSent              int
	NACKCount                int
	RetransmittedPacketsSent int
	PLICount                 int
}

// SenderStats returns the outbound-rtp counters of the track sid, by rid,
// or "" for a track of one layer.
func (p *BrowserPeer) SenderStats(sid string) map[string]SenderStat {
	p.b.t.Helper()

	var stats map[string]SenderStat
	p.b.call("senderStats", map[string]any{"peer": p.Identity, "trackSid": sid}, &stats)

	return stats
}

// CaptureFreeze stops the browser's audio service process for Burst, once
// every Every. Chromium captures audio in that process, so the audio of
// each burst is lost. The encoder's timestamps stay contiguous, so the
// sender's media clock runs slow. It acts on every page of the browser and
// needs a capture running.
type CaptureFreeze struct {
	Burst time.Duration
	Every time.Duration
}

// CaptureFreezeStats reports what a freeze did.
type CaptureFreezeStats struct {
	Stops      int
	FrozenMs   float64
	MaxBurstMs float64
}

// FreezeCapture starts f, until ThawCapture.
func (b *Browser) FreezeCapture(f CaptureFreeze) {
	b.t.Helper()

	require.Less(b.t, f.Burst, f.Every, "a freeze burst shorter than its period")

	var started struct{ Pid int }
	b.call("freezeCapture", map[string]any{"burstMs": f.Burst.Milliseconds(), "everyMs": f.Every.Milliseconds()}, &started)

	// Playwright starts the browser in a session of its own, which the
	// runner's process group does not reach. If the runner exits during a
	// burst, nothing else sends the audio service SIGCONT.
	b.t.Cleanup(func() { _ = syscall.Kill(started.Pid, syscall.SIGCONT) })
}

// ThawCapture ends the freeze and returns what it did. It fails the test if
// the freeze could not signal the audio service.
func (b *Browser) ThawCapture() CaptureFreezeStats {
	b.t.Helper()

	var stats CaptureFreezeStats
	b.call("thawCapture", nil, &stats)

	return stats
}
