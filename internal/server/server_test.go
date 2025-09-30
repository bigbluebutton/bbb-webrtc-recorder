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
type mockRecorder struct {
	path string
}

func (r *mockRecorder) GetFilePath() string             { return r.path }
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
	err := sess.StartRecording(startEvent, time.Time{})
	assert.NoError(t, err)

	// 2. Stop the session for the first time
	stopEvent1 := &events.StopRecording{SessionId: sessionID}
	err1 := sess.StopRecording(stopEvent1, events.StopReasonNormal, time.Time{})

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

		err2 = sess.StopRecording(stopEvent2, events.StopReasonNormal, time.Time{})
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
	server.HandlePubSubMsg(ctx, startEventJSON)

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

func TestStartRecordingFailureDoesNotPublishStopEvent(t *testing.T) {
	cfg := &config.Config{
		Recorder: config.Recorder{
			// Use a recorder config that will cause recorder.NewRecorder to fail
			Directory: "/this/path/cannot/exist/surely/",
			FileMode:  "0640",
		},
	}
	ps := &mockPubSub{publishChan: make(chan []byte, 10)}
	server := NewServer(cfg, ps)
	ctx := context.Background()
	sessionID := "test-start-fail"
	startEventPayload := &events.StartRecording{
		SessionId: sessionID,
		Adapter:   "livekit",
		FileName:  "test.webm",
	}
	event := &events.Event{
		Id:   "startRecording",
		Data: startEventPayload,
	}
	event.StartRecording()
	server.HandlePubSubEvent(ctx, event)

	// 1. We expect exactly one message: a "startRecordingResponse" with a "failed" status
	select {
	case responseBytes := <-ps.publishChan:
		var response events.StartRecordingResponse
		err := json.Unmarshal(responseBytes, &response)
		assert.NoError(t, err)

		assert.Equal(t, "startRecordingResponse", response.Id)
		assert.Equal(t, sessionID, response.SessionId)
		assert.Equal(t, "failed", response.Status)
		assert.NotNil(t, response.Error)
	case <-time.After(1 * time.Second):
		t.Fatal("Did not receive a response from the server")
	}

	// 2. We expect NO other messages, specifically no "recordingStopped" message.
	select {
	case unexpectedBytes := <-ps.publishChan:
		t.Fatalf("Received an unexpected second message on pubsub channel: %s", string(unexpectedBytes))
	case <-time.After(100 * time.Millisecond):
		// This is the expected outcome: the channel is empty.
	}
}

func TestGetRecordings(t *testing.T) {
	testCases := []struct {
		name       string
		setup      func(server *Server) map[string]*Session
		assertions func(t *testing.T, response *events.GetRecordingsResponse, sessions map[string]*Session)
	}{
		{
			name: "With Active Sessions",
			setup: func(server *Server) map[string]*Session {
				sessions := make(map[string]*Session)

				// LiveKit session
				sessionID1 := "test-session-1"
				startEvent1 := &events.StartRecording{
					SessionId: sessionID1,
					FileName:  "rec1.webm",
					Adapter:   "livekit",
					AdapterOptions: &events.AdapterOptions{
						LiveKit: &events.LiveKitConfig{Room: "room1", TrackIDs: []string{"track1"}},
					},
					Metadata: map[string]any{"participantId": "user1", "source": "camera"},
				}
				sess1 := &Session{
					id:                    sessionID1,
					startEvent:            startEvent1,
					recorder:              &mockRecorder{path: "/var/recordings/rec1.webm"},
					metadata:              startEvent1.Metadata,
					recordingStartTimeUTC: time.Now().Add(time.Millisecond * 50),
					recordingStartTimeHR:  time.Second * 10,
					mediaHasFlowed:        true,
				}
				server.sessions.Store(sessionID1, sess1)
				sessions[sessionID1] = sess1

				// Mediasoup session
				sessionID2 := "test-session-2"
				startEvent2 := &events.StartRecording{
					SessionId: sessionID2,
					FileName:  "rec2.webm",
					Adapter:   "mediasoup",
					AdapterOptions: &events.AdapterOptions{
						Mediasoup: &events.MediasoupConfig{SDP: "v=0..."},
					},
					Metadata: map[string]any{"participantId": "user2", "source": "screen"},
				}
				sess2 := &Session{
					id:                    sessionID2,
					startEvent:            startEvent2,
					recorder:              &mockRecorder{path: "/var/recordings/rec2.webm"},
					metadata:              startEvent2.Metadata,
					recordingStartTimeUTC: time.Now().Add(time.Millisecond * 50),
					recordingStartTimeHR:  time.Second * 20,
					mediaHasFlowed:        true,
				}
				server.sessions.Store(sessionID2, sess2)
				sessions[sessionID2] = sess2

				// Session without media flow
				sessionID3 := "test-session-3"
				startEvent3 := &events.StartRecording{
					SessionId: sessionID3,
					FileName:  "rec3.webm",
					Adapter:   "livekit",
					AdapterOptions: &events.AdapterOptions{
						LiveKit: &events.LiveKitConfig{Room: "room3", TrackIDs: []string{"track3"}},
					},
					Metadata: map[string]interface{}{"participantId": "user3", "source": "camera"},
				}
				sess3 := &Session{
					id:             sessionID3,
					startEvent:     startEvent3,
					recorder:       &mockRecorder{path: "/var/recordings/rec3.webm"},
					metadata:       startEvent3.Metadata,
					mediaHasFlowed: false,
				}
				server.sessions.Store(sessionID3, sess3)
				sessions[sessionID3] = sess3

				// Session with no metadata
				sessionID4 := "test-session-4"
				startEvent4 := &events.StartRecording{
					SessionId: sessionID4,
					FileName:  "rec4.webm",
					Adapter:   "livekit",
					AdapterOptions: &events.AdapterOptions{
						LiveKit: &events.LiveKitConfig{Room: "room4", TrackIDs: []string{"track4"}},
					},
					Metadata: nil, // Explicitly no metadata
				}
				sess4 := &Session{
					id:             sessionID4,
					startEvent:     startEvent4,
					recorder:       &mockRecorder{path: "/var/recordings/rec4.webm"},
					metadata:       startEvent4.Metadata,
					mediaHasFlowed: false,
				}
				server.sessions.Store(sessionID4, sess4)
				sessions[sessionID4] = sess4

				return sessions
			},
			assertions: func(t *testing.T, response *events.GetRecordingsResponse, sessions map[string]*Session) {
				assert.Len(t, response.Recordings, 4)

				recMap := make(map[string]*events.RecordingInfo)
				for _, rec := range response.Recordings {
					recMap[rec.SessionId] = rec
				}

				// Validate LiveKit recording
				sess1 := sessions["test-session-1"]
				rec1, ok1 := recMap[sess1.id]
				assert.True(t, ok1)
				assert.Equal(t, sess1.recorder.GetFilePath(), rec1.FileName)
				assert.Equal(t, events.AdapterLiveKit, rec1.Adapter)
				assert.Equal(t, sess1.startEvent.AdapterOptions.LiveKit.Room, rec1.AdapterOptions.LiveKit.Room)
				assert.Equal(t, sess1.startEvent.AdapterOptions.LiveKit.TrackIDs, rec1.AdapterOptions.LiveKit.TrackIDs)
				assert.Equal(t, sess1.startEvent.Metadata, rec1.Metadata)
				assert.NotNil(t, rec1.StartTimeUTC)
				assert.Equal(t, sess1.recordingStartTimeUTC.UnixMilli(), *rec1.StartTimeUTC)
				assert.NotNil(t, rec1.StartTimeHR)
				assert.Equal(t, sess1.recordingStartTimeHR, *rec1.StartTimeHR)

				// Validate Mediasoup recording
				sess2 := sessions["test-session-2"]
				rec2, ok2 := recMap[sess2.id]
				assert.True(t, ok2)
				assert.Equal(t, sess2.recorder.GetFilePath(), rec2.FileName)
				assert.Equal(t, events.AdapterMediasoup, rec2.Adapter)
				assert.Equal(t, sess2.startEvent.AdapterOptions.Mediasoup.SDP, rec2.AdapterOptions.Mediasoup.SDP)
				assert.Equal(t, sess2.startEvent.Metadata, rec2.Metadata)
				assert.NotNil(t, rec2.StartTimeUTC)
				assert.Equal(t, sess2.recordingStartTimeUTC.UnixMilli(), *rec2.StartTimeUTC)
				assert.NotNil(t, rec2.StartTimeHR)
				assert.Equal(t, sess2.recordingStartTimeHR, *rec2.StartTimeHR)

				// Validate session that request times are undefined when media has not started flowing.
				sess3 := sessions["test-session-3"]
				rec3, ok3 := recMap[sess3.id]
				assert.True(t, ok3)
				assert.Equal(t, sess3.recorder.GetFilePath(), rec3.FileName)
				assert.Equal(t, events.AdapterLiveKit, rec3.Adapter)
				assert.Equal(t, sess3.startEvent.AdapterOptions.LiveKit.Room, rec3.AdapterOptions.LiveKit.Room)
				assert.Equal(t, sess3.startEvent.AdapterOptions.LiveKit.TrackIDs, rec3.AdapterOptions.LiveKit.TrackIDs)
				assert.Equal(t, sess3.startEvent.Metadata, rec3.Metadata)
				assert.Nil(t, rec3.StartTimeUTC)
				assert.Nil(t, rec3.StartTimeHR)

				// Validate session with no metadata
				sess4 := sessions["test-session-4"]
				rec4, ok4 := recMap[sess4.id]
				assert.True(t, ok4)
				assert.Equal(t, sess4.recorder.GetFilePath(), rec4.FileName)
				assert.Equal(t, events.AdapterLiveKit, rec4.Adapter)
				assert.Nil(t, rec4.Metadata)
			},
		},
		{
			name:  "With No Active Sessions",
			setup: func(server *Server) map[string]*Session { return nil },
			assertions: func(t *testing.T, response *events.GetRecordingsResponse, sessions map[string]*Session) {
				assert.NotNil(t, response.Recordings, "Recordings field should be an empty array, not null")
				assert.Len(t, response.Recordings, 0, "Recordings array should be empty")
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			ps := &mockPubSub{publishChan: make(chan []byte, 10)}
			server := NewServer(cfg, ps)
			ctx := context.Background()
			sessions := tc.setup(server)
			requestID := "test-req-" + tc.name

			getRecordingsEvent := &events.Event{
				Id: "getRecordings",
				Data: &events.GetRecordings{
					Id:        "getRecordings",
					RequestId: requestID,
				},
			}
			server.HandlePubSubEvent(ctx, getRecordingsEvent)

			select {
			case responseBytes := <-ps.publishChan:
				var response events.GetRecordingsResponse
				err := json.Unmarshal(responseBytes, &response)
				assert.NoError(t, err)
				assert.Equal(t, "getRecordingsResponse", response.Id)
				assert.Equal(t, requestID, response.RequestId)
				tc.assertions(t, &response, sessions)

			case <-time.After(2 * time.Second):
				t.Fatal("Did not receive a getRecordingsResponse from the server within 2 seconds")
			}
		})
	}
}
