//go:build e2e && linux

// Package e2e runs the recorder binary against a real livekit-server and
// redis-server on this host, driving it over Redis as bbb-webrtc-sfu does
// and publishing generated media with a Go LiveKit participant.
//
// Environment:
//
//	LIVEKIT_SERVER_BIN  livekit-server binary (default: livekit-server in PATH)
//	REDIS_SERVER_BIN    redis-server binary (default: redis-server in PATH)
//	REDIS_ADDR          use this Redis instead of starting one (e.g. a CI service)
//	E2E_RECORDER_BIN    use this recorder binary instead of building one
//	E2E_RECORDER_RACE   build the recorder with -race when set to 1; a
//	                    race in its log fails the test
//	E2E_ARTIFACTS       artifact root, always kept (default: a new
//	                    $TMPDIR/bwr-e2e-* directory per run, kept only for
//	                    failed tests unless E2E_KEEP_ARTIFACTS=1)
//	E2E_RECORDER_LOG_LEVEL, E2E_LIVEKIT_LOG_LEVEL  (default: debug, info)
package e2e

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// recorderBin is the recorder binary every test starts, set by TestMain.
var recorderBin string

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	// Children a test's cleanup failed to stop. A panic or a -timeout ends
	// the binary before this runs; Pdeathsig kills the children then.
	defer killAllProcs()

	bin, cleanup, err := buildRecorder()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		return 1
	}
	defer cleanup()

	recorderBin = bin

	code := m.Run()
	removeEmptyRunDir()

	return code
}

// buildRecorder builds ./cmd/bbb-webrtc-recorder from the module root into a
// temporary directory, unless E2E_RECORDER_BIN names a binary.
func buildRecorder() (string, func(), error) {
	if bin := os.Getenv("E2E_RECORDER_BIN"); bin != "" {
		abs, err := filepath.Abs(bin)
		if err != nil {
			return "", nil, err
		}

		return abs, func() {}, nil
	}

	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", nil, fmt.Errorf("go is not on PATH; set E2E_RECORDER_BIN to a built recorder: %w", err)
	}

	root, err := moduleRoot(goBin)
	if err != nil {
		return "", nil, err
	}

	removeStaleBinDirs()

	dir, err := os.MkdirTemp("", fmt.Sprintf("%s%d-", binDirPrefix, os.Getpid()))
	if err != nil {
		return "", nil, err
	}

	cleanup := func() { _ = os.RemoveAll(dir) }
	bin := filepath.Join(dir, "bbb-webrtc-recorder")

	args := []string{"build", "-o", bin}
	if os.Getenv("E2E_RECORDER_RACE") == "1" {
		args = append(args, "-race")
	}
	args = append(args, "./cmd/bbb-webrtc-recorder")

	cmd := exec.Command(goBin, args...)
	cmd.Dir = root
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("build recorder: %w", err)
	}

	return bin, cleanup, nil
}

// binDirPrefix names a run's build directory, followed by its PID and a
// random suffix.
const binDirPrefix = "bwr-e2e-bin-"

// removeStaleBinDirs removes the build directories of runs that died
// without cleaning up, as a test timeout does.
func removeStaleBinDirs() {
	dirs, _ := filepath.Glob(filepath.Join(os.TempDir(), binDirPrefix+"*"))

	for _, d := range dirs {
		pidField, _, _ := strings.Cut(strings.TrimPrefix(filepath.Base(d), binDirPrefix), "-")

		pid, err := strconv.Atoi(pidField)
		if err != nil || !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			continue
		}

		_ = os.RemoveAll(d)
	}
}

func moduleRoot(goBin string) (string, error) {
	out, err := exec.Command(goBin, "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}

	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		return "", fmt.Errorf("not inside a Go module")
	}

	return filepath.Dir(gomod), nil
}
