package server

import (
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/signal"
	pwebrtc "github.com/pion/webrtc/v3"
	"time"
)

type Session struct {
	id       string
	server   *Server
	webrtc   *webrtc.WebRTC
	recorder recorder.Recorder

	stopped bool
}

func NewSession(id string, s *Server, wrtc *webrtc.WebRTC, recorder recorder.Recorder) *Session {
	return &Session{
		id:       id,
		server:   s,
		webrtc:   wrtc,
		recorder: recorder,
	}
}

func (s *Session) StartRecording(sdp string) string {
	offer := pwebrtc.SessionDescription{}
	signal.Decode(sdp, &offer)

	answer := s.webrtc.Init(offer, s.recorder, func(state pwebrtc.ICEConnectionState) {
		if state > pwebrtc.ICEConnectionStateConnected {
			if !s.stopped {
				ts := s.StopRecording() / time.Millisecond
				s.server.PublishPubSub(events.NewRecordingStopped(s.id, state.String(), ts))
				s.server.CloseSession(s.id)
			}
		}
	})
	return signal.Encode(answer)
}

func (s *Session) StopRecording() time.Duration {
	//s.webrtc.Close()
	if !s.stopped {
		s.stopped = true
		return s.recorder.Close()
	}
	return 0
}
