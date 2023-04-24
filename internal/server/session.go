package server

import (
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/prometheus"
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
	prometheus.Sessions.Inc()
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
	}, func(isFlowing bool, videoTimestamp time.Duration, closed bool) {
		var message interface{}
		if !closed {
			message = events.NewRecordingRtpStatusChanged(s.id, isFlowing, videoTimestamp/time.Millisecond)
		} else {
			s.server.CloseSession(s.id)
			message = events.NewRecordingStopped(s.id, "closed", videoTimestamp/time.Millisecond)
		}
		s.server.PublishPubSub(message)
	})
	return signal.Encode(answer)
}

func (s *Session) StopRecording() time.Duration {
	if !s.stopped {
		s.stopped = true
		prometheus.Sessions.Dec()
		return s.recorder.Close()
	}
	return 0
}
