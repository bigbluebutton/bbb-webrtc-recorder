package server

import (
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/prometheus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/livekit"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/signal"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/utils"
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
	stats    *utils.StatsFileWriter
}

func NewSession(id string, s *Server, wrtc *webrtc.WebRTC, lk *livekit.LiveKitWebRTC, recorder recorder.Recorder) *Session {
	return &Session{
		id:       id,
		server:   s,
		webrtc:   wrtc,
		livekit:  lk,
		recorder: recorder,
		stats:    utils.NewStatsFileWriter(s.cfg.Recorder.Directory),
	}
}

func (s *Session) StartRecording(e *events.StartRecording) (string, error) {
	prometheus.Sessions.Inc()
	// Only initialize WebRTC if we're using mediasoup
	if s.webrtc != nil {
		offer := pwebrtc.SessionDescription{}
		signal.Decode(e.GetSDP(), &offer)
		s.webrtc.SetConnectionStateCallback(func(state pwebrtc.ICEConnectionState) {
			if state > pwebrtc.ICEConnectionStateConnected {
				if !s.stopped {
					ts := s.StopRecording() / time.Millisecond
					s.server.PublishPubSub(events.NewRecordingStopped(s.id, state.String(), ts))
					s.server.CloseSession(s.id)
				}
			}
		})
		s.webrtc.SetFlowCallback(func(isFlowing bool, timestamp time.Duration, closed bool) {
			var message interface{}
			if !closed {
				message = events.NewRecordingRtpStatusChanged(s.id, isFlowing, timestamp/time.Millisecond)
			} else {
				s.server.CloseSession(s.id)
				message = events.NewRecordingStopped(s.id, "closed", timestamp/time.Millisecond)
			}
			s.server.PublishPubSub(message)
		})
		s.webrtc.SetSDPOffer(offer)
		answer, err := s.webrtc.Init()

		if err != nil {
			return "", err
		}

		return signal.Encode(answer), nil
	}

	// For LiveKit, we don't need to return an SDP answer
	if s.livekit != nil {
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

		if err := s.livekit.Init(); err != nil {
			return "", err
		}
	}

	return "", nil
}

func (s *Session) StopRecording() time.Duration {
	if !s.stopped {
		s.stopped = true
		prometheus.Sessions.Dec()

		if s.livekit != nil {
			mediaStats := s.livekit.GetStats()

			stats := &utils.Stats{
				MediaAdapter: mediaStats,
				Timestamp:    time.Now().Unix(),
			}

			if err := s.stats.WriteStats(s.recorder.GetFilePath(), stats); err != nil {
				log.WithError(err).Error("Failed to write recording stats")
			}
		}

		if s.webrtc != nil {
			return s.webrtc.Close()
		}
	}

	return 0
}
