package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/types"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// published drains what ps has published so far, keyed by event id.
func published(t *testing.T, ps *mockPubSub) map[string]map[string]any {
	t.Helper()

	got := map[string]map[string]any{}
	for {
		select {
		case msg := <-ps.publishChan:
			var e map[string]any
			require.NoError(t, json.Unmarshal(msg, &e))
			got[e["id"].(string)] = e
		default:
			return got
		}
	}
}

func startLiveKitSession(t *testing.T, rec *mockRecorder) (*Server, *mockPubSub, *mockLiveKitWebRTC, *Session) {
	t.Helper()

	ps := &mockPubSub{publishChan: make(chan []byte, 16)}
	server := NewServer(&config.Config{}, ps)
	lk := &mockLiveKitWebRTC{closed: make(chan struct{})}
	sess := NewSession("test-media-times", server, (*webrtc.WebRTC)(nil), lk, rec)
	server.sessions.Store(sess.id, sess)
	server.runSession(sess)

	require.NoError(t, sess.StartRecording(&events.StartRecording{SessionId: sess.id, Adapter: "livekit"}, time.Time{}))
	require.Eventually(t, func() bool {
		lk.mu.Lock()
		defer lk.mu.Unlock()

		return lk.mediaStart != nil
	}, time.Second, time.Millisecond)

	return server, ps, lk, sess
}

func TestSessionPublishesTheMediaStart(t *testing.T) {
	server, ps, lk, sess := startLiveKitSession(t, &mockRecorder{})
	start := time.Now().Add(-time.Second)

	lk.mu.Lock()
	mediaStart := lk.mediaStart
	lk.mu.Unlock()
	mediaStart(start)

	// The whole event, so that an extra key fails too.
	assert.Equal(t, map[string]any{
		"id":                 "recordingMediaStarted",
		"recordingSessionId": sess.id,
		"mediaStartUTC":      float64(start.UnixMilli()),
		"mediaStartHR":       float64(events.MonotonicMs(start)),
	}, published(t, ps)["recordingMediaStarted"])

	info := sess.GetRecordingInfo()
	require.NotNil(t, info.MediaStartUTC)
	assert.Equal(t, start.UnixMilli(), *info.MediaStartUTC)
	require.NotNil(t, info.MediaStartHR)
	assert.Equal(t, events.MonotonicMs(start), *info.MediaStartHR)

	require.NoError(t, sess.StopRecording(nil, events.StopReasonNormal, time.Time{}))
	server.shutdownWg.Wait()
}

// The adapter's Close writes the frames still held, so the span is read
// after it, whatever ended the session.
func TestSessionStopCarriesTheMediaSpan(t *testing.T) {
	for _, c := range []struct {
		name   string
		event  func(id string) *events.StopRecording
		reason string
	}{
		{
			name:   "stopRecording",
			event:  func(id string) *events.StopRecording { return &events.StopRecording{SessionId: id} },
			reason: events.StopReasonNormal,
		},
		{name: "track closed", event: func(string) *events.StopRecording { return nil }, reason: "closed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			start := time.Now().Add(-10 * time.Second)
			span := &types.MediaSpan{Start: start, End: start.Add(9 * time.Second), Settled: true}
			rec := &mockRecorder{}
			server, ps, lk, sess := startLiveKitSession(t, rec)
			lk.onClose = func() { rec.span = span }

			require.NoError(t, sess.StopRecording(c.event(sess.id), c.reason, time.Time{}))
			server.shutdownWg.Wait()

			e := published(t, ps)["recordingStopped"]
			require.NotNil(t, e)
			assert.Equal(t, float64(start.UnixMilli()), e["mediaStartUTC"])
			assert.Equal(t, float64(events.MonotonicMs(start)), e["mediaStartHR"])
			assert.Equal(t, float64(span.End.UnixMilli()), e["mediaEndUTC"])
			assert.Equal(t, float64(events.MonotonicMs(span.End)), e["mediaEndHR"])
		})
	}
}

func TestSessionStopWithoutMediaHasNoSpan(t *testing.T) {
	server, ps, _, sess := startLiveKitSession(t, &mockRecorder{})

	require.NoError(t, sess.StopRecording(nil, events.StopReasonNormal, time.Time{}))
	server.shutdownWg.Wait()

	e := published(t, ps)["recordingStopped"]
	require.NotNil(t, e)
	for _, k := range []string{"mediaStartUTC", "mediaStartHR", "mediaEndUTC", "mediaEndHR"} {
		assert.NotContains(t, e, k)
	}
}
