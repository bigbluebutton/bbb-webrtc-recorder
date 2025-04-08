package server

import (
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/prometheus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/livekit"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/signal"
	pwebrtc "github.com/pion/webrtc/v3"
	log "github.com/sirupsen/logrus"
)

type Session struct {
	id       string
	server   *Server
	webrtc   *webrtc.WebRTC
	livekit  *livekit.LiveKitWebRTC
	recorder recorder.Recorder
	stopped  bool
}

func NewSession(id string, s *Server, wrtc *webrtc.WebRTC, lk *livekit.LiveKitWebRTC, recorder recorder.Recorder) *Session {
	return &Session{
		id:       id,
		server:   s,
		webrtc:   wrtc,
		livekit:  lk,
		recorder: recorder,
	}
}

func (s *Session) StartRecording(e *events.StartRecording) (string, error) {
	prometheus.Sessions.Inc()
	// Only initialize WebRTC if we're using mediasoup
	if s.webrtc != nil {
		offer := pwebrtc.SessionDescription{}
		signal.Decode(e.GetSDP(), &offer)

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
		return signal.Encode(answer), nil
	}

	// For LiveKit, we don't need to return an SDP answer
	if s.livekit != nil {

		if err := s.livekit.Connect(e.AdapterOptions.LiveKit.Room, e.AdapterOptions.LiveKit.TrackIDs); err != nil {
			return "", err
		}

		log.WithField("session", s.id).Info("Setting LiveKit flow callback")
		s.livekit.SetFlowCallback(func(isFlowing bool, timestamp time.Duration, closed bool) {
			var message interface{}
			if !closed {
				message = events.NewRecordingRtpStatusChanged(s.id, isFlowing, timestamp/time.Millisecond)
			} else {
				s.server.CloseSession(s.id)
				message = events.NewRecordingStopped(s.id, "closed", timestamp/time.Millisecond)
			}
			s.server.PublishPubSub(message)
		})

		return "", nil
	}

	return "", nil
}

func (s *Session) StopRecording() time.Duration {
	if !s.stopped {
		s.stopped = true
		prometheus.Sessions.Dec()
		return s.recorder.Close()
	}
	return 0
}
