//go:build e2e && linux

package e2e

import (
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	defaultToRecorder   = "to-bbb-webrtc-recorder"
	defaultFromRecorder = "from-bbb-webrtc-recorder"
)

// StackOptions adjusts a stack before its processes start.
type StackOptions struct {
	// LiveKitConfig edits the generated livekit-server config.
	LiveKitConfig func(cfg map[string]any)
	// RecorderConfig edits the generated recorder config, with Redis and
	// LiveKit already up: e.g. to point livekit.host at a proxy.
	RecorderConfig func(s *Stack, cfg map[string]any)
}

// Stack is one test's redis-server, livekit-server, recorder and driver.
type Stack struct {
	T         *testing.T
	Artifacts string
	Redis     *Redis
	LiveKit   *LiveKit
	Driver    *Driver
	Recorder  *Recorder
}

// NewStack starts a stack whose logs, configs and outputs go to the test's
// artifact directory; t.Cleanup stops it.
func NewStack(t *testing.T, o StackOptions) *Stack {
	t.Helper()

	s := &Stack{T: t, Artifacts: ArtifactDir(t)}
	redirectStdLog(t, filepath.Join(s.Artifacts, "lksdk-test-process.log"))

	s.Redis = StartRedis(t, s.Artifacts)
	s.LiveKit = StartLiveKit(t, s.Artifacts, o.LiveKitConfig)

	to, from := defaultToRecorder, defaultFromRecorder
	if s.Redis.Proc == nil {
		// A shared Redis may carry another run's recorder traffic: the run
		// directory (<stamp>-<pid>) and the test's make the channels unique.
		suffix := "-e2e-" + filepath.Base(filepath.Dir(s.Artifacts)) + "-" + filepath.Base(s.Artifacts)
		to, from = to+suffix, from+suffix
	}

	s.Driver = NewDriver(t, s.Redis.Addr, to, from, filepath.Join(s.Artifacts, "pubsub.ndjson"))

	cfg := RecorderConfig(t, s.Artifacts, s.Redis, to, from, s.LiveKit)
	if o.RecorderConfig != nil {
		o.RecorderConfig(s, cfg)
	}

	s.Recorder = StartRecorder(t, s.Artifacts, cfg, s.Driver)

	return s
}

// NewPublisher joins room on the stack's LiveKit.
func (s *Stack) NewPublisher(room, identity string) *Publisher {
	return NewPublisher(s.T, s.LiveKit, s.Artifacts, room, identity)
}

// redirectStdLog sends the standard logger, which lksdk logs to by default,
// to path for the rest of the test.
func redirectStdLog(t *testing.T, path string) {
	f, err := os.Create(path)
	require.NoError(t, err)

	prev := log.Writer()
	log.SetOutput(f)

	t.Cleanup(func() {
		log.SetOutput(prev)
		_ = f.Close()
	})
}
