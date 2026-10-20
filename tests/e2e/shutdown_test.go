//go:build e2e && linux

package e2e

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestShutdownSignalClosesRecordings stops the recorder with SIGINT and with
// SIGTERM while a session records. systemd stops a service with SIGTERM, so
// both must be a graceful shutdown: the recorder stops every session, closes
// its WebM and trace, and exits 0.
func TestShutdownSignalClosesRecordings(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) { testShutdownSignal(t, sig) })
	}
}

func testShutdownSignal(t *testing.T, sig syscall.Signal) {
	t.Helper()

	// Above the time the recorder needs to stop a session (well under a
	// second here), so only a recorder that ignores the signal is killed.
	const grace = 15 * time.Second

	s := NewStack(t, StackOptions{})
	room := "e2e-shutdown-" + uuid.NewString()[:8]
	pub := s.NewPublisher(room, "e2e-publisher")

	video, err := corpus.GenerateVP8(corpus.VP8Options{FPS: 15, Duration: 30 * time.Second})
	require.NoError(t, err)

	cam := pub.PublishVP8(TrackOptions{Name: "camera", Schedule: video})

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	require.NoError(t, pub.WaitPublished(ctx, cam))

	rec, err := s.Driver.StartRecording(ctx, room, cam.SID)
	require.NoError(t, err)
	_, err = s.Driver.WaitRtpStatus(ctx, rec.From, rec.SessionID, "flowing")
	require.NoError(t, err)
	time.Sleep(3 * time.Second)

	proc := s.Recorder.Proc
	require.NoError(t, proc.Stop(sig, grace), "the recorder did not exit on %s", sig)

	_, waitErr := proc.Exited()
	require.NoError(t, waitErr, "the recorder must exit 0 on %s; see %s", sig, proc.LogPath)

	logs, err := os.ReadFile(proc.LogPath)
	require.NoError(t, err)
	assert.Contains(t, string(logs), "webm writer closed", "the session's WebM was not closed")

	out := ReadOutput(t, rec, cam.SID)
	require.NotNil(t, out.EOF, "the trace has no EOF record: the session did not stop")
	t.Logf("%s: %d frames read, %d written, trace EOF reason %q", sig, out.ReadFrames, out.WebM.Blocks, out.EOF.Reason)
	assert.Positive(t, out.WebM.Blocks)
	assert.Equal(t, out.ReadFrames, out.WebM.Blocks, "every frame the recorder read is written")
}
