//go:build e2e && linux

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Recorder is a running recorder binary.
type Recorder struct {
	Dir        string // recorder.directory
	MetricsURL string
	ConfigPath string
	Proc       *Proc
}

// RecorderConfig returns the generated recorder config for a stack's Redis,
// channels and LiveKit: recordings under dir, Prometheus on a free loopback
// port, and header-mode trace capture. Keys follow internal/config's yaml tags.
func RecorderConfig(t *testing.T, dir string, rd *Redis, toRecorder, fromRecorder string, lk *LiveKit) map[string]any {
	t.Helper()

	return map[string]any{
		"log": map[string]any{"level": envOr("E2E_RECORDER_LOG_LEVEL", "debug")},
		"recorder": map[string]any{
			"directory":      filepath.Join(dir, "recorder"),
			"dirFileMode":    "0700",
			"fileMode":       "0600",
			"writeToDevNull": false,
			"writeStatsFile": true,
		},
		"pubsub": map[string]any{
			"channels": map[string]any{"subscribe": toRecorder, "publish": fromRecorder},
			"adapter":  "redis",
			"adapters": map[string]any{
				"redis": map[string]any{"address": rd.Addr, "network": "tcp"},
			},
		},
		"http": map[string]any{"enable": false},
		"prometheus": map[string]any{
			"enable":        true,
			"listenAddress": fmt.Sprintf("127.0.0.1:%d", FreePort(t, "tcp")),
		},
		"livekit": map[string]any{
			"host":      lk.URL,
			"apiKey":    lk.APIKey,
			"apiSecret": lk.APISecret,
			"traceCapture": map[string]any{
				"enabled": true,
				"payload": "header",
			},
			"healthCheck": map[string]any{"enable": false},
		},
	}
}

// StartRecorder writes cfg (from RecorderConfig, maybe edited) and starts
// the recorder binary on it, then waits for its startup recorderStatus on d
// and for its metrics endpoint.
func StartRecorder(t *testing.T, dir string, cfg map[string]any, d *Driver) *Recorder {
	t.Helper()

	r := &Recorder{
		Dir:        lookup[string](t, cfg, "recorder", "directory"),
		ConfigPath: filepath.Join(dir, "bbb-webrtc-recorder.yml"),
	}
	require.NoError(t, os.MkdirAll(r.Dir, 0o700))

	prom := lookup[map[string]any](t, cfg, "prometheus")
	retry := false

	StartRetryingPorts(t, func() (*Proc, error) {
		if retry {
			host, _, err := net.SplitHostPort(lookup[string](t, cfg, "prometheus", "listenAddress"))
			require.NoError(t, err)
			prom["listenAddress"] = net.JoinHostPort(host, strconv.Itoa(FreePort(t, "tcp")))
		}

		retry = true

		return r.Proc, r.start(t, dir, cfg, d)
	})

	return r
}

func (r *Recorder) start(t *testing.T, dir string, cfg map[string]any, d *Driver) error {
	t.Helper()

	r.MetricsURL = "http://" + lookup[string](t, cfg, "prometheus", "listenAddress") + "/metrics"
	writeYAML(t, r.ConfigPath, cfg)

	from := len(d.Messages())
	logPath := filepath.Join(dir, "bbb-webrtc-recorder.log")
	proc := StartProc(t, "bbb-webrtc-recorder", logPath, dir, childEnv(),
		recorderBin, "--config", r.ConfigPath)
	r.Proc = proc

	// SIGINT is the recorder's graceful shutdown: it stops every session. A
	// race-enabled recorder (E2E_RECORDER_RACE) reports races in its log and
	// still exits, so the log decides.
	t.Cleanup(func() {
		if err := proc.Stop(syscall.SIGINT, 15*time.Second); err != nil {
			t.Logf("recorder: %v", err)
		}

		if out, err := os.ReadFile(logPath); err == nil {
			if n := bytes.Count(out, []byte("WARNING: DATA RACE")); n > 0 {
				t.Errorf("recorder: %d data race reports in %s", n, logPath)
			}
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	statusCh := make(chan error, 1)
	go func() {
		_, err := d.WaitRecorderStatus(ctx, from)
		statusCh <- err
	}()

	select {
	case err := <-statusCh:
		if err != nil {
			return fmt.Errorf("recorder did not announce itself: %w; see %s", err, proc.LogPath)
		}
	case <-proc.Done():
		return fmt.Errorf("recorder exited during startup; see %s", proc.LogPath)
	}

	// The recorder keeps running when its metrics listener fails.
	return WaitFor(10*time.Second, proc, func() error { return httpOK(r.MetricsURL) })
}

func lookup[T any](t *testing.T, cfg map[string]any, path ...string) T {
	t.Helper()

	var v any = cfg
	for _, k := range path {
		m, ok := v.(map[string]any)
		require.True(t, ok, "config: %v is not a map", path)
		v = m[k]
	}

	out, ok := v.(T)
	require.True(t, ok, "config: %v is %T", path, v)

	return out
}
