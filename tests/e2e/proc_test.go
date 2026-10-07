//go:build e2e && linux

package e2e

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Proc is a child process in its own process group, its output in LogPath.
type Proc struct {
	Name    string
	LogPath string
	Cmd     *exec.Cmd

	done    chan struct{}
	waitErr error
	stop    sync.Once
}

var (
	procsMu sync.Mutex
	procs   = map[*Proc]struct{}{}
)

// childEnvDrop lists the variables a child must not inherit: each would
// override the generated config, and could point it at the host's services.
var childEnvDrop = []string{"BBBRECORDER_", "LIVEKIT_", "REDIS_", "NODE_IP=", "UDP_PORT="}

func childEnv(extra ...string) []string {
	var env []string

	for _, kv := range os.Environ() {
		drop := false
		for _, p := range childEnvDrop {
			if strings.HasPrefix(kv, p) {
				drop = true
				break
			}
		}

		if !drop {
			env = append(env, kv)
		}
	}

	return append(env, extra...)
}

// StartProc starts bin with its stdout and stderr in logPath, and stops it
// (SIGTERM, then SIGKILL to the group after grace) when the test ends.
func StartProc(t *testing.T, name, logPath, dir string, env []string, bin string, args ...string) *Proc {
	t.Helper()

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	require.NoError(t, err)

	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Pdeathsig kills the child if the test binary dies without cleaning up.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}

	_, _ = fmt.Fprintf(logFile, "### %s: %s %s\n", time.Now().Format(time.RFC3339Nano), bin, strings.Join(args, " "))

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		require.NoError(t, err, "start %s", name)
	}

	p := &Proc{Name: name, LogPath: logPath, Cmd: cmd, done: make(chan struct{})}

	go func() {
		p.waitErr = cmd.Wait()
		_ = logFile.Close()
		close(p.done)
	}()

	procsMu.Lock()
	procs[p] = struct{}{}
	procsMu.Unlock()

	t.Cleanup(func() {
		if err := p.Stop(syscall.SIGTERM, 10*time.Second); err != nil {
			t.Logf("%s: %v", name, err)
		}
	})
	t.Logf("%s: pid %d, log %s", name, cmd.Process.Pid, logPath)

	return p
}

// Done is closed once the process has exited.
func (p *Proc) Done() <-chan struct{} { return p.done }

// Exited reports whether the process has exited, and its Wait error.
func (p *Proc) Exited() (bool, error) {
	select {
	case <-p.done:
		return true, p.waitErr
	default:
		return false, nil
	}
}

// Stop sends sig to the process group, again after a quarter of grace, then
// kills the group once grace has passed. It reports a forced kill; an exit status after sig is not an error.
func (p *Proc) Stop(sig syscall.Signal, grace time.Duration) error {
	var err error

	p.stop.Do(func() {
		defer func() {
			procsMu.Lock()
			delete(procs, p)
			procsMu.Unlock()
		}()

		pgid := p.Cmd.Process.Pid

		if exited, _ := p.Exited(); !exited {
			_ = syscall.Kill(-pgid, sig)

			select {
			case <-p.done:
			case <-time.After(grace / 4):
				// livekit-server takes a second signal as a forced exit; one
				// received while it starts up can leave it hanging.
				_ = syscall.Kill(-pgid, sig)

				select {
				case <-p.done:
				case <-time.After(grace - grace/4):
					err = fmt.Errorf("still running %s after %s, killed", sig, grace)
				}
			}
		}

		// Whatever the leader left in its group goes too.
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-p.done
	})

	return err
}

func killAllProcs() {
	procsMu.Lock()
	left := make([]*Proc, 0, len(procs))
	for p := range procs {
		left = append(left, p)
	}
	procsMu.Unlock()

	for _, p := range left {
		_ = p.Stop(syscall.SIGKILL, time.Second)
	}
}

var (
	portsMu sync.Mutex
	ports   = map[int]bool{}
)

// FreePort returns a port that is free on every address for network "tcp"
// or "udp", and that no earlier call in this process returned.
func FreePort(t *testing.T, network string) int {
	t.Helper()

	for range 50 {
		var port int

		switch network {
		case "tcp":
			l, err := net.Listen("tcp4", "0.0.0.0:0")
			require.NoError(t, err)
			port = l.Addr().(*net.TCPAddr).Port
			_ = l.Close()
		case "udp":
			c, err := net.ListenPacket("udp4", "0.0.0.0:0")
			require.NoError(t, err)
			port = c.LocalAddr().(*net.UDPAddr).Port
			_ = c.Close()
		default:
			require.FailNow(t, fmt.Sprintf("FreePort: unknown network %q", network))
		}

		portsMu.Lock()
		taken := ports[port]
		ports[port] = true
		portsMu.Unlock()

		if !taken {
			return port
		}
	}

	require.FailNow(t, fmt.Sprintf("FreePort: no fresh %s port", network))

	return 0
}

// StartRetryingPorts calls start, which picks its ports with FreePort and
// starts a process on them, and once more if that process lost a port:
// FreePort's reservation holds only in this process, so another one, or an
// outgoing connection's source port, can take it before the child binds.
func StartRetryingPorts(t *testing.T, start func() (*Proc, error)) {
	t.Helper()

	p, err := start()
	if err != nil && p != nil && lostPort(p) {
		t.Logf("%s: a port was taken before it bound it (%v); retrying on fresh ports", p.Name, err)
		_ = p.Stop(syscall.SIGKILL, time.Second)
		_, err = start()
	}

	require.NoError(t, err)
}

func lostPort(p *Proc) bool {
	data, _ := os.ReadFile(p.LogPath)

	return strings.Contains(strings.ToLower(string(data)), "address already in use")
}

// WaitFor polls cond every 50 ms until it returns nil, the process exits or
// timeout passes, and returns the last error.
func WaitFor(timeout time.Duration, proc *Proc, cond func() error) error {
	deadline := time.Now().Add(timeout)

	for {
		err := cond()
		if err == nil {
			return nil
		}

		if proc != nil {
			if exited, werr := proc.Exited(); exited {
				return fmt.Errorf("%s exited (%v) before it was ready: %w; see %s", proc.Name, werr, err, proc.LogPath)
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("not ready after %s: %w", timeout, err)
		}

		time.Sleep(50 * time.Millisecond)
	}
}

var (
	runDirOnce sync.Once
	runDir     string
	runDirErr  error

	artifactNamesMu sync.Mutex
	artifactNames   = map[string]int{}
)

func keepArtifacts() bool {
	return os.Getenv("E2E_ARTIFACTS") != "" || os.Getenv("E2E_KEEP_ARTIFACTS") == "1"
}

// artifactRunDir returns the run's artifact directory, absolute because each
// child runs in its own artifact directory and is handed paths under it.
func artifactRunDir() (string, error) {
	runDirOnce.Do(func() {
		dir := ""
		if root := os.Getenv("E2E_ARTIFACTS"); root != "" {
			dir = filepath.Join(root, fmt.Sprintf("%s-%d", time.Now().Format("20060102-150405"), os.Getpid()))
			runDirErr = os.MkdirAll(dir, 0o755)
		} else {
			// Private and unpredictable: the children read their configs
			// from it.
			dir, runDirErr = os.MkdirTemp("", "bwr-e2e-")
		}

		if runDirErr == nil {
			runDir, runDirErr = filepath.Abs(dir)
		}
	})

	return runDir, runDirErr
}

func removeEmptyRunDir() {
	if runDir != "" {
		_ = os.Remove(runDir) // only succeeds when empty
	}
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// ArtifactDir returns a directory for t's logs, configs and outputs, one per
// test run (-count=N gives N), removed after a passing test unless kept.
func ArtifactDir(t *testing.T) string {
	t.Helper()

	root, err := artifactRunDir()
	require.NoError(t, err)

	name := unsafeName.ReplaceAllString(t.Name(), "_")

	artifactNamesMu.Lock()
	artifactNames[name]++
	n := artifactNames[name]
	artifactNamesMu.Unlock()

	dir := filepath.Join(root, fmt.Sprintf("%s-%d", name, n))
	require.NoError(t, os.MkdirAll(dir, 0o755))

	// Registered first, so it runs after every process has been stopped.
	t.Cleanup(func() {
		if t.Failed() || keepArtifacts() {
			t.Logf("artifacts: %s", dir)
			return
		}

		_ = os.RemoveAll(dir)
	})

	return dir
}
