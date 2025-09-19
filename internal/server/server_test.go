package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/appstats"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/types"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/interfaces"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/utils"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
)

// Mock PubSub
type mockPubSub struct {
	publishChan chan []byte
}

func (p *mockPubSub) Publish(channel string, msg []byte) error {
	p.publishChan <- msg
	return nil
}
func (p *mockPubSub) Subscribe(channel string, handler pubsub.PubSubHandler, onStart func() error) error {
	return nil
}
func (p *mockPubSub) OnStart() error { return nil }
func (p *mockPubSub) OnStop()        {}
func (p *mockPubSub) Check() error   { return nil }
func (p *mockPubSub) Close() error   { return nil }

var _ pubsub.PubSub = (*mockPubSub)(nil)

// Mock Recorder
type mockRecorder struct{}

func (r *mockRecorder) GetFilePath() string             { return "/tmp/rec.webm" }
func (r *mockRecorder) PushVideo(p *rtp.Packet)         {}
func (r *mockRecorder) PushAudio(p *rtp.Packet)         {}
func (r *mockRecorder) Close() time.Duration            { return 0 }
func (r *mockRecorder) SetHasAudio(hasAudio bool)       {}
func (r *mockRecorder) GetHasAudio() bool               { return false }
func (r *mockRecorder) SetHasVideo(hasVideo bool)       {}
func (r *mockRecorder) GetHasVideo() bool               { return false }
func (r *mockRecorder) WithContext(ctx context.Context) {}
func (r *mockRecorder) GetStats() *types.RecorderStats  { return nil }
func (r *mockRecorder) SetKeyframeRequester(requester recorder.KeyframeRequester) {
}
func (r *mockRecorder) VideoTimestamp() time.Duration  { return 0 }
func (r *mockRecorder) AudioTimestamp() time.Duration  { return 0 }
func (r *mockRecorder) NotifySkippedPacket(seq uint16) {}

var _ recorder.Recorder = (*mockRecorder)(nil)

// Mock LiveKitWebRTC
type mockLiveKitWebRTC struct {
	closeOnce sync.Once
	closed    chan struct{}
}

func (lk *mockLiveKitWebRTC) SetConnectionStateCallback(callback func(state utils.ConnectionState)) {
}
func (lk *mockLiveKitWebRTC) SetFlowCallback(callback func(isFlowing bool, timestamp time.Duration, closed bool)) {
}
func (lk *mockLiveKitWebRTC) Init() error { return nil }
func (lk *mockLiveKitWebRTC) Close() time.Duration {
	lk.closeOnce.Do(func() {
		close(lk.closed)
	})
	return 0
}
func (lk *mockLiveKitWebRTC) GetStats() *appstats.CaptureStats { return nil }
func (lk *mockLiveKitWebRTC) RequestKeyframe()                 {}
func (lk *mockLiveKitWebRTC) RequestKeyframeForSSRC(ssrc uint32) {
}
func (lk *mockLiveKitWebRTC) HasTrack(trackID string) bool { return true }

var _ interfaces.LiveKitWebRTCInterface = (*mockLiveKitWebRTC)(nil)

func TestDoubleStopSession(t *testing.T) {
	cfg := &config.Config{
		LiveKit: config.LiveKit{
			Host:      "ws://localhost:7880",
			APIKey:    "key",
			APISecret: "secret",
		},
	}
	ps := &mockPubSub{publishChan: make(chan []byte, 10)}
	server := NewServer(cfg, ps)
	sessionID := "test-double-stop"

	startEvent := &events.StartRecording{
		SessionId: sessionID,
		Adapter:   "livekit",
		AdapterOptions: &events.AdapterOptions{
			LiveKit: &events.LiveKitConfig{
				Room:     "test-room",
				TrackIDs: []string{"track1"},
			},
		},
	}

	// 1. Create a session and start it
	rec := &mockRecorder{}
	lk := &mockLiveKitWebRTC{closed: make(chan struct{})}
	sess := NewSession(sessionID, server, (*webrtc.WebRTC)(nil), lk, rec)
	server.sessions.Store(sessionID, sess)
	server.shutdownWg.Add(1)
	go sess.Run(&server.shutdownWg)
	err := sess.StartRecording(startEvent)
	assert.NoError(t, err)

	// 2. Stop the session for the first time
	stopEvent1 := &events.StopRecording{SessionId: sessionID}
	err1 := sess.StopRecording(stopEvent1, events.StopReasonNormal)

	assert.NoError(t, err1, "First stop should not produce an error")

	// Wait for the session's Run goroutine to fully terminate
	<-lk.closed

	// 3. Stop the session for the second time and assert it doesn't panic
	t.Run("double_stop_no_panic", func(t *testing.T) {
		stopEvent2 := &events.StopRecording{SessionId: sessionID}
		var err2 error

		defer func() {
			if r := recover(); r != nil {
				t.Errorf("The code panicked on second stop: %v", r)
			}
		}()

		err2 = sess.StopRecording(stopEvent2, events.StopReasonNormal)
		assert.NoError(t, err2, "Second stop should not produce an error")
	})

	server.shutdownWg.Wait()
}

func TestDuplicateStartSession(t *testing.T) {
	cfg := &config.Config{}
	ps := &mockPubSub{publishChan: make(chan []byte, 10)}
	server := NewServer(cfg, ps)
	ctx := context.Background()
	sessionID := "test-duplicate-start"

	existingSess := &Session{id: sessionID, commands: make(chan interface{}, 1)}
	server.sessions.Store(sessionID, existingSess)

	startEventJSON := []byte(fmt.Sprintf(`{
		"id": "startRecording",
		"recordingSessionId": "%s"
	}`, sessionID))

	// Send a start request for the already-existing session
	server.HandlePubSub(ctx, startEventJSON)

	// We expect a "startRecordingResponse" message with a "failed" status
	select {
	case responseBytes := <-ps.publishChan:
		var response events.StartRecordingResponse
		err := json.Unmarshal(responseBytes, &response)
		assert.NoError(t, err)

		assert.Equal(t, "startRecordingResponse", response.Id)
		assert.Equal(t, sessionID, response.SessionId)
		assert.Equal(t, "failed", response.Status)
		assert.NotNil(t, response.Error)
		assert.Contains(t, *response.Error, "session test-duplicate-start already exists")
	case <-time.After(1 * time.Second):
		t.Fatal("Did not receive a response from the server")
	}
}
