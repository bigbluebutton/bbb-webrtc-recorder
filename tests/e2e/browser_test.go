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

	cmd := exec.Command("node", runner)
	cmd.Dir = s.Artifacts
	cmd.Env = childEnv()
	cmd.Stdout = w
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)

	b := &Browser{t: t, lk: s.LiveKit, stdin: stdin, rpcLog: rpcLog, pending: map[int]chan browserReply{}}
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

// Join opens a page for identity and connects it to room.
func (b *Browser) Join(room, identity string) *BrowserPeer {
	b.t.Helper()

	b.call("join", map[string]any{"peer": identity, "url": b.lk.URL, "token": b.lk.Token(b.t, room, identity, nil)}, nil)

	return &BrowserPeer{Identity: identity, b: b, room: room}
}

// MicOptions are a microphone's publish options. The zero value publishes as
// BBB's client does by default: no DTX, RED on.
type MicOptions struct {
	Name       string // default "microphone"
	DTX        bool
	DisableRED bool
}

// BrowserTrack is a track a BrowserPeer published.
type BrowserTrack struct {
	SID string
}

// PublishMic publishes the browser's fake microphone, a tone, and waits
// until LiveKit lists the track, as Publisher.WaitPublished does.
func (p *BrowserPeer) PublishMic(o MicOptions) *BrowserTrack {
	p.b.t.Helper()

	if o.Name == "" {
		o.Name = "microphone"
	}

	var published struct{ TrackSid string }
	p.b.call("publishMic", map[string]any{"peer": p.Identity, "name": o.Name, "dtx": o.DTX, "red": !o.DisableRED}, &published)

	ctx, cancel := context.WithTimeout(p.b.t.Context(), browserCallTimeout)
	defer cancel()

	require.NoError(p.b.t, waitTracksListed(ctx, p.b.lk, p.room, p.Identity, published.TrackSid))

	return &BrowserTrack{SID: published.TrackSid}
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
